//go:build windows

package helper

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ActiveSession returns the console session id (0xFFFFFFFF if none).
func ActiveSession() uint32 { return windows.WTSGetActiveConsoleSessionId() }

// ErrNoUser means nobody is logged on at the console. Capturing the logon /
// lock screen would require a SYSTEM-token helper on the Winlogon desktop;
// we deliberately don't do that (least privilege).
var ErrNoUser = errors.New("no user is logged on at the console (logon/lock screen not supported)")

type winProc struct {
	StdioConn
	proc windows.Handle
}

// SpawnInSession starts `exe args...` in the active console session as the
// logged-on user. Must be called from a LocalSystem service (needs
// SE_TCB_NAME for WTSQueryUserToken). logPath receives the helper's stderr.
func SpawnInSession(exe string, args []string, logPath string) (Proc, error) {
	sid := ActiveSession()
	if sid == 0xFFFFFFFF {
		return nil, ErrNoUser
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(sid, &tok); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoUser, err)
	}
	defer tok.Close()

	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	// Pipes: child gets inR (stdin) and outW (stdout); we keep inW / outR.
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, sa, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, sa, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}
	closeAll := func(hs ...windows.Handle) {
		for _, h := range hs {
			windows.CloseHandle(h)
		}
	}
	// Parent ends must not be inherited.
	windows.SetHandleInformation(inW, windows.HANDLE_FLAG_INHERIT, 0)
	windows.SetHandleInformation(outR, windows.HANDLE_FLAG_INHERIT, 0)

	logp, _ := windows.UTF16PtrFromString(logPath)
	logH, err := windows.CreateFile(logp, windows.FILE_APPEND_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		sa, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, fmt.Errorf("open helper log: %w", err)
	}

	// Whitelist exactly which handles the child inherits.
	handles := []windows.Handle{inR, outW, logH}
	al, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeAll(inR, inW, outR, outW, logH)
		return nil, err
	}
	defer al.Delete()
	if err := al.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]),
		uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		closeAll(inR, inW, outR, outW, logH)
		return nil, err
	}

	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Desktop:   desktop,
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  inR,
			StdOutput: outW,
			StdErr:    logH,
		},
		ProcThreadAttributeList: al.List(),
	}
	si.Cb = uint32(unsafe.Sizeof(si))

	exeP, _ := windows.UTF16PtrFromString(exe)
	cmdline, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	var pi windows.ProcessInformation
	err = windows.CreateProcessAsUser(tok, exeP, cmdline, nil, nil, true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW|windows.EXTENDED_STARTUPINFO_PRESENT,
		env, nil, &si.StartupInfo, &pi)
	closeAll(inR, outW, logH) // child has its copies now
	if err != nil {
		closeAll(inW, outR)
		return nil, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	return &winProc{
		StdioConn: StdioConn{R: os.NewFile(uintptr(outR), "helper-stdout"), W: os.NewFile(uintptr(inW), "helper-stdin")},
		proc:      pi.Process,
	}, nil
}

func (p *winProc) Wait() error {
	if _, err := windows.WaitForSingleObject(p.proc, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	windows.GetExitCodeProcess(p.proc, &code)
	windows.CloseHandle(p.proc)
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

func (p *winProc) Kill() error { return windows.TerminateProcess(p.proc, 1) }
