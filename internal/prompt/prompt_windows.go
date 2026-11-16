//go:build windows

package prompt

import (
	"context"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procMessageBoxTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxTimeoutW")

const (
	mbYesNo         = 0x00000004
	mbIconWarning   = 0x00000030
	mbDefButton2    = 0x00000100 // default = No (Deny)
	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
	idYes           = 6
)

func ask(ctx context.Context, msg string, timeout time.Duration) (bool, error) {
	text, _ := windows.UTF16PtrFromString(msg + "\n\nYes = Allow, No = Deny")
	title, _ := windows.UTF16PtrFromString("rmt-desk")
	res := make(chan uintptr, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// MessageBoxTimeoutW is undocumented but present since Windows XP;
		// it returns 32000 on timeout.
		r, _, _ := procMessageBoxTimeout.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)),
			mbYesNo|mbIconWarning|mbDefButton2|mbSystemModal|mbSetForeground|mbTopmost,
			0, uintptr(timeout.Milliseconds()))
		res <- r
	}()
	select {
	case r := <-res:
		return r == idYes, nil
	case <-ctx.Done():
		return false, nil
	}
}
