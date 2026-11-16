//go:build !linux && !darwin && !windows

package prompt

import (
	"context"
	"errors"
	"time"
)

func ask(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("consent prompt not supported on this OS")
}
