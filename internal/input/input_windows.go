//go:build windows

package input

// Windows backend via SendInput (pure Go syscalls).
//
// NOTE: a process in session 0 (a Windows service) cannot inject into the
// interactive desktop; phase 3 runs this inside a per-session helper.

import (
	"fmt"
	"unsafe"

	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows"
)

var (
	user32                        = windows.NewLazySystemDLL("user32.dll")
	procSendInput                 = user32.NewProc("SendInput")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfWheel      = 0x0800
	mouseeventfHWheel     = 0x1000
	mouseeventfVirtualDsk = 0x4000
	mouseeventfAbsolute   = 0x8000

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002
	keyeventfScancode = 0x0008

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	wheelDelta = 120
)

// INPUT = { DWORD type; union { MOUSEINPUT; KEYBDINPUT; HARDWAREINPUT } }.
// Nesting the union member as its own struct makes Go's alignment reproduce
// the C layout: 40 bytes on 64-bit (union at offset 8), 28 bytes on 32-bit.
type mouseData struct {
	Dx    int32
	Dy    int32
	Data  uint32
	Flags uint32
	Time  uint32
	Extra uintptr
}

type kbdData struct {
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	Extra uintptr
	_     [8]byte // union is sized by MOUSEINPUT
}

type mouseInput struct {
	Type uint32
	Mi   mouseData
}

type keybdInput struct {
	Type uint32
	Ki   kbdData
}

type win struct{}

func New() (Injector, error) {
	// Per-monitor DPI aware v2, so virtual-screen metrics are physical pixels
	// matching what capture returns. Ignore failure (older Windows).
	if procSetProcessDpiAwarenessCtx.Find() == nil {
		procSetProcessDpiAwarenessCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
	}
	if unsafe.Sizeof(mouseInput{}) != unsafe.Sizeof(keybdInput{}) ||
		unsafe.Offsetof(mouseInput{}.Mi) != unsafe.Alignof(uintptr(0)) {
		return nil, fmt.Errorf("input: INPUT layout mismatch")
	}
	return win{}, nil
}

func send[T mouseInput | keybdInput](in *T) error {
	n, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
	if n != 1 {
		return fmt.Errorf("input: SendInput: %v", err)
	}
	return nil
}

func metric(i uintptr) int32 {
	r, _, _ := procGetSystemMetrics.Call(i)
	return int32(r)
}

func (win) MoveTo(display int, nx, ny float64) error {
	if display < 0 || display >= screenshot.NumActiveDisplays() {
		return fmt.Errorf("input: no display %d", display)
	}
	b := screenshot.GetDisplayBounds(display)
	px := float64(b.Min.X) + nx*float64(b.Dx()-1)
	py := float64(b.Min.Y) + ny*float64(b.Dy()-1)
	vx, vy := float64(metric(smXVirtualScreen)), float64(metric(smYVirtualScreen))
	vw, vh := float64(metric(smCXVirtualScreen)), float64(metric(smCYVirtualScreen))
	if vw <= 1 || vh <= 1 {
		return fmt.Errorf("input: bad virtual screen metrics")
	}
	return send(&mouseInput{
		Type: inputMouse,
		Mi: mouseData{
			Dx:    int32((px - vx) * 65535 / (vw - 1)),
			Dy:    int32((py - vy) * 65535 / (vh - 1)),
			Flags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDsk,
		},
	})
}

func (win) Button(b Button, down bool) error {
	var f uint32
	switch b {
	case Left:
		f = map[bool]uint32{true: mouseeventfLeftDown, false: mouseeventfLeftUp}[down]
	case Middle:
		f = map[bool]uint32{true: mouseeventfMiddleDown, false: mouseeventfMiddleUp}[down]
	case Right:
		f = map[bool]uint32{true: mouseeventfRightDown, false: mouseeventfRightUp}[down]
	}
	return send(&mouseInput{Type: inputMouse, Mi: mouseData{Flags: f}})
}

func (win) Scroll(dx, dy int) error {
	if dy != 0 { // Windows: positive = wheel away from user (scroll up)
		if err := send(&mouseInput{Type: inputMouse, Mi: mouseData{Flags: mouseeventfWheel, Data: uint32(int32(-dy * wheelDelta))}}); err != nil {
			return err
		}
	}
	if dx != 0 {
		return send(&mouseInput{Type: inputMouse, Mi: mouseData{Flags: mouseeventfHWheel, Data: uint32(int32(dx * wheelDelta))}})
	}
	return nil
}

func (win) Key(k Key, down bool) error {
	f := uint32(keyeventfScancode)
	if k.Scan&0xFF00 == 0xE000 {
		f |= keyeventfExtended
	}
	if !down {
		f |= keyeventfKeyUp
	}
	return send(&keybdInput{Type: inputKeyboard, Ki: kbdData{Scan: k.Scan & 0xFF, Flags: f}})
}

func (win) Close() error { return nil }
