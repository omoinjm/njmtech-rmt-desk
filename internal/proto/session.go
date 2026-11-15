package proto

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.LogOutput = io.Discard
	return c
}

// ServerSession wraps an admitted connection on the host side.
func ServerSession(c net.Conn) (*yamux.Session, error) { return yamux.Server(c, yamuxConfig()) }

// ClientSession wraps a connection on the viewer side.
func ClientSession(c net.Conn) (*yamux.Session, error) { return yamux.Client(c, yamuxConfig()) }

// OpenStream opens a typed logical stream.
func OpenStream(s *yamux.Session, kind string) (net.Conn, error) {
	st, err := s.Open()
	if err != nil {
		return nil, err
	}
	if err := WriteJSON(st, MsgOpen, StreamOpen{Kind: kind}); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// AcceptStream accepts the next logical stream and returns its kind.
func AcceptStream(s *yamux.Session) (net.Conn, string, error) {
	st, err := s.Accept()
	if err != nil {
		return nil, "", err
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	var o StreamOpen
	if err := ReadJSON(st, MsgOpen, &o); err != nil {
		st.Close()
		return nil, "", fmt.Errorf("stream open: %w", err)
	}
	_ = st.SetReadDeadline(time.Time{})
	return st, o.Kind, nil
}
