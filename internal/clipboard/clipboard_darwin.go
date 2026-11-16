//go:build darwin

package clipboard

import (
	"os"
	"os/exec"
	"strings"
)

// pbcopy/pbpaste ship with macOS; avoids cgo. LANG must be UTF-8 or
// non-ASCII text is mangled.
type mac struct{}

func New() (Clipboard, error) { return mac{}, nil }

func env() []string { return append(os.Environ(), "LANG=en_US.UTF-8", "LC_CTYPE=UTF-8") }

func (mac) Read() (string, error) {
	cmd := exec.Command("pbpaste")
	cmd.Env = env()
	out, err := cmd.Output()
	return string(out), err
}

func (mac) Write(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Env = env()
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
