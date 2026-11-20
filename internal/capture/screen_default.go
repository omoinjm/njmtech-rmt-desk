//go:build !darwin || !cgo

package capture

import (
	"image"

	"github.com/kbinani/screenshot"
)

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
