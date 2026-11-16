// Package prompt shows a native Allow/Deny dialog to the person at the host.
// Every implementation defaults to Deny on timeout, close, or error.
package prompt

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// Ask asks whether `who` may connect. It must run in the interactive user
// session (in Windows service mode it runs inside the session helper).
func Ask(ctx context.Context, who string, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	return ask(ctx, message(who), timeout)
}

func message(who string) string {
	return "Allow remote control of this computer?\n\n" + sanitize(who) +
		"\n\nwants to view and control your screen. Deny if you did not expect this."
}

// sanitize strips control characters and bounds length; the identity comes
// from the tailnet, but treat it as untrusted display text anyway.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
