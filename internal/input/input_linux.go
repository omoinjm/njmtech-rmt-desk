//go:build linux

package input

// X11 backend via the XTEST extension (pure Go, no cgo). Works on Xorg and on
// XWayland for X11 clients. Native Wayland injection (uinput / RemoteDesktop
// portal) is phase 5.

import (
	"fmt"
	"os"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"

	"rmt-desk/internal/capture"
)

const (
	xKeyPress      = 2
	xKeyRelease    = 3
	xButtonPress   = 4
	xButtonRelease = 5
	xMotionNotify  = 6
)

type x11 struct {
	c    *xgb.Conn
	root xproto.Window
}

func New() (Injector, error) {
	if os.Getenv("DISPLAY") == "" {
		return nil, fmt.Errorf("input: DISPLAY not set (X11 required; Wayland-native input not yet supported)")
	}
	c, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("input: connect X11: %w", err)
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, fmt.Errorf("input: XTEST unavailable: %w", err)
	}
	return &x11{c: c, root: xproto.Setup(c).DefaultScreen(c).Root}, nil
}

func (x *x11) fake(typ, detail byte, rx, ry int16) error {
	return xtest.FakeInputChecked(x.c, typ, detail, 0, x.root, rx, ry, 0).Check()
}

func (x *x11) MoveTo(display int, nx, ny float64) error {
	// Root-window coordinates, exactly what the capturer uses (kbinani's
	// bounds are relative to the primary monitor, which XTEST would misplace).
	bs, err := capture.RootBounds(x.c)
	if err != nil {
		return err
	}
	if display < 0 || display >= len(bs) {
		return fmt.Errorf("input: no display %d", display)
	}
	b := bs[display]
	px := b.Min.X + int(nx*float64(b.Dx()-1)+0.5)
	py := b.Min.Y + int(ny*float64(b.Dy()-1)+0.5)
	return x.fake(xMotionNotify, 0, int16(px), int16(py))
}

func (x *x11) Button(b Button, down bool) error {
	btn := map[Button]byte{Left: 1, Middle: 2, Right: 3}[b]
	t := byte(xButtonRelease)
	if down {
		t = xButtonPress
	}
	return x.fake(t, btn, 0, 0)
}

func (x *x11) click(btn byte, n int) error {
	for range n {
		if err := x.fake(xButtonPress, btn, 0, 0); err != nil {
			return err
		}
		if err := x.fake(xButtonRelease, btn, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

func (x *x11) Scroll(dx, dy int) error {
	// X11 wheel buttons: 4 up, 5 down, 6 left, 7 right.
	if dy > 0 {
		if err := x.click(5, dy); err != nil {
			return err
		}
	} else if dy < 0 {
		if err := x.click(4, -dy); err != nil {
			return err
		}
	}
	if dx > 0 {
		return x.click(7, dx)
	} else if dx < 0 {
		return x.click(6, -dx)
	}
	return nil
}

func (x *x11) Key(k Key, down bool) error {
	// evdev/libinput X servers: keycode = evdev code + 8.
	t := byte(xKeyRelease)
	if down {
		t = xKeyPress
	}
	return x.fake(t, byte(k.Evdev+8), 0, 0)
}

func (x *x11) Close() error { x.c.Close(); return nil }
