// Package hostsetup provisions this machine as an rmt-desk host: state
// directory, tailnet login, configuration edits and the background service.
// It is shared by the rmtd CLI and the rmt-desk app's setup wizard.
package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/config"
	"rmt-desk/internal/tsnode"

	"github.com/kardianos/service"
)

// ServiceName is the OS service / agent name.
const ServiceName = "rmtd"

// StateDir: Windows services run as LocalSystem, so state lives in
// %ProgramData% (locked down to SYSTEM+Administrators by SecureDir).
// Elsewhere rmtd runs as the desktop user. RMTD_DIR overrides.
func StateDir() string {
	if d := os.Getenv("RMTD_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "rmt-desk", "host")
	}
	return tsnode.DefaultStateDir("host")
}

// ConfigPath is the host config file inside dir.
func ConfigPath(dir string) string { return filepath.Join(dir, "rmtd.toml") }

// TSNetDir is the host node's tailnet state inside dir.
func TSNetDir(dir string) string { return filepath.Join(dir, "tsnet") }

// CodecDir holds the OpenH264 library. On Windows it sits outside the
// admin-only state dir (the per-user helper must read it) with a DACL that
// lets users read but only admins write; h264.Load also re-verifies its hash.
func CodecDir(stateDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(filepath.Dir(stateDir), "codecs")
	}
	return filepath.Join(stateDir, "codecs")
}

// EnsureDir creates dir restricted to the service's account.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return tsnode.SecureDir(dir)
}

// RmtdPath is the rmtd executable shipped next to the running program
// (the app bundle / install dir). On macOS the app keeps it in
// Contents/MacOS beside the GUI binary.
func RmtdPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "rmtd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("rmtd not found next to %s", exe)
	}
	return p, nil
}

// ---------- configuration ----------

// Load reads the host config (defaults if absent).
func Load(dir string) (config.Host, error) { return config.LoadHost(ConfigPath(dir)) }

// Edit applies f to the host config and saves it atomically.
func Edit(dir string, f func(*config.Host) error) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	cfg, err := config.LoadHost(ConfigPath(dir))
	if err != nil {
		return err
	}
	if err := f(&cfg); err != nil {
		return err
	}
	return config.SaveHost(ConfigPath(dir), cfg)
}

// Allow adds a tailnet login or tag:name to the allowlist.
func Allow(c *config.Host, who string) error {
	who = strings.TrimSpace(who)
	if who == "" || strings.ContainsAny(who, " \t\r\n") {
		return fmt.Errorf("invalid login or tag %q", who)
	}
	list := &c.Policy.AllowedLogins
	if strings.HasPrefix(who, "tag:") {
		list = &c.Policy.AllowedTags
	}
	if !slices.Contains(*list, who) {
		*list = append(*list, who)
	}
	return nil
}

// Revoke removes who from the allowlist.
func Revoke(c *config.Host, who string) {
	del := func(s string) bool { return s == who }
	c.Policy.AllowedLogins = slices.DeleteFunc(c.Policy.AllowedLogins, del)
	c.Policy.AllowedTags = slices.DeleteFunc(c.Policy.AllowedTags, del)
}

// SetPassword sets the unattended-access password.
func SetPassword(c *config.Host, pw string) error {
	h, err := access.HashPassword(pw)
	if err != nil {
		return err
	}
	c.Access.PasswordHash = h
	return nil
}

// ClearPassword removes the password and TOTP (attended-only).
func ClearPassword(c *config.Host) { c.Access.PasswordHash, c.Access.TOTPSecret = "", "" }

// SetMode validates and sets the access mode.
func SetMode(c *config.Host, m access.Mode) error {
	if m != access.Attended && m != access.Unattended && m != access.Both {
		return fmt.Errorf("unknown mode %q", m)
	}
	c.Access.Mode = m
	return nil
}

// NewTOTP returns a fresh secret and its otpauth:// URI for c's host. It is
// not enabled until EnableTOTP confirms a code from the user's app.
func NewTOTP(c *config.Host) (secret, uri string, err error) {
	if c.Access.PasswordHash == "" {
		return "", "", errors.New("set a password first")
	}
	secret, err = access.NewTOTPSecret()
	if err != nil {
		return "", "", err
	}
	return secret, access.TOTPURI(secret, c.Hostname), nil
}

// EnableTOTP enables secret if code is currently valid for it.
func EnableTOTP(c *config.Host, secret, code string) error {
	if c.Access.PasswordHash == "" {
		return errors.New("set a password first")
	}
	if _, ok := access.VerifyTOTP(secret, strings.TrimSpace(code), time.Now()); !ok {
		return errors.New("code did not match (check the device clock)")
	}
	c.Access.TOTPSecret = secret
	return nil
}

