//go:build windows

package main

import (
	"log"
	"os"
	"path/filepath"

	"rmt-desk/internal/helper"
	"rmt-desk/internal/host"
	"rmt-desk/internal/proto"
)

func newBackend(dir string, serviceMode bool) (*backend, error) {
	if !serviceMode {
		return inProcessBackend(), nil
	}
	// Session 0 can't see the desktop: delegate to a helper in the user's
	// session, launched on demand and replaced when the console session changes.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &helper.Manager{
		Spawn: func() (helper.Proc, error) {
			return helper.SpawnInSession(exe, []string{"helper"}, filepath.Join(dir, "helper.log"))
		},
		Session: helper.ActiveSession,
		Logf:    log.Printf,
	}
	return &backend{
		Handlers: map[string]host.StreamHandler{
			proto.StreamVideo:     m.Proxy(proto.StreamVideo),
			proto.StreamInput:     m.Proxy(proto.StreamInput),
			proto.StreamClipboard: m.Proxy(proto.StreamClipboard),
			proto.StreamFiles:     m.Proxy(proto.StreamFiles),
		},
		Consent: m.Consent,
		Close:   m.Close,
	}, nil
}
