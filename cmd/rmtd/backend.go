package main

import (
	"log"

	"rmt-desk/internal/capture"
	"rmt-desk/internal/clipboard"
	"rmt-desk/internal/files"
	"rmt-desk/internal/host"
	"rmt-desk/internal/input"
	"rmt-desk/internal/prompt"
	"rmt-desk/internal/proto"
)

// inProcessDesktop must run in the user's session: it touches the screen,
// input, clipboard and the user's ~/Downloads.
func inProcessDesktop() *host.Desktop {
	d := &host.Desktop{Capturer: capture.Screen{}, NewInjector: input.New, NewClipboard: clipboard.New}
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
func inProcessBackend() *backend {
	return &backend{
		Handlers: desktopHandlers(inProcessDesktop()),
		Consent:  prompt.Ask,
		Close:    func() {},
	}
}