// ---------- tailnet login ----------

// LoggedIn reports whether the host node has tailnet state.
func LoggedIn(dir string) bool {
	_, err := os.Stat(filepath.Join(TSNetDir(dir), "tailscaled.state"))
	return err == nil
}

// Login brings the host node up once (interactive login: onState receives
// the login URL to show), waits until it is connected, then shuts it down so
// the service can take over the state. The service must not be running.
func Login(ctx context.Context, dir string, onState func(tsnode.State)) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	cfg, err := Load(dir)
	if err != nil {
		return err
	}
	if err := config.SaveHost(ConfigPath(dir), cfg); err != nil { // persist defaults
		return err
	}
	node, err := tsnode.Open(tsnode.Options{
		Hostname: cfg.Hostname,
		StateDir: TSNetDir(dir),
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Tags:     cfg.Tags,
	})
	if err != nil {
		return err
	}
	defer node.Close()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last tsnode.State
	for {
		st, err := node.State(ctx)
		if err == nil {
			if st != last && onState != nil {
				onState(st)
			}
			last = st
			if st.Running {
				return nil
			}
			if st.TagDenied {
				return &TagError{Msg: st.Error}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// TagError: the tailnet refused tag:rmt-host for the user who logged in.
type TagError struct{ Msg string }

func (e *TagError) Error() string {
	return "the tailnet refused tag:rmt-host (" + e.Msg + "). Add this to your tailnet policy file:\n" + tsnode.TagPolicySnippet
}

// ---------- service ----------

// systemd user unit bound to the graphical session (so DISPLAY etc. exist).
// Syntax is kardianos/service's mini template engine.
const systemdUserUnit = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
Restart=always
RestartSec=5
{{range EnvVars}}{{.}}
{{end}}
[Install]
WantedBy=graphical-session.target
`

// NewService describes the rmtd service that runs exe ("rmtd run") with
// state in dir. prg is the service body (rmtd's runner); callers that only
// control the service may pass nil.
func NewService(dir, exe string, prg service.Interface) (service.Service, error) {
	if prg == nil {
		prg = noop{}
	}
	c := &service.Config{
		Name:        ServiceName,
		DisplayName: "rmt-desk host agent",
		Description: "Remote desktop over Tailscale (rmt-desk).",
		Executable:  exe,
		Arguments:   []string{"run"},
		EnvVars:     map[string]string{"RMTD_DIR": dir},
		Option:      service.KeyValue{},
	}
	switch runtime.GOOS {
	case "windows":
		c.Option["OnFailure"] = "restart"
		c.Option["OnFailureDelayDuration"] = "5s"
		c.Option["DelayedAutoStart"] = false
	case "darwin":
		c.Option["UserService"] = true // LaunchAgent: needs the user's GUI session
		c.Option["RunAtLoad"] = true
		c.Option["KeepAlive"] = true
	default:
		c.Option["UserService"] = true
		c.Option["SystemdScript"] = systemdUserUnit
	}
	return service.New(prg, c)
}

type noop struct{}

func (noop) Start(service.Service) error { return nil }
func (noop) Stop(service.Service) error  { return nil }

// Service states reported by ServiceState.
const (
	NotInstalled = "not installed"
	Running      = "running"
	Stopped      = "stopped"
)

// ServiceState reports whether the rmtd service is installed and running.
func ServiceState(dir, exe string) string {
	s, err := NewService(dir, exe, nil)
	if err != nil {
		return NotInstalled
	}
	switch st, err := s.Status(); {
	case err != nil:
		return NotInstalled
	case st == service.StatusRunning:
		return Running
	default:
		return Stopped
	}
}

// Control runs install/uninstall/start/stop/restart on the rmtd service.
func Control(dir, exe, action string) error {
	if action == "install" && !LoggedIn(dir) {
		return errors.New("not logged in to the tailnet yet: run 'rmtd up' once (with TS_AUTHKEY or the login URL), then install")
	}
	s, err := NewService(dir, exe, nil)
	if err != nil {
		return err
	}
	if err := service.Control(s, action); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

// Enable installs (if needed) and (re)starts the service.
func Enable(dir, exe string) error {
	switch ServiceState(dir, exe) {
	case NotInstalled:
		if err := Control(dir, exe, "install"); err != nil {
			return err
		}
		return Control(dir, exe, "start")
	case Running:
		return Control(dir, exe, "restart")
	default:
		return Control(dir, exe, "start")
	}
}

// Disable stops and uninstalls the service, keeping config and login.
func Disable(dir, exe string) error {
	if ServiceState(dir, exe) == NotInstalled {
		return nil
	}
	_ = Control(dir, exe, "stop")
	return Control(dir, exe, "uninstall")
}
