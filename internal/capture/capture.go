// Package capture grabs screen contents. v1 is backed by kbinani/screenshot
// (Windows GDI, macOS ScreenCaptureKit, Linux X11/portal); faster per-OS
// backends (DXGI duplication, PipeWire) can be added behind the same interface.
package capture

import (
	"errors"
	"image"

	"github.com/kbinani/screenshot"
)

type Display struct {
	Index  int             `json:"index"`
	Bounds image.Rectangle `json:"-"`
	Width  int             `json:"width"`
	Height int             `json:"height"`
}

type Capturer interface {
	Displays() []Display
	Capture(display int) (*image.RGBA, error)
}

var ErrNoDisplay = errors.New("capture: no such display")

// Screen is the default Capturer.
type Screen struct{}

func (Screen) Displays() []Display {
	n := screenshot.NumActiveDisplays()
	out := make([]Display, 0, n)
	for i := range n {
		b := screenshot.GetDisplayBounds(i)
		out = append(out, Display{Index: i, Bounds: b, Width: b.Dx(), Height: b.Dy()})
	}
	return out
}

func (Screen) Capture(display int) (*image.RGBA, error) {
	if display < 0 || display >= screenshot.NumActiveDisplays() {
		return nil, ErrNoDisplay
	}
	return screenshot.CaptureDisplay(display)
}

// New returns the best capturer for this platform.
func New() Capturer {
	if c, err := newNative(); err == nil {
		return c
	}
	return Screen{}
}
