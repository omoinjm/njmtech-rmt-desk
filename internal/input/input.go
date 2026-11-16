// Package input injects remote mouse and keyboard events on the host.
//
// Pointer positions arrive normalized to [0,1] within a display, so each OS
// backend maps them into its own coordinate space (physical pixels on
// Windows/X11, points on macOS) without DPI/Retina mismatches.
//
// Keys arrive as browser KeyboardEvent.code values (physical key positions),
// so the host's keyboard layout determines the resulting character, the
// same behaviour as AnyDesk/RDP "scancode" mode.
package input

import (
	"errors"
	"fmt"
	"math"
)

// Event is the wire format for one input action (JSON on the input stream).
type Event struct {
	T       string  `json:"t"`           // "move","down","up","wheel","keydown","keyup"
	Display int     `json:"d,omitempty"` // display index for pointer events
	X       float64 `json:"x,omitempty"` // normalized [0,1]
	Y       float64 `json:"y,omitempty"`
	Button  int     `json:"b,omitempty"` // 0 left, 1 middle, 2 right (DOM MouseEvent.button)
	DX      float64 `json:"dx,omitempty"`
	DY      float64 `json:"dy,omitempty"` // wheel delta in "clicks" (positive = down/right)
	Code    string  `json:"code,omitempty"`
}

type Button int

const (
	Left Button = iota
	Middle
	Right
)

// Injector is implemented per OS.
type Injector interface {
	MoveTo(display int, nx, ny float64) error
	Button(b Button, down bool) error
	Scroll(dx, dy int) error // clicks; positive dy = scroll down
	Key(k Key, down bool) error
	Close() error
}

var ErrUnknownKey = errors.New("input: unknown key code")

// Apply validates ev and dispatches it to inj.
func Apply(inj Injector, ev Event) error {
	switch ev.T {
	case "move":
		if !finite01(ev.X) || !finite01(ev.Y) {
			return fmt.Errorf("input: coordinates out of range")
		}
		return inj.MoveTo(ev.Display, ev.X, ev.Y)
	case "down", "up":
		if ev.Button < 0 || ev.Button > 2 {
			return fmt.Errorf("input: bad button %d", ev.Button)
		}
		return inj.Button(Button(ev.Button), ev.T == "down")
	case "wheel":
		dx, dy := clampClicks(ev.DX), clampClicks(ev.DY)
		if dx == 0 && dy == 0 {
			return nil
		}
		return inj.Scroll(dx, dy)
	case "keydown", "keyup":
		k, ok := Keys[ev.Code]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownKey, ev.Code)
		}
		return inj.Key(k, ev.T == "keydown")
	}
	return fmt.Errorf("input: unknown event type %q", ev.T)
}

func finite01(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }

func clampClicks(v float64) int {
	if math.IsNaN(v) {
		return 0
	}
	return int(math.Max(-10, math.Min(10, math.Round(v))))
}

// Releaser tracks pressed keys/buttons so a session that drops mid-keypress
// can't leave e.g. Ctrl stuck down on the host.
type Releaser struct {
	Injector
	keys    map[Key]bool
	buttons map[Button]bool
}

func NewReleaser(inj Injector) *Releaser {
	return &Releaser{Injector: inj, keys: map[Key]bool{}, buttons: map[Button]bool{}}
}

func (r *Releaser) Key(k Key, down bool) error {
	if down {
		r.keys[k] = true
	} else {
		delete(r.keys, k)
	}
	return r.Injector.Key(k, down)
}

func (r *Releaser) Button(b Button, down bool) error {
	if down {
		r.buttons[b] = true
	} else {
		delete(r.buttons, b)
	}
	return r.Injector.Button(b, down)
}

// ReleaseAll lifts everything still held.
func (r *Releaser) ReleaseAll() {
	for k := range r.keys {
		_ = r.Injector.Key(k, false)
	}
	for b := range r.buttons {
		_ = r.Injector.Button(b, false)
	}
	clear(r.keys)
	clear(r.buttons)
}
