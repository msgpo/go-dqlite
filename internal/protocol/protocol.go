package protocol

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// Protocol sends and receive the dqlite message on the wire.
type Protocol struct {
	version        uint64        // Protocol version
	conn           net.Conn      // Underlying network connection.
	contextTimeout time.Duration // Default context timeout.
	closeCh        chan struct{} // Stops the heartbeat when the connection gets closed
	mu             sync.Mutex    // Serialize requests
	netErr         error         // A network error occurred
}

func NewProtocol(version uint64, conn net.Conn) *Protocol {
	protocol := &Protocol{
		version:        version,
		conn:           conn,
		closeCh:        make(chan struct{}),
		contextTimeout: 5 * time.Second,
	}

	return protocol
}

// SetContextTimeout sets the default context timeout when no deadline is
// provided.
func (p *Protocol) SetContextTimeout(timeout time.Duration) {
	p.contextTimeout = timeout
}

// Call invokes a dqlite RPC, sending a request message and receiving a
// response message.
func (p *Protocol) Call(ctx context.Context, request, response *Message) (err error) {
	// We need to take a lock since the dqlite server currently does not
	// support concurrent requests.
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.netErr != nil {
		return p.netErr
	}

	// Honor the ctx deadline, if present, or use a default.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(p.contextTimeout)
	}

	p.conn.SetDeadline(deadline)

	if err = p.send(request); err != nil {
		err = errors.Wrap(err, "failed to send request")
		goto err
	}

	if err = p.recv(response); err != nil {
		err = errors.Wrap(err, "failed to receive response")
		goto err
	}

	return

err:
	switch errors.Cause(err).(type) {
	case *net.OpError:
		p.netErr = err
	}
	return
}

// More is used when a request maps to multiple responses.
func (p *Protocol) More(ctx context.Context, response *Message) error {
	return p.recv(response)
}

// Interrupt sends an interrupt request and awaits for the server's empty
// response.
func (p *Protocol) Interrupt(ctx context.Context, request *Message, response *Message) error {
	// We need to take a lock since the dqlite server currently does not
	// support concurrent requests.
	p.mu.Lock()
	defer p.mu.Unlock()

	// Honor the ctx deadline, if present, or use a default.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	p.conn.SetDeadline(deadline)

	defer request.Reset()

	EncodeInterrupt(request, 0)

	if err := p.send(request); err != nil {
		return errors.Wrap(err, "failed to send interrupt request")
	}

	for {
		if err := p.recv(response); err != nil {
			response.Reset()
			return errors.Wrap(err, "failed to receive response")
		}

		mtype, _ := response.getHeader()
		response.Reset()

		if mtype == ResponseEmpty {
			break
		}
	}

	return nil
}

// Close the client connection.
func (p *Protocol) Close() error {
	close(p.closeCh)
	return p.conn.Close()
}

func (p *Protocol) send(req *Message) error {
	if err := p.sendHeader(req); err != nil {
		return errors.Wrap(err, "failed to send header")
	}

	if err := p.sendBody(req); err != nil {
		return errors.Wrap(err, "failed to send body")
	}

	return nil
}

func (p *Protocol) sendHeader(req *Message) error {
	n, err := p.conn.Write(req.header[:])
	if err != nil {
		return errors.Wrap(err, "failed to send header")
	}

	if n != messageHeaderSize {
		return errors.Wrap(io.ErrShortWrite, "failed to send header")
	}

	return nil
}

func (p *Protocol) sendBody(req *Message) error {
	buf := req.body1.Bytes[:req.body1.Offset]
	n, err := p.conn.Write(buf)
	if err != nil {
		return errors.Wrap(err, "failed to send static body")
	}

	if n != len(buf) {
		return errors.Wrap(io.ErrShortWrite, "failed to write body")
	}

	if req.body2.Bytes == nil {
		return nil
	}

	buf = req.body2.Bytes[:req.body2.Offset]
	n, err = p.conn.Write(buf)
	if err != nil {
		return errors.Wrap(err, "failed to send dynamic body")
	}

	if n != len(buf) {
		return errors.Wrap(io.ErrShortWrite, "failed to write body")
	}

	return nil
}

func (p *Protocol) recv(res *Message) error {
	if err := p.recvHeader(res); err != nil {
		return errors.Wrap(err, "failed to receive header")
	}

	if err := p.recvBody(res); err != nil {
		return errors.Wrap(err, "failed to receive body")
	}

	return nil
}

func (p *Protocol) recvHeader(res *Message) error {
	if err := p.recvPeek(res.header); err != nil {
		return errors.Wrap(err, "failed to receive header")
	}

	res.words = binary.LittleEndian.Uint32(res.header[0:])
	res.mtype = res.header[4]
	res.flags = res.header[5]
	res.extra = binary.LittleEndian.Uint16(res.header[6:])

	return nil
}

func (p *Protocol) recvBody(res *Message) error {
	n := int(res.words) * messageWordSize
	n1 := n
	n2 := 0

	if n1 > len(res.body1.Bytes) {
		// We need to allocate the dynamic buffer.
		n1 = len(res.body1.Bytes)
		n2 = n - n1
	}

	buf := res.body1.Bytes[:n1]

	if err := p.recvPeek(buf); err != nil {
		return errors.Wrap(err, "failed to read body")
	}

	if n2 > 0 {
		res.body2.Bytes = make([]byte, n2)
		res.body2.Offset = 0
		buf = res.body2.Bytes
		if err := p.recvPeek(buf); err != nil {
			return errors.Wrap(err, "failed to read body")
		}
	}

	return nil
}

// Read until buf is full.
func (p *Protocol) recvPeek(buf []byte) error {
	for offset := 0; offset < len(buf); {
		n, err := p.recvFill(buf[offset:])
		if err != nil {
			return err
		}
		offset += n
	}

	return nil
}

// Try to fill buf, but perform at most one read.
func (p *Protocol) recvFill(buf []byte) (int, error) {
	// Read new data: try a limited number of times.
	//
	// This technique is copied from bufio.Reader.
	for i := messageMaxConsecutiveEmptyReads; i > 0; i-- {
		n, err := p.conn.Read(buf)
		if n < 0 {
			panic(errNegativeRead)
		}
		if err != nil {
			return -1, err
		}
		if n > 0 {
			return n, nil
		}
	}
	return -1, io.ErrNoProgress
}

/*
func (p *Protocol) heartbeat() {
	request := Message{}
	request.Init(16)
	response := Message{}
	response.Init(512)

	for {
		delay := c.heartbeatTimeout / 3

		//c.logger.Debug("sending heartbeat", zap.Duration("delay", delay))
		time.Sleep(delay)

		// Check if we've been closed.
		select {
		case <-c.closeCh:
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)

		EncodeHeartbeat(&request, uint64(time.Now().Unix()))

		err := c.Call(ctx, &request, &response)

		// We bail out upon failures.
		//
		// TODO: make the client survive temporary disconnections.
		if err != nil {
			cancel()
			//c.logger.Error("heartbeat failed", zap.Error(err))
			return
		}

		//addresses, err := DecodeServers(&response)
		_, err = DecodeServers(&response)
		if err != nil {
			cancel()
			//c.logger.Error("invalid heartbeat response", zap.Error(err))
			return
		}

		// if err := c.store.Set(ctx, addresses); err != nil {
		// 	cancel()
		// 	c.logger.Error("failed to update servers", zap.Error(err))
		// 	return
		// }

		cancel()

		request.Reset()
		response.Reset()
	}
}
*/
