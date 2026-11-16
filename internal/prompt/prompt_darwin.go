//go:build darwin

package prompt

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func ask(ctx context.Context, msg string, timeout time.Duration) (bool, error) {
	// The message is passed as argv, never interpolated into AppleScript.
	script := []string{
		"on run argv",
		`set r to display dialog (item 1 of argv) with title "rmt-desk" buttons {"Deny", "Allow"} ` +
			`default button "Deny" cancel button "Deny" with icon caution giving up after ` +
			strconv.Itoa(int(timeout.Seconds())),
		`if gave up of r then return "timeout"`,
		"return button returned of r",
		"end run",
	}
	args := []string{}
	for _, l := range script {
		args = append(args, "-e", l)
	}
	args = append(args, msg)
	out, err := exec.CommandContext(ctx, "osascript", args...).Output()
	if err != nil {
		// "Deny" as cancel button makes osascript exit non-zero (-128).
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) == "Allow", nil
}
