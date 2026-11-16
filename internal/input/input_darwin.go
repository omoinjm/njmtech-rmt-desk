//go:build darwin && cgo

package input

// macOS backend via Quartz CGEvent. Requires the Accessibility permission
// (System Settings → Privacy & Security → Accessibility) for the rmtd binary.

/*
#cgo LDFLAGS: -framework CoreGraphics -framework ApplicationServices
#include <CoreGraphics/CoreGraphics.h>
#include <ApplicationServices/ApplicationServices.h>

static int rmt_trusted(void) { return AXIsProcessTrusted(); }

static int rmt_display_bounds(int idx, double *x, double *y, double *w, double *h) {
	CGDirectDisplayID ids[32];
	uint32_t n = 0;
	if (CGGetActiveDisplayList(32, ids, &n) != kCGErrorSuccess || idx < 0 || (uint32_t)idx >= n) return -1;
	CGRect r = CGDisplayBounds(ids[idx]); // global coords, points, top-left origin
	*x = r.origin.x; *y = r.origin.y; *w = r.size.width; *h = r.size.height;
	return 0;
}

static void rmt_mouse(CGEventType t, double x, double y, CGMouseButton b, int clicks) {
	CGEventRef e = CGEventCreateMouseEvent(NULL, t, CGPointMake(x, y), b);
	if (clicks > 0) CGEventSetIntegerValueField(e, kCGMouseEventClickState, clicks);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void rmt_scroll(int dx, int dy) {
	CGEventRef e = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitLine, 2, -dy, -dx);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void rmt_key(CGKeyCode k, int down, CGEventFlags flags) {
	CGEventRef e = CGEventCreateKeyboardEvent(NULL, k, down ? true : false);
	CGEventSetFlags(e, flags);
	CGEventPost(kCGHIDEventTap, e);
	CFRelease(e);
}

static void rmt_cursor(double *x, double *y) {
	CGEventRef e = CGEventCreate(NULL);
	CGPoint p = CGEventGetLocation(e);
	CFRelease(e);
	*x = p.x; *y = p.y;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"time"
)

type mac struct {
	held   map[Button]bool
	flags  C.CGEventFlags
	lastDn time.Time
	lastB  Button
	clicks int
}

func New() (Injector, error) {
	if C.rmt_trusted() == 0 {
		return nil, errors.New("input: Accessibility permission not granted to rmtd (System Settings → Privacy & Security → Accessibility)")
	}
	return &mac{held: map[Button]bool{}}, nil
}

func (m *mac) MoveTo(display int, nx, ny float64) error {
	var x, y, w, h C.double
	if C.rmt_display_bounds(C.int(display), &x, &y, &w, &h) != 0 {
		return fmt.Errorf("input: no display %d", display)
	}
	px, py := float64(x)+nx*float64(w-1), float64(y)+ny*float64(h-1)
	t := C.CGEventType(C.kCGEventMouseMoved)
	b := C.CGMouseButton(C.kCGMouseButtonLeft)
	switch {
	case m.held[Left]:
		t = C.kCGEventLeftMouseDragged
	case m.held[Right]:
		t, b = C.kCGEventRightMouseDragged, C.kCGMouseButtonRight
	case m.held[Middle]:
		t, b = C.kCGEventOtherMouseDragged, C.kCGMouseButtonCenter
	}
	C.rmt_mouse(t, C.double(px), C.double(py), b, 0)
	return nil
}

func (m *mac) Button(b Button, down bool) error {
	var x, y C.double
	C.rmt_cursor(&x, &y)
	var t C.CGEventType
	var cb C.CGMouseButton
	switch b {
	case Left:
		t, cb = map[bool]C.CGEventType{true: C.kCGEventLeftMouseDown, false: C.kCGEventLeftMouseUp}[down], C.kCGMouseButtonLeft
	case Right:
		t, cb = map[bool]C.CGEventType{true: C.kCGEventRightMouseDown, false: C.kCGEventRightMouseUp}[down], C.kCGMouseButtonRight
	default:
		t, cb = map[bool]C.CGEventType{true: C.kCGEventOtherMouseDown, false: C.kCGEventOtherMouseUp}[down], C.kCGMouseButtonCenter
	}
	// macOS needs an explicit click count for double-click to register.
	if down {
		if b == m.lastB && time.Since(m.lastDn) < 400*time.Millisecond {
			m.clicks++
		} else {
			m.clicks = 1
		}
		m.lastB, m.lastDn = b, time.Now()
	}
	m.held[b] = down
	C.rmt_mouse(t, x, y, cb, C.int(m.clicks))
	return nil
}

func (m *mac) Scroll(dx, dy int) error {
	C.rmt_scroll(C.int(dx), C.int(dy))
	return nil
}

var modFlags = map[uint16]C.CGEventFlags{
	0x38: C.kCGEventFlagMaskShift, 0x3C: C.kCGEventFlagMaskShift,
	0x3B: C.kCGEventFlagMaskControl, 0x3E: C.kCGEventFlagMaskControl,
	0x3A: C.kCGEventFlagMaskAlternate, 0x3D: C.kCGEventFlagMaskAlternate,
	0x37: C.kCGEventFlagMaskCommand, 0x36: C.kCGEventFlagMaskCommand,
}

func (m *mac) Key(k Key, down bool) error {
	// Synthetic events don't update modifier state, so track it ourselves.
	if f, ok := modFlags[k.Mac]; ok {
		if down {
			m.flags |= f
		} else {
			m.flags &^= f
		}
	}
	C.rmt_key(C.CGKeyCode(k.Mac), boolInt(down), m.flags)
	return nil
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func (m *mac) Close() error { return nil }
