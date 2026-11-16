// Package clipboard reads/writes the OS clipboard (text) and keeps two
// clipboards in sync over a stream.
//
// Sync is symmetric: the host runs it against the host clipboard and the
// viewer bridge runs it against the viewer's clipboard. Only changes made
// after the session starts are sent, so connecting never leaks what was
// already on either clipboard.
package clipboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	"rmt-desk/internal/proto"
)

// Clipboard is a text clipboard.
type Clipboard interface {
	Read() (string, error)
	Write(text string) error
}

var ErrUnavailable = errors.New("clipboard: not available")

// PollInterval is how often the local clipboard is checked for changes.
var PollInterval = 500 * time.Millisecond

// Sync runs until ctx is done or the stream fails.
func Sync(ctx context.Context, local Clipboard, c net.Conn, logf func(string, ...any)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	context.AfterFunc(ctx, func() { c.Close() })

	var mu sync.Mutex
	var last [32]byte // hash of the content both sides are known to share
	if cur, err := local.Read(); err == nil {
		last = sha256.Sum256([]byte(cur)) // baseline: don't send pre-existing content
	}

	errc := make(chan error, 2)
	// Remote -> local.
	go func() {
		for {
			var m proto.Clipboard
			if err := proto.ReadJSON(c, proto.MsgClipboard, &m); err != nil {
				errc <- err
				return
			}
			if len(m.Text) > proto.MaxClipboard || !utf8.ValidString(m.Text) {
				continue
			}
			h := sha256.Sum256([]byte(m.Text))
			mu.Lock()
			if h != last {
				if err := local.Write(m.Text); err != nil {
					logf("clipboard write: %v", err)
				} else {
					last = h // suppress echo back to the sender
				}
			}
			mu.Unlock()
		}
	}()
	// Local -> remote.
	go func() {
		t := time.NewTicker(PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			case <-t.C:
			}
			cur, err := local.Read()
			if err != nil || cur == "" || len(cur) > proto.MaxClipboard || !utf8.ValidString(cur) {
				continue
			}
			h := sha256.Sum256([]byte(cur))
			mu.Lock()
			changed := h != last
			if changed {
				last = h
			}
			mu.Unlock()
			if changed {
				if err := proto.WriteJSON(c, proto.MsgClipboard, proto.Clipboard{Text: cur}); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	err := <-errc
	cancel()
	return err
}

// Memory is an in-memory Clipboard (tests, headless hosts).
type Memory struct {
	mu   sync.Mutex
	text string
}

func (m *Memory) Read() (string, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.text, nil }
func (m *Memory) Write(s string) error  { m.mu.Lock(); m.text = s; m.mu.Unlock(); return nil }
