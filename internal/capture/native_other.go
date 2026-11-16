//go:build !linux

package capture

import "errors"

func newNative() (Capturer, error) { return nil, errors.New("no native capturer") }
