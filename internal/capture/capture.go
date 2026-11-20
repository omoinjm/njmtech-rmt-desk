// Package capture grabs screen contents. Screen is backed by kbinani/screenshot
// (Windows GDI, Linux X11/portal) and, on macOS, by screen_darwin.go
// (ScreenCaptureKit on 14+, CoreGraphics before); faster per-OS backends
// (DXGI duplication, PipeWire) can be added behind the same interface.
package capture

import (
	"errors"
	"image"
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

// New returns the best capturer for this platform.
func New() Capturer {
	if c, err := newNative(); err == nil {
		return c
	}
	return Screen{}
}
