package h264

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestABI(t *testing.T) {
	if err := checkABI(); err != nil {
		if strings.Contains(err.Error(), "64-bit") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
}

func TestLoadRejectsUnpinnedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "evil.so")
	os.WriteFile(p, []byte("not openh264"), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unpinned library must be refused, got %v", err)
	}
}

// synthetic "screen": flat background, a moving coloured window and text-like stripes.
func frame(w, h, i int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{235, 238, 242, 255}
			if x >= 40+i*4 && x < 240+i*4 && y >= 30 && y < 150 {
				c = color.RGBA{37, 99, 235, 255}
				if (y/6)%2 == 0 && x%7 < 5 {
					c = color.RGBA{255, 255, 255, 255}
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func psnr(a, b []byte) float64 {
	var mse float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		mse += d * d
	}
	mse /= float64(len(a))
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

// TestEncodeDecodesWithFFmpeg needs the real Cisco library:
//
//	RMT_OPENH264_LIB=/path/to/libopenh264-2.6.0-linux64.8.so go test ./internal/codec/h264
func TestEncodeDecodesWithFFmpeg(t *testing.T) {
	path := os.Getenv("RMT_OPENH264_LIB")
	if path == "" {
		t.Skip("RMT_OPENH264_LIB not set")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	lib, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	const w, h, n = 641, 361, 20 // odd size: exercises cropping to even
	enc := NewEncoder(lib)
	defer enc.Close()

	var stream bytes.Buffer
	keys := 0
	var srcs []*image.RGBA
	for i := range n {
		img := frame(w, h, i)
		au, key, err := enc.Encode(img, 15, 2_000_000, i == 10) // forced IDR mid-stream
		if err != nil {
			t.Fatal(err)
		}
		if au == nil {
			t.Fatalf("frame %d skipped", i)
		}
		if !bytes.HasPrefix(au, []byte{0, 0, 0, 1}) && !bytes.HasPrefix(au, []byte{0, 0, 1}) {
			t.Fatalf("frame %d is not Annex B", i)
		}
		if key {
			keys++
		}
		if (i == 0 || i == 10) && !key {
			t.Fatalf("frame %d should be a keyframe", i)
		}
		stream.Write(au)
		srcs = append(srcs, img)
	}
	if keys != 2 {
		t.Fatalf("keyframes=%d want 2", keys)
	}

	dir := t.TempDir()
	in := filepath.Join(dir, "out.h264")
	os.WriteFile(in, stream.Bytes(), 0o644)
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "h264", "-i", in, "-f", "rawvideo", "-pix_fmt", "rgb24", "-")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg could not decode our stream: %v", err)
	}
	fw, fh := w&^1, h&^1
	fsz := fw * fh * 3
	if len(raw) != n*fsz {
		t.Fatalf("decoded %d bytes, want %d frames of %dx%d", len(raw), n, fw, fh)
	}
	worst := 99.0
	for i := range n {
		var src []byte
		for y := 0; y < fh; y++ {
			for x := 0; x < fw; x++ {
				o := y*srcs[i].Stride + 4*x
				src = append(src, srcs[i].Pix[o:o+3]...)
			}
		}
		p := psnr(src, raw[i*fsz:(i+1)*fsz])
		worst = min(worst, p)
	}
	t.Logf("%d frames, %d bytes total, worst PSNR %.1f dB", n, stream.Len(), worst)
	if worst < 30 {
		t.Fatalf("quality too low: worst PSNR %s dB", strconv.FormatFloat(worst, 'f', 1, 64))
	}
}
