package host

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/maphash"
	"image"
	"log"
	"net"
	"slices"
	"sync"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/codec/tiles"
	"rmt-desk/internal/files"
	"rmt-desk/internal/input"
	"rmt-desk/internal/proto"
)

// Desktop serves the video, input, clipboard and files streams. It runs
// wherever the user's desktop is reachable (in-process, or the Windows
// session helper).
type Desktop struct {
	Capturer     capture.Capturer
	NewInjector  func() (input.Injector, error)
	NewClipboard func() (clipboard.Clipboard, error)
	Files        *files.Exchange
	// NewH264 returns an H.264 encoder, or an error if the codec isn't
	// installed (the stream then falls back to JPEG tiles).
	NewH264 func() (H264Encoder, error)
	Logf    func(string, ...any)

	// Only one session may drive input at a time.
	inputMu sync.Mutex
}

func (d *Desktop) logf(f string, a ...any) {
	if d.Logf != nil {
		d.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(hi, v))
}

func displayInfo(c capture.Capturer) []proto.DisplayInfo {
	var out []proto.DisplayInfo
	for _, d := range c.Displays() {
		out = append(out, proto.DisplayInfo{Index: d.Index, Width: d.Width, Height: d.Height})
	}
	return out
}

// H264Encoder is implemented by codec/h264.Encoder.
type H264Encoder interface {
	Encode(img *image.RGBA, fps, bitrate int, forceKey bool) ([]byte, bool, error)
	Close()
}

// h264Bitrate maps the quality tier to a bitrate, scaled by resolution.
func h264Bitrate(quality, w, h int) int {
	base := 4_000_000
	switch {
	case quality <= 40:
		base = 1_500_000
	case quality > 70:
		base = 8_000_000
	}
	scale := max(0.25, float64(w*h)/(1920*1080))
	return int(float64(base) * scale)
}

// Video streams screen updates. The viewer sends VideoReq first and may send
// more to reconfigure. Back-pressure is natural: a slow link blocks the
// write, and the next capture simply picks up the latest screen state.
func (d *Desktop) Video(ctx context.Context, id authz.Identity, c net.Conn) {
	defer c.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var req proto.VideoReq
	if err := proto.ReadJSON(c, proto.MsgVideoReq, &req); err != nil {
		return
	}
	reqs := make(chan proto.VideoReq, 1)
	go func() {
		defer cancel()
		for {
			var r proto.VideoReq
			if err := proto.ReadJSON(c, proto.MsgVideoReq, &r); err != nil {
				return
			}
			select {
			case <-reqs: // drop stale pending request
			default:
			}
			reqs <- r
		}
	}()

	var wmu sync.Mutex // serialize writes (frames + info)
	write := func(t proto.MsgType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.WriteFrame(c, t, b)
	}
	writeJSON := func(t proto.MsgType, v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return proto.WriteJSON(c, t, v)
	}

	enc := tiles.NewEncoder(70)
	var h264 H264Encoder
	defer func() {
		if h264 != nil {
			h264.Close()
		}
	}()
	codec := proto.CodecTiles
	forceKey := true
	apply := func(r proto.VideoReq) error {
		disps := displayInfo(d.Capturer)
		if r.Display < 0 || r.Display >= len(disps) {
			r.Display = 0
		}
		req = r
		req.FPS = clamp(r.FPS, 1, 30, 15)
		enc.Quality = clamp(r.Quality, 10, 95, 70)
		enc.Reset()
		forceKey = true
		codec = proto.CodecTiles
		if slices.Contains(r.Codecs, proto.CodecH264) && d.NewH264 != nil {
			if h264 == nil {
				var err error
				if h264, err = d.NewH264(); err != nil {
					d.logf("h264 unavailable, using JPEG tiles: %v", err)
					h264 = nil
				}
			}
			if h264 != nil {
				codec = proto.CodecH264
			}
		}
		return writeJSON(proto.MsgVideoInfo, proto.VideoInfo{Displays: disps, Current: req.Display, Codec: codec})
	}
	if err := apply(req); err != nil {
		return
	}
	d.logf("video start: %s display=%d fps=%d q=%d codec=%s", id.NodeName, req.Display, req.FPS, enc.Quality, codec)

	seed := maphash.MakeSeed()
	var lastHash uint64
	var lastChange time.Time
	tick := time.NewTicker(time.Second / time.Duration(req.FPS))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-reqs:
			if err := apply(r); err != nil {
				return
			}
			tick.Reset(time.Second / time.Duration(req.FPS))
			continue
		case <-tick.C:
		}
		img, err := d.Capturer.Capture(req.Display)
		if err != nil {
			d.logf("capture: %v", err)
			_ = writeJSON(proto.MsgError, proto.Error{Message: "capture failed: " + err.Error()})
			return
		}
		if codec == proto.CodecTiles {
			frame, n, err := enc.Encode(img)
			if err != nil {
				d.logf("encode: %v", err)
				return
			}
			if n == 0 {
				continue
			}
			if err := write(proto.MsgVideoFrame, frame); err != nil {
				return
			}
			continue
		}

		// H.264: skip static screens, but keep encoding ~1s after a change so
		// the encoder can refine quality.
		hsh := maphash.Bytes(seed, img.Pix)
		now := time.Now()
		if hsh != lastHash {
			lastHash, lastChange = hsh, now
		} else if !forceKey && now.Sub(lastChange) > time.Second {
			continue
		}
		b := img.Bounds()
		au, key, err := h264.Encode(img, req.FPS, h264Bitrate(enc.Quality, b.Dx(), b.Dy()), forceKey)
		if err != nil {
			d.logf("h264 encode: %v; falling back to JPEG tiles", err)
			h264.Close()
			h264, codec = nil, proto.CodecTiles
			enc.Reset()
			_ = writeJSON(proto.MsgVideoInfo, proto.VideoInfo{Displays: displayInfo(d.Capturer), Current: req.Display, Codec: codec})
			continue
		}
		forceKey = false
		if au == nil {
			continue
		}
		hdr := make([]byte, 5, 5+len(au))
		if key {
			hdr[0] = 1
		}
		binary.BigEndian.PutUint16(hdr[1:], uint16(b.Dx()&^1))
		binary.BigEndian.PutUint16(hdr[3:], uint16(b.Dy()&^1))
		if err := write(proto.MsgVideoH264, append(hdr, au...)); err != nil {
			return
		}
	}
}

