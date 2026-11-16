// rmtd is the rmt-desk host agent. It joins your tailnet as its own node and
// accepts remote-desktop sessions from allowlisted tailnet identities only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/audit"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/config"
	"rmt-desk/internal/helper"
	"rmt-desk/internal/host"
	"rmt-desk/internal/prompt"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"

	"github.com/kardianos/service"
	"golang.org/x/term"
)

const usage = `rmtd - rmt-desk host agent

Run:
  rmtd up [-v]                 run in the foreground (first run: log in to the tailnet)
  rmtd install                 install as a service (Windows: system service;
                               macOS/Linux: per-user agent in the graphical session)
  rmtd start|stop|restart      control the installed service
  rmtd uninstall               remove the service

Access:
  rmtd status                  show configuration
  rmtd allow  <login|tag:x>    allowlist a tailnet user login or ACL tag
  rmtd revoke <login|tag:x>    remove from allowlist
  rmtd passwd [-clear]         set (or remove) the unattended-access password
  rmtd totp enable|disable     require an authenticator code with the password
  rmtd mode attended|unattended|both
                               attended:   host user must click Allow
                               unattended: password (+code) required, no prompt
                               both:       password admits; otherwise prompt (default)

Changes take effect after 'rmtd restart' (or restarting 'rmtd up').

Environment:
  TS_AUTHKEY   Tailscale auth key for the first login (tagged, pre-approved, single-use)
  RMTD_DIR     state/config dir (default: ` + "%s" + `)
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("rmtd: ")
	dir := stateDir()
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, dir)
		os.Exit(2)
	}
	cfgPath := filepath.Join(dir, "rmtd.toml")
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp(dir, cfgPath, args)
	case "run":
		err = cmdRun(dir, cfgPath)
	case "helper":
		err = cmdHelper()
	case "install", "uninstall", "start", "stop", "restart":
		err = cmdService(dir, cmd)
	case "status":
		err = cmdStatus(dir, cfgPath)
	case "allow", "revoke":
		if len(args) != 1 {
			err = errors.New("usage: rmtd " + cmd + " <login|tag:name>")
		} else {
			err = cmdEdit(dir, cfgPath, cmd, args[0])
		}
	case "passwd":
		err = cmdPasswd(dir, cfgPath, args)
	case "totp":
		err = cmdTOTP(dir, cfgPath, args)
	case "mode":
		err = cmdMode(dir, cfgPath, args)
	case "-h", "--help", "help":
		fmt.Fprintf(os.Stderr, usage, dir)
	default:
		fmt.Fprintf(os.Stderr, usage, dir)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// stateDir: Windows services run as LocalSystem, so state lives in
// %ProgramData% (locked down to SYSTEM+Administrators by SecureDir).
// Elsewhere rmtd runs as the desktop user.
func stateDir() string {
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

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return tsnode.SecureDir(dir)
}

// ---------- running ----------

func cmdUp(dir, cfgPath string, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose tsnet logs")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, dir, cfgPath, *verbose, false)
}

// serve runs the agent until ctx is done. serviceMode selects the session
// helper split on Windows.
func serve(ctx context.Context, dir, cfgPath string, verbose, serviceMode bool) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	cfg, err := config.LoadHost(cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Policy.AllowedLogins) == 0 && len(cfg.Policy.AllowedTags) == 0 {
		log.Printf("WARNING: allowlist is empty; all connections will be denied. Run: rmtd allow <you@example.com>")
	}
	if err := config.SaveHost(cfgPath, cfg); err != nil { // persist defaults
		return err
	}
	log.Printf("access mode: %s (password set: %v, TOTP: %v)",
		cfg.Access.EffectiveMode(), cfg.Access.PasswordHash != "", cfg.Access.TOTPSecret != "")

	be, err := newBackend(dir, serviceMode)
	if err != nil {
		return err
	}
	defer be.Close()

	alog, err := audit.Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		return err
	}
	defer alog.Close()

	node, err := tsnode.Start(ctx, tsnode.Options{
		Hostname: cfg.Hostname,
		StateDir: filepath.Join(dir, "tsnet"),
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Tags:     cfg.Tags,
		Verbose:  verbose,
	})
	if err != nil {
		return err
	}
	defer node.Close()

	// Listen ONLY on the tailnet interface provided by tsnet.
	ln, err := node.Srv.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	log.Printf("serving on tailnet as %q port %d", cfg.Hostname, cfg.Port)

	handlers := map[string]host.StreamHandler{
		proto.StreamControl: func(_ context.Context, _ authz.Identity, c net.Conn) {
			// Control stream echoes for now (used by `rmt ping`).
			defer c.Close()
			io.Copy(c, c)
		},
	}
	for k, h := range be.Handlers {
		handlers[k] = h
	}
	srv := &host.Server{
		Policy:   cfg.Policy,
		Gate:     &access.Gate{Config: cfg.Access, Consent: be.Consent},
		Resolver: node,
		Audit:    alog,
		HostName: cfg.Hostname,
		Handlers: handlers,
	}
	return srv.Serve(ctx, ln)
}

// backend is where screen/input/consent actually happen: in-process, or in
// a session helper (Windows service mode).
type backend struct {
	Handlers map[string]host.StreamHandler
	Consent  access.ConsentFunc
	Close    func()
}

// cmdHelper runs inside the interactive session, spawned by the service.
// stdout is the IPC channel: nothing else may write to it.
func cmdHelper() error {
	log.SetOutput(os.Stderr)
	log.SetPrefix("rmtd-helper: ")
	desk := inProcessDesktop()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("started (pid %d)", os.Getpid())
	return helper.Serve(ctx, helper.StdioConn{R: os.Stdin, W: os.Stdout}, map[string]helper.Handler{
		proto.StreamVideo:   func(ctx context.Context, c net.Conn) { desk.Video(ctx, authz.Identity{}, c) },
		proto.StreamInput:   func(ctx context.Context, c net.Conn) { desk.Input(ctx, authz.Identity{}, c) },
		proto.StreamConsent: helper.ConsentHandler(prompt.Ask),
	})
}

// ---------- service ----------

type program struct {
	dir, cfgPath string
	cancel       context.CancelFunc
	done         chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		if err := serve(ctx, p.dir, p.cfgPath, false, !service.Interactive()); err != nil {
			log.Printf("fatal: %v", err)
			os.Exit(1) // let the service manager restart us
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// systemd user unit bound to the graphical session (so DISPLAY etc. exist).
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

func newService(dir string) (service.Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	c := &service.Config{
		Name:        "rmtd",
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
	return service.New(&program{dir: dir, cfgPath: filepath.Join(dir, "rmtd.toml")}, c)
}

// cmdRun is the service entrypoint.
func cmdRun(dir, cfgPath string) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	if !service.Interactive() {
		f, err := openLog(filepath.Join(dir, "rmtd.log"))
		if err != nil {
			return err
		}
		log.SetOutput(f)
	}
	s, err := newService(dir)
	if err != nil {
		return err
	}
	return s.Run()
}

func openLog(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func cmdService(dir, action string) error {
	s, err := newService(dir)
	if err != nil {
		return err
	}
	if action == "install" {
		if _, err := os.Stat(filepath.Join(dir, "tsnet", "tailscaled.state")); err != nil {
			return errors.New("not logged in to the tailnet yet: run 'rmtd up' once (with TS_AUTHKEY or the login URL), then install")
		}
	}
	if err := service.Control(s, action); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	fmt.Printf("rmtd: %s ok\n", action)
	if action == "install" {
		fmt.Println("now run: rmtd start")
		if runtime.GOOS == "darwin" {
			fmt.Println("macOS: grant Screen Recording and Accessibility to this binary in System Settings → Privacy & Security.")
		}
	}
	return nil
}

// ---------- configuration ----------

func cmdStatus(dir, cfgPath string) error {
	cfg, err := config.LoadHost(cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("state dir:      %s\nhostname:       %s\nport:           %d\ntags:           %v\n",
		dir, cfg.Hostname, cfg.Port, cfg.Tags)
	fmt.Printf("allowed logins: %v\nallowed tags:   %v\n", cfg.Policy.AllowedLogins, cfg.Policy.AllowedTags)
	fmt.Printf("access mode:    %s (configured: %q)\npassword:       %v\nTOTP:           %v\npermissions:    %v\n",
		cfg.Access.EffectiveMode(), cfg.Access.Mode, cfg.Access.PasswordHash != "", cfg.Access.TOTPSecret != "", cfg.Access.Permissions)
	if s, err := newService(dir); err == nil {
		st, err := s.Status()
		names := map[service.Status]string{service.StatusRunning: "running", service.StatusStopped: "stopped"}
		if err != nil {
			fmt.Printf("service:        not installed\n")
		} else if n, ok := names[st]; ok {
			fmt.Printf("service:        %s\n", n)
		}
	}
	return nil
}

func edit(dir, cfgPath string, f func(*config.Host) error) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	cfg, err := config.LoadHost(cfgPath)
	if err != nil {
		return err
	}
	if err := f(&cfg); err != nil {
		return err
	}
	return config.SaveHost(cfgPath, cfg)
}

func cmdEdit(dir, cfgPath, op, who string) error {
	err := edit(dir, cfgPath, func(cfg *config.Host) error {
		list := &cfg.Policy.AllowedLogins
		if strings.HasPrefix(who, "tag:") {
			list = &cfg.Policy.AllowedTags
		}
		if op == "allow" {
			if !slices.Contains(*list, who) {
				*list = append(*list, who)
			}
		} else {
			*list = slices.DeleteFunc(*list, func(s string) bool { return s == who })
		}
		return nil
	})
	if err == nil {
		fmt.Printf("%s: %s (restart rmtd to apply)\n", op, who)
	}
	return err
}

func readSecret(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func cmdPasswd(dir, cfgPath string, args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	clear := fs.Bool("clear", false, "remove the password (disables unattended access)")
	fs.Parse(args)
	if *clear {
		return edit(dir, cfgPath, func(c *config.Host) error {
			c.Access.PasswordHash, c.Access.TOTPSecret = "", ""
			fmt.Println("password and TOTP removed; host is now attended-only (restart rmtd to apply)")
			return nil
		})
	}
	pw, err := readSecret("new unattended-access password: ")
	if err != nil {
		return err
	}
	again, err := readSecret("again: ")
	if err != nil {
		return err
	}
	if pw != again {
		return errors.New("passwords do not match")
	}
	h, err := access.HashPassword(pw)
	if err != nil {
		return err
	}
	return edit(dir, cfgPath, func(c *config.Host) error {
		c.Access.PasswordHash = h
		fmt.Printf("password set; access mode is now %q (restart rmtd to apply)\n", c.Access.EffectiveMode())
		return nil
	})
}

func cmdTOTP(dir, cfgPath string, args []string) error {
	if len(args) != 1 || (args[0] != "enable" && args[0] != "disable") {
		return errors.New("usage: rmtd totp enable|disable")
	}
	if args[0] == "disable" {
		return edit(dir, cfgPath, func(c *config.Host) error {
			c.Access.TOTPSecret = ""
			fmt.Println("TOTP disabled (restart rmtd to apply)")
			return nil
		})
	}
	return edit(dir, cfgPath, func(c *config.Host) error {
		if c.Access.PasswordHash == "" {
			return errors.New("set a password first: rmtd passwd")
		}
		sec, err := access.NewTOTPSecret()
		if err != nil {
			return err
		}
		fmt.Println("Add this to your authenticator app (it is shown only once):")
		fmt.Println()
		fmt.Println("  " + access.TOTPURI(sec, c.Hostname))
		fmt.Println()
		fmt.Println("  or enter the secret manually: " + sec)
		fmt.Println()
		code, err := readSecret("enter the 6-digit code from the app to confirm: ")
		if err != nil {
			return err
		}
		if _, ok := access.VerifyTOTP(sec, strings.TrimSpace(code), time.Now()); !ok {
			return errors.New("code did not match; TOTP not enabled (check the device clock)")
		}
		c.Access.TOTPSecret = sec
		fmt.Println("TOTP enabled (restart rmtd to apply)")
		return nil
	})
}

func cmdMode(dir, cfgPath string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rmtd mode attended|unattended|both")
	}
	m := access.Mode(args[0])
	if m != access.Attended && m != access.Unattended && m != access.Both {
		return fmt.Errorf("unknown mode %q", args[0])
	}
	return edit(dir, cfgPath, func(c *config.Host) error {
		c.Access.Mode = m
		if m != access.Attended && c.Access.PasswordHash == "" {
			fmt.Println("note: no password set, so the host stays attended-only until you run 'rmtd passwd'")
		}
		fmt.Printf("mode: %s (restart rmtd to apply)\n", m)
		return nil
	})
}
