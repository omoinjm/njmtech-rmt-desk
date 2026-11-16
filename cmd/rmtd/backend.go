package main

import (
	"rmt-desk/internal/capture"
	"rmt-desk/internal/host"
	"rmt-desk/internal/input"
	"rmt-desk/internal/prompt"
	"rmt-desk/internal/proto"
)

func inProcessDesktop() *host.Desktop {
	return &host.Desktop{Capturer: capture.Screen{}, NewInjector: input.New}
}

// inProcessBackend serves the desktop from this process (foreground `up`,
// and macOS/Linux user services, which already run in the GUI session).
func inProcessBackend() *backend {
	desk := inProcessDesktop()
	return &backend{
		Handlers: map[string]host.StreamHandler{
			proto.StreamVideo: desk.Video,
			proto.StreamInput: desk.Input,
		},
		Consent: prompt.Ask,
		Close:   func() {},
	}
}
