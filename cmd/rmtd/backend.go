package main

import (
	"log"

	"rmt-desk/internal/capture"
	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/codec/h264"
	"rmt-desk/internal/files"
	"rmt-desk/internal/host"
	"rmt-desk/internal/input"
	"rmt-desk/internal/prompt"
	"rmt-desk/internal/proto"
)

// inProcessDesktop must run in the user's session: it touches the screen,
// input, clipboard and the user's ~/Downloads. codecDir holds the optional
// OpenH264 library ('rmtd codec install').
func inProcessDesktop(codecDir string) *host.Desktop {
	d := &host.Desktop{Capturer: capture.New(), NewInjector: input.New, NewClipboard: clipboard.New}
	d.NewH264 = func() (host.H264Encoder, error) {
		p, err := h264.LibPath(codecDir)
		if err != nil {
			return nil, err
		}
		lib, err := h264.Load(p)
		if err != nil {
			return nil, err
		}
		return h264.NewEncoder(lib), nil
	}
	if dir, err := files.DefaultDir(); err != nil {
		log.Printf("file transfer disabled: %v", err)
	} else {
		d.Files = &files.Exchange{Dir: dir, OnEvent: func(op, name string, size int64) {
			log.Printf("file %s: %s (%d bytes) in %s", op, name, size, dir)
		}}
	}
	return d
}

// desktopHandlers are the streams served by a Desktop.
func desktopHandlers(d *host.Desktop) map[string]host.StreamHandler {
	return map[string]host.StreamHandler{
		proto.StreamVideo:     d.Video,
		proto.StreamInput:     d.Input,
		proto.StreamClipboard: d.Clipboard,
		proto.StreamFiles:     d.FileStream,
	}
}

// inProcessBackend serves the desktop from this process (foreground `up`,
// and macOS/Linux user services, which already run in the GUI session).
func inProcessBackend(codecDir string) *backend {
	return &backend{
		Handlers: desktopHandlers(inProcessDesktop(codecDir)),
		Consent:  prompt.Ask,
		Close:    func() {},
	}
}
