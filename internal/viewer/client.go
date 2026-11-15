// Package viewer implements the client side of an rmt session.
package viewer

import (
	"context"
	"fmt"
	"net"
	"time"

	"rmt-desk/internal/proto"

	"github.com/hashicorp/yamux"
)

const ClientName = "rmt/0.1.0"

type Session struct {
	Info proto.HelloAck
	Mux  *yamux.Session
	conn net.Conn
}

// Dialer abstracts tsnet.Server.Dial so tests can use plain TCP.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Connect dials addr, performs the handshake, and returns a multiplexed session.
func Connect(ctx context.Context, dial Dialer, addr, password string) (*Session, error) {
	c, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if err := proto.WriteJSON(c, proto.MsgHello, proto.Hello{
		Version: proto.Version, Client: ClientName, Password: password,
	}); err != nil {
		c.Close()
		return nil, err
	}
	var ack proto.HelloAck
	if err := proto.ReadJSON(c, proto.MsgHelloAck, &ack); err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	_ = c.SetDeadline(time.Time{})
	mux, err := proto.ClientSession(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &Session{Info: ack, Mux: mux, conn: c}, nil
}

func (s *Session) Open(kind string) (net.Conn, error) { return proto.OpenStream(s.Mux, kind) }

func (s *Session) Close() error { return s.Mux.Close() }
