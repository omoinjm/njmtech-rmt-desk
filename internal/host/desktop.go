package host

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/capture"
	"rmt-desk/internal/codec/tiles"
	"rmt-desk/internal/input"
	"rmt-desk/internal/proto"
)

// Desktop serves the video and input streams.
type Desktop struct {
	Capturer    capture.Capturer
	NewInjector func() (input.Injector, error)
	Logf        func(string, ...any)

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
	apply := func(r proto.VideoReq) error {
		disps := displayInfo(d.Capturer)
		if r.Display < 0 || r.Display >= len(disps) {
			r.Display = 0
		}
		req = r
		req.FPS = clamp(r.FPS, 1, 30, 15)
		enc.Quality = clamp(r.Quality, 10, 95, 70)
		enc.Reset()
		return writeJSON(proto.MsgVideoInfo, proto.VideoInfo{Displays: disps, Current: req.Display})
	}
	if err := apply(req); err != nil {
		return
	}
	d.logf("video start: %s display=%d fps=%d q=%d", id.NodeName, req.Display, req.FPS, enc.Quality)

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
