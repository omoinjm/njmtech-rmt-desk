//go:build !linux && !windows && !darwin

package clipboard

func New() (Clipboard, error) { return nil, ErrUnavailable }
