package host

import (
	"context"
	"time"

	"rmt-desk/internal/access"
)

// autoGate approves every session via the consent path (no password set).
func autoGate() *access.Gate {
	return &access.Gate{Consent: func(context.Context, string, time.Duration) (bool, error) { return true, nil }}
}
