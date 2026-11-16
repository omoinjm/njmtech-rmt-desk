// rmt-desk is the desktop app: connect to your devices, and allow remote
// access to this one. It wraps the same viewer bridge as `rmt ui` and the
// same host provisioning as `rmtd`, in a native window (Wails).
//
// Modes:
//
//	rmt-desk                       main window (viewer + "This device")
//	rmt-desk --host-setup [--allow=login]
//	                               host setup wizard; on Windows the main
//	                               window relaunches itself elevated with this
//	rmt-desk --host-disable        stop and remove the host service, no window
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"rmt-desk/internal/hostsetup"
	"rmt-desk/internal/viewer/web"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	setup := flag.Bool("host-setup", false, "run the host setup wizard")
	allow := flag.String("allow", "", "login to pre-fill in the setup wizard")
	disable := flag.Bool("host-disable", false, "stop and remove the host service, then exit")
	flag.Parse()

	if *disable {
		exe, err := hostsetup.RmtdPath()
		if err == nil {
			err = hostsetup.Disable(hostsetup.StateDir(), exe)
		}
		if err != nil {
			fatalDialog("Could not turn off remote access", err)
		}
		return
	}

	app := newApp(*setup, *allow)
	opts := &options.App{
		Title:     "rmt-desk",
		Width:     1100,
		Height:    720,
		MinWidth:  420,
		MinHeight: 480,
		AssetServer: &assetserver.Options{
			Assets: web.Assets(),
		},
		BackgroundColour: &options.RGBA{R: 15, G: 17, B: 21, A: 255},
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		OnShutdown:       app.shutdown,
		Bind:             []any{app},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true, // dropped files arrive as paths (sent to the host)
			DisableWebViewDrop: true, // never let the web view navigate to a dropped file
		},
		Windows: &windows.Options{WindowClassName: windowClass},
		Mac: &mac.Options{
			About: &mac.AboutInfo{Title: "rmt-desk", Message: "Remote desktop over your Tailscale tailnet.\n" + version},
		},
		Linux: &linux.Options{ProgramName: "rmt-desk"},
	}
	if *setup {
		opts.Title = "rmt-desk: allow remote access"
		opts.Width, opts.Height = 640, 720
		// An elevated web view must not share the user's data folder
		// (WebView2 refuses mixed integrity levels).
		if d, err := os.UserCacheDir(); err == nil {
			opts.Windows.WebviewUserDataPath = filepath.Join(d, "rmt-desk", "webview-setup")
		}
	} else {
		if trayAvailable() {
			// Live in the tray: closing the window hides it (and its taskbar
			// button); the tray icon brings it back, "Quit" exits.
			opts.HideWindowOnClose = true
			app.tray = true
		}
		opts.SingleInstanceLock = &options.SingleInstanceLock{
			UniqueId:               "rmt-desk.main.4f0c2a",
			OnSecondInstanceLaunch: app.onSecondInstance,
		}
	}
	if err := wails.Run(opts); err != nil {
		log.Fatal(err)
	}
}

func fatalDialog(title string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", title, err)
	messageBox(title, err.Error())
	os.Exit(1)
}
