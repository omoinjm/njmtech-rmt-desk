//go:build darwin

package main

import (
	"errors"
	"os/exec"
)

func openPrivacyPane(pane string) error {
	u := map[string]string{
		"screen":        "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture",
		"accessibility": "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility",
	}[pane]
	if u == "" {
		return errors.New("unknown settings pane")
	}
	return exec.Command("open", u).Run()
}
