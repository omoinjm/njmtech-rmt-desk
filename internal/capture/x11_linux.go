//go:build linux

package capture

// Persistent X11 capturer. Compared with kbinani/screenshot it keeps one X
// connection and one MIT-SHM segment across frames (instead of reconnecting
// and allocating a segment per frame), creates the segment 0600 (not 0777,
// which let any local user read the screen), and converts pixels in a tight
// loop instead of a SetRGBA call per pixel.
//
// Display bounds are in X root-window coordinates, the same space XTEST
// uses, so capture and input agree on multi-monitor layouts.

import (
	"errors"
	"fmt"
	"image"
	"os"
	"sync"

	"github.com/gen2brain/shm"
	"github.com/jezek/xgb"
	mshm "github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xinerama"
	"github.com/jezek/xgb/xproto"
)

// RootBounds lists monitors in root-window coordinates (Xinerama order,
// which is also the order kbinani/screenshot and the viewer use).
func RootBounds(c *xgb.Conn) ([]image.Rectangle, error) {
	screen := xproto.Setup(c).DefaultScreen(c)
	whole := image.Rect(0, 0, int(screen.WidthInPixels), int(screen.HeightInPixels))
	if err := xinerama.Init(c); err == nil {
		if r, err := xinerama.QueryScreens(c).Reply(); err == nil && len(r.ScreenInfo) > 0 {
			out := make([]image.Rectangle, 0, len(r.ScreenInfo))
			for _, s := range r.ScreenInfo {
				b := image.Rect(int(s.XOrg), int(s.YOrg), int(s.XOrg)+int(s.Width), int(s.YOrg)+int(s.Height))
				out = append(out, b.Intersect(whole))
			}
			return out, nil
		}
	}
	return []image.Rectangle{whole}, nil
}

type X11 struct {
	mu   sync.Mutex
	c    *xgb.Conn
	root xproto.Window

	useShm bool
	seg    mshm.Seg
	data   []byte // attached segment
}

func NewX11() (*X11, error) {
	if os.Getenv("DISPLAY") == "" {
		return nil, errors.New("capture: DISPLAY not set")
	}
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	setup := xproto.Setup(c)
	screen := setup.DefaultScreen(c)
	if screen.RootDepth != 24 && screen.RootDepth != 32 {
		c.Close()
		return nil, fmt.Errorf("capture: unsupported root depth %d", screen.RootDepth)
	}
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		c.Close()
		return nil, errors.New("capture: unsupported image byte order")
	}
	x := &X11{c: c, root: screen.Root}
	x.useShm = mshm.Init(c) == nil
	return x, nil
}

func (x *X11) Displays() []Display {
	x.mu.Lock()
	defer x.mu.Unlock()
	bs, err := RootBounds(x.c)
	if err != nil {
		return nil
	}
	out := make([]Display, len(bs))
	for i, b := range bs {
		out[i] = Display{Index: i, Bounds: b, Width: b.Dx(), Height: b.Dy()}
	}
	return out
}

// ensureShm (re)allocates the segment if it's too small. The segment is
// marked for removal as soon as the X server has attached it, so it's freed
// automatically even if we crash.
func (x *X11) ensureShm(size int) error {
	if x.data != nil && len(x.data) >= size {
		return nil
	}
	x.freeShm()
	id, err := shm.Get(shm.IPC_PRIVATE, size, shm.IPC_CREAT|0o600)
	if err != nil {
		return err
	}
	defer shm.Rm(id)
	data, err := shm.At(id, 0, 0)
	if err != nil {
		return err
	}
	seg, err := mshm.NewSegId(x.c)
	if err != nil {
		shm.Dt(data)
		return err
	}
	// read-write: the server writes the image into the segment
	if err := mshm.AttachChecked(x.c, seg, uint32(id), false).Check(); err != nil {
		shm.Dt(data)
		return err
	}
	x.seg, x.data = seg, data
	return nil
}

func (x *X11) freeShm() {
	if x.data != nil {
		mshm.Detach(x.c, x.seg)
		shm.Dt(x.data)
		x.data = nil
	}
}

func (x *X11) Capture(display int) (*image.RGBA, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	bs, err := RootBounds(x.c)
	if err != nil {
		return nil, err
	}
	if display < 0 || display >= len(bs) {
		return nil, ErrNoDisplay
	}
	r := bs[display]
	w, h := r.Dx(), r.Dy()
	var src []byte
	if x.useShm {
		if err := x.ensureShm(w * h * 4); err != nil {
			x.useShm = false // e.g. X server in another IPC namespace (WSLg)
		} else {
			_, err := mshm.GetImage(x.c, xproto.Drawable(x.root), int16(r.Min.X), int16(r.Min.Y),
				uint16(w), uint16(h), 0xffffffff, xproto.ImageFormatZPixmap, x.seg, 0).Reply()
			if err != nil {
				x.freeShm()
				x.useShm = false
			} else {
				src = x.data[:w*h*4]
			}
		}
	}
	if src == nil {
		reply, err := xproto.GetImage(x.c, xproto.ImageFormatZPixmap, xproto.Drawable(x.root),
			int16(r.Min.X), int16(r.Min.Y), uint16(w), uint16(h), 0xffffffff).Reply()
		if err != nil {
			return nil, err
		}
		src = reply.Data
		if len(src) < w*h*4 {
			return nil, errors.New("capture: short image")
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bgrxToRGBA(img.Pix, src[:w*h*4])
	return img, nil
}

// bgrxToRGBA converts 32-bit little-endian TrueColor (B,G,R,X) to RGBA.
func bgrxToRGBA(dst, src []byte) {
	for i := 0; i+3 < len(src); i += 4 {
		s := src[i : i+4 : i+4]
		d := dst[i : i+4 : i+4]
		d[0], d[1], d[2], d[3] = s[2], s[1], s[0], 0xFF
	}
}

func (x *X11) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.freeShm()
	x.c.Close()
	return nil
}
