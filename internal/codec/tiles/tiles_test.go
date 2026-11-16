package tiles

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func TestKeyframeThenDelta(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 200, 100)) // 4x2 tiles at 64px, edge tiles partial
	fill(img, img.Bounds(), color.RGBA{10, 20, 30, 255})
	e := NewEncoder(70)

	f, n, err := e.Encode(img)
	if err != nil || n != 8 {
		t.Fatalf("keyframe: n=%d err=%v, want 8 tiles", n, err)
	}
	w, h, tiles, err := Parse(f)
	if err != nil || w != 200 || h != 100 || len(tiles) != 8 {
		t.Fatalf("parse: %d %d %d %v", w, h, len(tiles), err)
	}
	// Edge tile has partial width; each tile must be a valid JPEG of its size.
	for _, tl := range tiles {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(tl.JPEG))
		if err != nil || cfg.Width != tl.W || cfg.Height != tl.H {
			t.Fatalf("tile %+v: jpeg %dx%d err=%v", tl.X, cfg.Width, cfg.Height, err)
		}
	}

	// Unchanged frame -> nothing to send.
	if f, n, _ := e.Encode(img); f != nil || n != 0 {
		t.Fatalf("unchanged: n=%d", n)
	}

	// Change one pixel in the last (partial) tile.
	img.SetRGBA(199, 99, color.RGBA{255, 0, 0, 255})
	f, n, _ = e.Encode(img)
	if n != 1 {
		t.Fatalf("delta: n=%d want 1", n)
	}
	_, _, tiles, _ = Parse(f)
	if tiles[0].X != 192 || tiles[0].Y != 64 || tiles[0].W != 8 || tiles[0].H != 36 {
		t.Fatalf("delta tile %+v", tiles[0])
	}

	// Reset forces keyframe.
	e.Reset()
	if _, n, _ := e.Encode(img); n != 8 {
		t.Fatalf("after reset n=%d", n)
	}
}

func TestResolutionChangeForcesKeyframe(t *testing.T) {
	e := NewEncoder(70)
	e.Encode(image.NewRGBA(image.Rect(0, 0, 128, 128)))
	if _, n, _ := e.Encode(image.NewRGBA(image.Rect(0, 0, 256, 128))); n != 8 {
		t.Fatalf("n=%d want 8", n)
	}
}

func TestParseRejectsTruncated(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	f, _, _ := NewEncoder(70).Encode(img)
	for _, cut := range []int{3, 10, len(f) - 1} {
		if _, _, _, err := Parse(f[:cut]); err == nil {
			t.Errorf("cut=%d: expected error", cut)
		}
	}
}

func BenchmarkEncode1080pKeyframe(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	e := NewEncoder(70)
	for b.Loop() {
		e.Reset()
		e.Encode(img)
	}
}
