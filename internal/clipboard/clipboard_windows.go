//go:build windows

package clipboard

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procIsFormatAvail    = user32.NewProc("IsClipboardFormatAvailable")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

type win struct{}

// New returns the Windows clipboard (must run in the user's session).
func New() (Clipboard, error) { return win{}, nil }

// open retries: another application may hold the clipboard briefly.
func open(owner uintptr) error {
	for range 10 {
		if r, _, _ := procOpenClipboard.Call(owner); r != 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("clipboard: busy")
}

func (win) Read() (string, error) {
	runtime.LockOSThread() // clipboard ownership is per-thread
	defer runtime.UnlockOSThread()
	if r, _, _ := procIsFormatAvail.Call(cfUnicodeText); r == 0 {
		return "", nil
	}
	if err := open(0); err != nil {
		return "", err
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", errors.New("clipboard: lock failed")
	}
	defer procGlobalUnlock.Call(h)
	return windows.UTF16PtrToString((*uint16)(osPtr(p))), nil
}

func (win) Write(text string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	u, err := windows.UTF16FromString(text)
	if err != nil {
		return err // contains NUL
	}
	// With OpenClipboard(NULL), EmptyClipboard sets a NULL owner and
	// SetClipboardData then fails (documented), so own it via a throwaway
	// message-only window on this (locked) thread.
	cls, _ := windows.UTF16PtrFromString("STATIC")
	const hwndMessage = ^uintptr(2) // (HWND)-3
	hwnd, _, e := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		return fmt.Errorf("clipboard: CreateWindowEx: %v", e)
	}
	defer procDestroyWindow.Call(hwnd)
	if err := open(hwnd); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	size := uintptr(len(u)) * 2
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return errors.New("clipboard: alloc failed")
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return errors.New("clipboard: lock failed")
	}
	copy(unsafe.Slice((*uint16)(osPtr(p)), len(u)), u)
	procGlobalUnlock.Call(h)
	if r, _, e := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h) // ownership not transferred
		return fmt.Errorf("clipboard: SetClipboardData: %v", e)
	}
	return nil
}

// osPtr converts an address of OS-allocated (GlobalLock'd) memory, which the
// Go GC never moves or frees, to a pointer.
func osPtr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }
