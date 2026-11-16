//go:build linux

package prompt

import (
	"context"
	"errors"
	"html"
	"os/exec"
	"strconv"
	"time"
)

func ask(ctx context.Context, msg string, timeout time.Duration) (bool, error) {
	secs := strconv.Itoa(int(timeout.Seconds()))
	if p, err := exec.LookPath("zenity"); err == nil {
		// zenity --text is Pango markup: escape it.
		cmd := exec.CommandContext(ctx, p, "--question", "--title=rmt-desk", "--icon-name=dialog-warning",
			"--ok-label=Allow", "--cancel-label=Deny", "--default-cancel", "--timeout="+secs,
			"--text="+html.EscapeString(msg))
		return exitOK(cmd.Run())
	}
	if p, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.CommandContext(ctx, p, "--title", "rmt-desk", "--yes-label", "Allow", "--no-label", "Deny",
			"--warningyesno", msg)
		return exitOK(cmd.Run())
	}
	return false, errors.New("no dialog tool found (install zenity or kdialog)")
}

// exitOK maps "user clicked Allow" (exit 0) to true; any non-zero exit
// (Deny, closed, timeout) is a clean "no".
func exitOK(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, err
}
