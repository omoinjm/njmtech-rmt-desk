//go:build windows || linux

package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	"rmt-desk/internal/hostsetup"

	"fyne.io/systray"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed build/tray.png
var trayPNG []byte

//go:embed build/windows/icon.ico
var trayICO []byte

// startTray shows the tray icon. The window then hides to the tray on close
// instead of quitting (main sets HideWindowOnClose when trayAvailable).
func (a *App) startTray() {
	go func() {
		// Windows delivers the tray window's messages only to the thread
		// that created it, so creation and the message loop share this one.
		runtime.LockOSThread()
		systray.Run(a.onTrayReady, nil)
	}()
}

func (a *App) stopTray() { systray.Quit() }

func (a *App) onTrayReady() {
	if runtime.GOOS == "windows" {
		systray.SetIcon(trayICO)
	} else {
		systray.SetIcon(trayPNG)
	}
	systray.SetTitle("rmt-desk")
	systray.SetTooltip("rmt-desk")
	systray.SetOnTapped(a.showWindow) // left click opens the window; right click shows the menu

	open := systray.AddMenuItem("Open rmt-desk", "Show the rmt-desk window")
	systray.AddSeparator()
	status := systray.AddMenuItem("Remote access: off", "")
	status.Disable()
	disconnect := systray.AddMenuItem("Disconnect remote session", "End every session controlling this computer")
	disconnect.Hide()
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit rmt-desk", "Close rmt-desk (remote access, if on, keeps running)")

	update := func() {
		if !a.Info().HostSupported {
			status.Hide()
			return
		}
		v := a.HostStatus()
		text, tip := "Remote access: off", "rmt-desk"
		var sessions int
		switch {
		case v.Service == hostsetup.NotInstalled:
		case v.Status == nil:
			text = "Remote access: starting…"
		default:
			text = "Remote access: on"
			if !v.Status.Online {
				text = "Remote access: on (offline)"
			}
			if sessions = len(v.Status.Sessions); sessions > 0 {
				who := v.Status.Sessions[0].Peer
				if sessions > 1 {
					who = fmt.Sprintf("%s and %d more", who, sessions-1)
				}
				text = "Being controlled by " + who
				tip = "rmt-desk: this computer is being controlled by " + who
			}
		}
		status.SetTitle(text)
		systray.SetTooltip(tip)
		if sessions > 0 {
			disconnect.Show()
		} else {
			disconnect.Hide()
		}
	}
	update()

	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-open.ClickedCh:
			a.showWindow()
		case <-disconnect.ClickedCh:
			_, _ = a.DisconnectSessions()
			update()
		case <-quit.ClickedCh:
			wruntime.Quit(a.ctx)
			return
		case <-t.C:
			update()
		}
	}
}

func (a *App) showWindow() {
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
}
