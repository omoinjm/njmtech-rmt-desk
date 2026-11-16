//go:build linux

package clipboard

// X11 CLIPBOARD selection in pure Go (xgb). Works on Xorg and XWayland
// (including WSLg, which bridges to the Windows clipboard).
//
// X11 has no clipboard *storage*: the owning client must answer every paste
// request. So Write takes ownership of CLIPBOARD and an event loop serves
// SelectionRequest events until another client takes ownership.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// maxX11 keeps us below the core protocol request limit (no INCR support).
const maxX11 = 256 << 10

type x11 struct {
	c   *xgb.Conn
	win xproto.Window

	clipboard, utf8, targets, prop, incr xproto.Atom

	mu     sync.Mutex
	owned  bool
	data   []byte
	notify chan xproto.SelectionNotifyEvent
	readMu sync.Mutex // one ConvertSelection in flight
}

func New() (Clipboard, error) {
	if os.Getenv("DISPLAY") == "" {
		return nil, fmt.Errorf("%w: DISPLAY not set", ErrUnavailable)
	}
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	screen := xproto.Setup(c).DefaultScreen(c)
	win, err := xproto.NewWindowId(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := xproto.CreateWindowChecked(c, 0, win, screen.Root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		c.Close()
		return nil, err
	}
	x := &x11{c: c, win: win, notify: make(chan xproto.SelectionNotifyEvent, 1)}
	for name, dst := range map[string]*xproto.Atom{
		"CLIPBOARD": &x.clipboard, "UTF8_STRING": &x.utf8, "TARGETS": &x.targets,
		"RMT_DESK_SEL": &x.prop, "INCR": &x.incr,
	} {
		r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
		if err != nil {
			c.Close()
			return nil, err
		}
		*dst = r.Atom
	}
	go x.loop()
	return x, nil
}

func (x *x11) loop() {
	for {
		ev, err := x.c.WaitForEvent()
		if ev == nil && err == nil {
			return // connection closed
		}
		switch e := ev.(type) {
		case xproto.SelectionRequestEvent:
			x.serve(e)
		case xproto.SelectionClearEvent:
			x.mu.Lock()
			x.owned, x.data = false, nil // someone else copied something
			x.mu.Unlock()
		case xproto.SelectionNotifyEvent:
			select {
			case x.notify <- e:
			default:
			}
		}
	}
}

// serve answers another application's paste request.
func (x *x11) serve(e xproto.SelectionRequestEvent) {
	x.mu.Lock()
	data, owned := x.data, x.owned
	x.mu.Unlock()

	prop := e.Property
	if prop == xproto.AtomNone {
		prop = e.Target // obsolete clients
	}
	switch {
	case !owned || e.Selection != x.clipboard:
		prop = xproto.AtomNone
	case e.Target == x.targets:
		atoms := []xproto.Atom{x.targets, x.utf8, xproto.AtomString}
		buf := make([]byte, 4*len(atoms))
		for i, a := range atoms {
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(a))
		}
		xproto.ChangeProperty(x.c, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(atoms)), buf)
	case e.Target == x.utf8 || e.Target == xproto.AtomString:
		xproto.ChangeProperty(x.c, xproto.PropModeReplace, e.Requestor, prop, e.Target, 8, uint32(len(data)), data)
	default:
		prop = xproto.AtomNone // unsupported target
	}
	ev := xproto.SelectionNotifyEvent{Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop}
	xproto.SendEvent(x.c, false, e.Requestor, xproto.EventMaskNoEvent, string(ev.Bytes()))
}

func (x *x11) Read() (string, error) {
	x.mu.Lock()
	if x.owned {
		d := string(x.data)
		x.mu.Unlock()
		return d, nil
	}
	x.mu.Unlock()

	x.readMu.Lock()
	defer x.readMu.Unlock()
	owner, err := xproto.GetSelectionOwner(x.c, x.clipboard).Reply()
	if err != nil {
		return "", err
	}
	if owner.Owner == xproto.WindowNone {
		return "", nil
	}
	// Drain a stale notification, then ask the owner to convert.
	select {
	case <-x.notify:
	default:
	}
	xproto.ConvertSelection(x.c, x.win, x.clipboard, x.utf8, x.prop, xproto.TimeCurrentTime)
	var n xproto.SelectionNotifyEvent
	select {
	case n = <-x.notify:
	case <-time.After(time.Second):
		return "", errors.New("clipboard: owner did not respond")
	}
	if n.Property == xproto.AtomNone {
		return "", nil // owner can't provide text (e.g. an image)
	}
	r, err := xproto.GetProperty(x.c, true, x.win, x.prop, xproto.GetPropertyTypeAny, 0, maxX11/4).Reply()
	if err != nil {
		return "", err
	}
	if r.Type == x.incr || r.BytesAfter > 0 {
		return "", errors.New("clipboard: content too large")
	}
	return string(r.Value), nil
}

func (x *x11) Write(text string) error {
	if len(text) > maxX11 {
		return errors.New("clipboard: text too large for X11 selection")
	}
	x.mu.Lock()
	x.data, x.owned = []byte(text), true
	x.mu.Unlock()
	if err := xproto.SetSelectionOwnerChecked(x.c, x.win, x.clipboard, xproto.TimeCurrentTime).Check(); err != nil {
		return err
	}
	r, err := xproto.GetSelectionOwner(x.c, x.clipboard).Reply()
	if err != nil || r.Owner != x.win {
		x.mu.Lock()
		x.owned = false
		x.mu.Unlock()
		return errors.New("clipboard: could not take ownership")
	}
	return nil
}