// Input applies viewer input events. Held keys/buttons are released when the
// stream ends so a dropped connection can't leave modifiers stuck.
func (d *Desktop) Input(ctx context.Context, id authz.Identity, c net.Conn) {
	defer c.Close()
	if !d.inputMu.TryLock() {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: "another session is controlling this host"})
		return
	}
	defer d.inputMu.Unlock()

	inj, err := d.NewInjector()
	if err != nil {
		d.logf("input unavailable: %v", err)
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: err.Error()})
		return
	}
	rel := input.NewReleaser(inj)
	defer func() {
		rel.ReleaseAll()
		inj.Close()
	}()
	go func() { <-ctx.Done(); c.Close() }()

	d.logf("input start: %s", id.NodeName)
	for {
		var ev input.Event
		if err := proto.ReadJSON(c, proto.MsgInput, &ev); err != nil {
			return
		}
		if err := input.Apply(rel, ev); err != nil {
			if errors.Is(err, input.ErrUnknownKey) {
				continue // e.g. media keys we don't map; ignore quietly
			}
			d.logf("input: %v", err)
		}
	}
}

// Clipboard syncs the host clipboard with the viewer's for the session.
func (d *Desktop) Clipboard(ctx context.Context, id authz.Identity, c net.Conn) {
	defer c.Close()
	if d.NewClipboard == nil {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: "clipboard not available on host"})
		return
	}
	cb, err := d.NewClipboard()
	if err != nil {
		d.logf("clipboard unavailable: %v", err)
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: err.Error()})
		return
	}
	d.logf("clipboard sync start: %s", id.NodeName)
	_ = clipboard.Sync(ctx, cb, c, d.logf)
}

// FileStream serves one file operation against the exchange folder.
func (d *Desktop) FileStream(ctx context.Context, id authz.Identity, c net.Conn) {
	if d.Files == nil {
		_ = proto.WriteJSON(c, proto.MsgError, proto.Error{Message: "file transfer not available on host"})
		c.Close()
		return
	}
	d.Files.Serve(ctx, c)
}
