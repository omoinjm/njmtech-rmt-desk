// Package tiles implements the v1 video codec: the screen is split into
// fixed-size tiles, each tile is hashed, and only tiles that changed since the
// previous frame are JPEG-encoded and sent.
//
// Frame wire format (big-endian):
//
//	u16 screenW | u16 screenH | u16 nTiles
//	nTiles × ( u16 x | u16 y | u16 w | u16 h | u32 len | len bytes JPEG )
//
// The viewer decodes each tile independently (browser createImageBitmap), so
// the format is deliberately trivial.
package tiles

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/maphash"
	"image"
	"image/jpeg"
	"runtime"
	"sync"
	"sync/atomic"
)

const DefaultTileSize = 64

type Encoder struct {
	TileSize int
	Quality  int // JPEG quality 1-100

	seed   maphash.Seed
	prev   []uint64
	bounds image.Rectangle
	buf    bytes.Buffer
}

func NewEncoder(quality int) *Encoder {
	return &Encoder{TileSize: DefaultTileSize, Quality: quality, seed: maphash.MakeSeed()}
}

// Reset forces the next Encode to send every tile (keyframe).
func (e *Encoder) Reset() { e.prev = nil }

// Encode returns the frame for img and the number of changed tiles.
// If nothing changed it returns (nil, 0, nil). The returned slice is only
// valid until the next call.
func (e *Encoder) Encode(img *image.RGBA) ([]byte, int, error) {
	b := img.Bounds()
	if b.Dx() > 0xFFFF || b.Dy() > 0xFFFF {
		return nil, 0, errors.New("tiles: image too large")
	}
	ts := e.TileSize
	cols := (b.Dx() + ts - 1) / ts
	rows := (b.Dy() + ts - 1) / ts
	if b != e.bounds || len(e.prev) != cols*rows {
		e.prev = nil
		e.bounds = b
	}
	keyframe := e.prev == nil
	if keyframe {
		e.prev = make([]uint64, cols*rows)
	}

	// Pass 1 (cheap, serial): hash every tile, collect the changed ones.
	var changed []image.Rectangle
	for ty := range rows {
		for tx := range cols {
			r := image.Rect(b.Min.X+tx*ts, b.Min.Y+ty*ts, b.Min.X+(tx+1)*ts, b.Min.Y+(ty+1)*ts).Intersect(b)
			h := e.hashRect(img, r)
			idx := ty*cols + tx
			if !keyframe && e.prev[idx] == h {
				continue
			}
			e.prev[idx] = h
			changed = append(changed, r)
		}
	}
	n := len(changed)
	if n == 0 {
		return nil, 0, nil
	}

	// Pass 2 (expensive, parallel): JPEG-encode changed tiles.
	jpegs := make([][]byte, n)
	errs := make([]error, n)
	opts := &jpeg.Options{Quality: e.Quality}
	workers := min(runtime.GOMAXPROCS(0), n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				buf.Reset()
				errs[i] = jpeg.Encode(&buf, img.SubImage(changed[i]), opts)
				jpegs[i] = bytes.Clone(buf.Bytes())
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			e.prev = nil // state is inconsistent; force keyframe next time
			return nil, 0, err
		}
	}

	// Pass 3 (serial): assemble the frame.
	e.buf.Reset()
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[0:], uint16(b.Dx()))
	binary.BigEndian.PutUint16(hdr[2:], uint16(b.Dy()))
	binary.BigEndian.PutUint16(hdr[4:], uint16(n))
	e.buf.Write(hdr[:])
	for i, r := range changed {
		var th [12]byte
		binary.BigEndian.PutUint16(th[0:], uint16(r.Min.X-b.Min.X))
		binary.BigEndian.PutUint16(th[2:], uint16(r.Min.Y-b.Min.Y))
		binary.BigEndian.PutUint16(th[4:], uint16(r.Dx()))
		binary.BigEndian.PutUint16(th[6:], uint16(r.Dy()))
		binary.BigEndian.PutUint32(th[8:], uint32(len(jpegs[i])))
		e.buf.Write(th[:])
		e.buf.Write(jpegs[i])
	}
	return e.buf.Bytes(), n, nil
}

func (e *Encoder) hashRect(img *image.RGBA, r image.Rectangle) uint64 {
	var h maphash.Hash
	h.SetSeed(e.seed)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		off := img.PixOffset(r.Min.X, y)
		h.Write(img.Pix[off : off+r.Dx()*4])
	}
	return h.Sum64()
}

// Tile is a decoded tile header plus its JPEG bytes.
type Tile struct {
	X, Y, W, H int
	JPEG       []byte
}

// Parse decodes a frame's structure (not the JPEG data). Used by tests and
// any non-browser viewer.
func Parse(frame []byte) (w, h int, tiles []Tile, err error) {
	if len(frame) < 6 {
		return 0, 0, nil, errors.New("tiles: short frame")
	}
	w = int(binary.BigEndian.Uint16(frame[0:]))
	h = int(binary.BigEndian.Uint16(frame[2:]))
	n := int(binary.BigEndian.Uint16(frame[4:]))
	p := frame[6:]
	for range n {
		if len(p) < 12 {
			return 0, 0, nil, errors.New("tiles: short tile header")
		}
		t := Tile{
			X: int(binary.BigEndian.Uint16(p[0:])), Y: int(binary.BigEndian.Uint16(p[2:])),
			W: int(binary.BigEndian.Uint16(p[4:])), H: int(binary.BigEndian.Uint16(p[6:])),
		}
		l := int(binary.BigEndian.Uint32(p[8:]))
		p = p[12:]
		if len(p) < l {
			return 0, 0, nil, errors.New("tiles: short tile data")
		}
		t.JPEG = p[:l]
		p = p[l:]
		tiles = append(tiles, t)
	}
	return w, h, tiles, nil
}
