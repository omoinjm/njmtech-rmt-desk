package h264

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"runtime"
	"sync"
	"unsafe"
)

// Lib is a loaded OpenH264 library.
type Lib struct {
	create, destroy uintptr
}

var (
	loadMu sync.Mutex
	loaded = map[string]*Lib{}
)

// Load verifies path against the pinned hash for this platform and loads it.
// Repeated calls return the same Lib.
func Load(path string) (*Lib, error) {
	if err := checkABI(); err != nil {
		return nil, err
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	if l, ok := loaded[path]; ok {
		return l, nil
	}
	rel, err := currentRelease()
	if err != nil {
		return nil, err
	}
	if err := verifyFile(path, rel.LibSHA256); err != nil {
		return nil, err
	}
	h, err := dlopen(path)
	if err != nil {
		return nil, fmt.Errorf("h264: load %s: %w", path, err)
	}
	l := &Lib{}
	if l.create, err = dlsym(h, "WelsCreateSVCEncoder"); err != nil {
		return nil, err
	}
	if l.destroy, err = dlsym(h, "WelsDestroySVCEncoder"); err != nil {
		return nil, err
	}
	loaded[path] = l
	return l, nil
}

func verifyFile(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("h264: %w (run 'rmtd codec install')", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("h264: %s does not match the pinned Cisco OpenH264 build (sha256 %s); refusing to load", path, got[:16])
	}
	return nil
}

// Encoder is a single-threaded H.264 encoder for screen content. It
// re-initializes itself when the frame size changes.
type Encoder struct {
	lib     *Lib
	enc     uintptr // ISVCEncoder*
	vtbl    *[9]uintptr
	w, h    int
	fps     int
	rate    int
	y, u, v []byte
	ts      int64
	info    *frameBSInfo
	pic     *sourcePicture
}

func NewEncoder(lib *Lib) *Encoder {
	return &Encoder{lib: lib, info: new(frameBSInfo), pic: new(sourcePicture)}
}

func (e *Encoder) call(slot int, args ...uintptr) int32 {
	a := append([]uintptr{e.enc}, args...)
	r := callN(e.vtbl[slot], a...)
	return int32(r)
}

func (e *Encoder) init(w, h, fps, bitrate int) error {
	e.close()
	var p uintptr
	if r := int32(callN(e.lib.create, uintptr(unsafe.Pointer(&p)))); r != 0 || p == 0 {
		return fmt.Errorf("h264: WelsCreateSVCEncoder failed (%d)", r)
	}
	e.enc = p
	e.vtbl = (*[9]uintptr)(cptr(*(*uintptr)(cptr(p))))

	quiet := int32(logQuiet)
	e.call(vSetOption, optTraceLevel, uintptr(unsafe.Pointer(&quiet)))
	param := encParamBase{
		UsageType: usageScreenContent, Width: int32(w), Height: int32(h),
		Bitrate: int32(bitrate), RCMode: rcBitrateMode, MaxFrameRate: float32(fps),
	}
	if r := e.call(vInitialize, uintptr(unsafe.Pointer(&param))); r != 0 {
		e.close()
		return fmt.Errorf("h264: Initialize failed (%d) for %dx%d", r, w, h)
	}
	format := int32(formatI420)
	e.call(vSetOption, optDataFormat, uintptr(unsafe.Pointer(&format)))
	runtime.KeepAlive(&param)

	e.w, e.h, e.fps, e.rate = w, h, fps, bitrate
	e.y = make([]byte, w*h)
	e.u = make([]byte, w*h/4)
	e.v = make([]byte, w*h/4)
	return nil
}

func (e *Encoder) setRate(fps, bitrate int) {
	if fps != e.fps {
		f := float32(fps)
		e.call(vSetOption, optFrameRate, uintptr(unsafe.Pointer(&f)))
		e.fps = fps
	}
	if bitrate != e.rate {
		bi := bitrateInfo{Layer: spatialLayerAll, Bitrate: int32(bitrate)}
		e.call(vSetOption, optBitrate, uintptr(unsafe.Pointer(&bi)))
		e.rate = bitrate
	}
}

// Encode returns an Annex B access unit (nil if the encoder skipped the
// frame) and whether it is a keyframe. The image is cropped to even
// dimensions, as 4:2:0 requires.
func (e *Encoder) Encode(img *image.RGBA, fps, bitrate int, forceKey bool) ([]byte, bool, error) {
	w, h := img.Bounds().Dx()&^1, img.Bounds().Dy()&^1
	if w < 16 || h < 16 {
		return nil, false, errors.New("h264: frame too small")
	}
	if e.enc == 0 || w != e.w || h != e.h {
		if err := e.init(w, h, fps, bitrate); err != nil {
			return nil, false, err
		}
		forceKey = true
	} else {
		e.setRate(fps, bitrate)
	}
	if forceKey {
		e.call(vForceIntraFrame, 1)
	}
	rgbaToI420(img, w, h, e.y, e.u, e.v)

	*e.pic = sourcePicture{
		ColorFormat: formatI420,
		Stride:      [4]int32{int32(w), int32(w / 2), int32(w / 2), 0},
		Data:        [4]*byte{&e.y[0], &e.u[0], &e.v[0], nil},
		Width:       int32(w), Height: int32(h),
		Timestamp: e.ts,
	}
	e.ts += int64(1000 / max(fps, 1))
	*e.info = frameBSInfo{}
	r := e.call(vEncodeFrame, uintptr(unsafe.Pointer(e.pic)), uintptr(unsafe.Pointer(e.info)))
	runtime.KeepAlive(e.y)
	runtime.KeepAlive(e.u)
	runtime.KeepAlive(e.v)
	if r != 0 {
		return nil, false, fmt.Errorf("h264: EncodeFrame failed (%d)", r)
	}
	if e.info.FrameType == frameSkip {
		return nil, false, nil
	}
	var out []byte
	for i := 0; i < int(e.info.LayerNum) && i < len(e.info.Layers); i++ {
		l := &e.info.Layers[i]
		if l.NalCount <= 0 || l.NalLen == 0 || l.BsBuf == 0 {
			continue
		}
		lens := unsafe.Slice((*int32)(cptr(l.NalLen)), l.NalCount)
		total := 0
		for _, n := range lens {
			total += int(n)
		}
		out = append(out, unsafe.Slice((*byte)(cptr(l.BsBuf)), total)...)
	}
	key := e.info.FrameType == frameIDR || e.info.FrameType == frameI
	return out, key, nil
}

func (e *Encoder) close() {
	if e.enc != 0 {
		e.call(vUninitialize)
		callN(e.lib.destroy, e.enc)
		e.enc, e.vtbl = 0, nil
	}
}

func (e *Encoder) Close() { e.close() }

// rgbaToI420 converts with BT.601 limited-range coefficients (what decoders
// assume when the stream carries no colour description).
func rgbaToI420(img *image.RGBA, w, h int, y, u, v []byte) {
	workers := min(runtime.GOMAXPROCS(0), h/2)
	var wg sync.WaitGroup
	rowsPer := (h/2 + workers - 1) / workers
	for wk := range workers {
		r0, r1 := wk*rowsPer, min((wk+1)*rowsPer, h/2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cy := r0; cy < r1; cy++ {
				for dy := 0; dy < 2; dy++ {
					row := 2*cy + dy
					p := img.Pix[row*img.Stride:]
					yo := y[row*w:]
					for x := 0; x < w; x++ {
						r, g, b := int(p[4*x]), int(p[4*x+1]), int(p[4*x+2])
						yo[x] = uint8(((66*r + 129*g + 25*b + 128) >> 8) + 16)
					}
				}
				p0 := img.Pix[(2*cy)*img.Stride:]
				p1 := img.Pix[(2*cy+1)*img.Stride:]
				uo, vo := u[cy*(w/2):], v[cy*(w/2):]
				for cx := 0; cx < w/2; cx++ {
					i := 8 * cx
					r := int(p0[i]) + int(p0[i+4]) + int(p1[i]) + int(p1[i+4])
					g := int(p0[i+1]) + int(p0[i+5]) + int(p1[i+1]) + int(p1[i+5])
					b := int(p0[i+2]) + int(p0[i+6]) + int(p1[i+2]) + int(p1[i+6])
					uo[cx] = uint8(((-38*r - 74*g + 112*b + 512) >> 10) + 128)
					vo[cx] = uint8(((112*r - 94*g - 18*b + 512) >> 10) + 128)
				}
			}
		}()
	}
	wg.Wait()
}
