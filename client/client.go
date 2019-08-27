package client

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"

	"github.com/canonical/go-dqlite/internal/protocol"
	"github.com/pkg/errors"
)

// DialFunc is a function that can be used to establish a network connection.
type DialFunc protocol.DialFunc

// Client speaks the dqlite wire protocol.
type Client struct {
	protocol *protocol.Protocol
}

// Option that can be used to tweak client parameters.
type Option func(*options)

type options struct {
	DialFunc DialFunc
}

// WithServerDialFunc sets a custom dial function for creating the client
// network connection.
func WithDialFunc(dial DialFunc) Option {
	return func(options *options) {
		options.DialFunc = dial
	}
}

// New creates a new client connected to the dqlite node with the given
// address.
func New(ctx context.Context, address string, options ...Option) (*Client, error) {
	o := defaultOptions()

	for _, option := range options {
		option(o)
	}
	// Establish the connection.
	conn, err := o.DialFunc(ctx, address)
	if err != nil {
		return nil, errors.Wrap(err, "failed to establish network connection")
	}

	// Latest protocol version.
	proto := make([]byte, 8)
	binary.LittleEndian.PutUint64(proto, protocol.VersionLegacy)

	// Perform the protocol handshake.
	n, err := conn.Write(proto)
	if err != nil {
		conn.Close()
		return nil, errors.Wrap(err, "failed to send handshake")
	}
	if n != 8 {
		conn.Close()
		return nil, errors.Wrap(io.ErrShortWrite, "failed to send handshake")
	}

	client := &Client{protocol: protocol.NewProtocol(protocol.VersionLegacy, conn)}

	return client, nil
}

// File holds the content of a single database file.
type File struct {
	Name string
	Data []byte
}

// Dump the content of the database with the given name. Two files will be
// returned, the first is the main database file (which has the same name as
// the database), the second is the WAL file (which has the same name as the
// database plus the suffix "-wal").
func (c *Client) Dump(ctx context.Context, dbname string) ([]File, error) {
	request := protocol.Message{}
	request.Init(16)
	response := protocol.Message{}
	response.Init(512)

	protocol.EncodeDump(&request, dbname)

	if err := c.protocol.Call(ctx, &request, &response); err != nil {
		return nil, errors.Wrap(err, "failed to send dump request")
	}

	files, err := protocol.DecodeFiles(&response)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse files response")
	}
	defer files.Close()

	dump := make([]File, 0)

	for {
		name, data := files.Next()
		if name == "" {
			break
		}
		dump = append(dump, File{Name: name, Data: data})
	}

	return dump, nil
}

// Create a client options object with sane defaults.
func defaultOptions() *options {
	return &options{
		DialFunc: defaultDialFunc,
	}
}

func defaultDialFunc(ctx context.Context, address string) (net.Conn, error) {
	if strings.HasPrefix(address, "@") {
		return protocol.UnixDial(ctx, address)
	}
	return protocol.TCPDial(ctx, address)
}
