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
	"syscall"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/audit"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/codec/h264"
	"rmt-desk/internal/config"
	"rmt-desk/internal/helper"
	"rmt-desk/internal/host"
	"rmt-desk/internal/hostctl"
	"rmt-desk/internal/hostsetup"
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

Video:
  rmtd codec install           download Cisco's OpenH264 (H.264 video; much lower bandwidth)
  rmtd codec status|remove

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

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("rmtd: ")
	dir := stateDir()
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, dir)
		os.Exit(2)
	}
	cfgPath := hostsetup.ConfigPath(dir)
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp(dir, cfgPath, args)
	case "run":
		err = cmdRun(dir, cfgPath)
	case "helper":
		err = cmdHelper(args)
	case "codec":
		err = cmdCodec(dir, args)
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
	case "version":
		fmt.Println("rmtd", version)
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

func stateDir() string           { return hostsetup.StateDir() }
func ensureDir(dir string) error { return hostsetup.EnsureDir(dir) }
func codecDir(dir string) string { return hostsetup.CodecDir(dir) }

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

	// Local status/kill-switch channel for the rmt-desk app.
	if cl, err := hostctl.Listen(dir); err != nil {
		log.Printf("hostctl unavailable: %v", err)
	} else {
		go hostctl.Serve(ctx, cl, &ctlHandler{cfg: cfg, node: node, srv: srv})
	}
	return srv.Serve(ctx, ln)
}

type ctlHandler struct {
	cfg  config.Host
	node *tsnode.Node
	srv  *host.Server
}

func (h *ctlHandler) Status(ctx context.Context) hostctl.Status {
	st := hostctl.Status{
		Version:     version,
		HostName:    h.cfg.Hostname,
		Mode:        string(h.cfg.Access.EffectiveMode()),
		PasswordSet: h.cfg.Access.PasswordHash != "",
		TOTPSet:     h.cfg.Access.TOTPSecret != "",
		Allowed:     append(slices.Clone(h.cfg.Policy.AllowedLogins), h.cfg.Policy.AllowedTags...),
		Sessions:    h.srv.Sessions(),
	}
	if ns, err := h.node.State(ctx); err == nil {
		st.TailnetName, st.IP, st.Online = ns.Name, ns.IP, ns.Running
	}
	return st
}

func (h *ctlHandler) DisconnectAll() int {
	n := h.srv.DisconnectAll()
	if n > 0 {
		log.Printf("host user disconnected %d session(s)", n)
	}
	return n
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
func cmdHelper(args []string) error {
	fs := flag.NewFlagSet("helper", flag.ExitOnError)
	codecs := fs.String("codecs", "", "codec library dir")
	fs.Parse(args)
	log.SetOutput(os.Stderr)
	log.SetPrefix("rmtd-helper: ")
	desk := inProcessDesktop(*codecs)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("started (pid %d)", os.Getpid())
	handlers := map[string]helper.Handler{proto.StreamConsent: helper.ConsentHandler(prompt.Ask)}
	for kind, h := range desktopHandlers(desk) {
		handlers[kind] = func(ctx context.Context, c net.Conn) { h(ctx, authz.Identity{}, c) }
	}
	return helper.Serve(ctx, helper.StdioConn{R: os.Stdin, W: os.Stdout}, handlers)
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

func newService(dir string) (service.Service, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return hostsetup.NewService(dir, exe, &program{dir: dir, cfgPath: hostsetup.ConfigPath(dir)})
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
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := hostsetup.Control(dir, exe, action); err != nil {
		return err
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
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("service:        %s\n", hostsetup.ServiceState(dir, exe))
	}
	return nil
}

func edit(dir, _ string, f func(*config.Host) error) error { return hostsetup.Edit(dir, f) }

func cmdEdit(dir, cfgPath, op, who string) error {
	err := edit(dir, cfgPath, func(cfg *config.Host) error {
		if op == "allow" {
			return hostsetup.Allow(cfg, who)
		}
		hostsetup.Revoke(cfg, who)
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
			hostsetup.ClearPassword(c)
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
	return edit(dir, cfgPath, func(c *config.Host) error {
		if err := hostsetup.SetPassword(c, pw); err != nil {
			return err
		}
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
		sec, uri, err := hostsetup.NewTOTP(c)
		if err != nil {
			return fmt.Errorf("%w: rmtd passwd", err)
		}
		fmt.Println("Add this to your authenticator app (it is shown only once):")
		fmt.Println()
		fmt.Println("  " + uri)
		fmt.Println()
		fmt.Println("  or enter the secret manually: " + sec)
		fmt.Println()
		code, err := readSecret("enter the 6-digit code from the app to confirm: ")
		if err != nil {
			return err
		}
		if err := hostsetup.EnableTOTP(c, sec, code); err != nil {
			return fmt.Errorf("TOTP not enabled: %w", err)
		}
		fmt.Println("TOTP enabled (restart rmtd to apply)")
		return nil
	})
}

func cmdMode(dir, cfgPath string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rmtd mode attended|unattended|both")
	}
	m := access.Mode(args[0])
	return edit(dir, cfgPath, func(c *config.Host) error {
		if err := hostsetup.SetMode(c, m); err != nil {
			return err
		}
		if m != access.Attended && c.Access.PasswordHash == "" {
			fmt.Println("note: no password set, so the host stays attended-only until you run 'rmtd passwd'")
		}
		fmt.Printf("mode: %s (restart rmtd to apply)\n", m)
		return nil
	})
}

func cmdCodec(dir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rmtd codec install|status|remove")
	}
	cdir := codecDir(dir)
	p, err := h264.LibPath(cdir)
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return err
		}
		if err := tsnode.SecureSharedDir(cdir); err != nil {
			return err
		}
		fmt.Println("Downloading OpenH264 from Cisco (ciscobinary.openh264.org)…")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		path, err := h264.Install(ctx, cdir)
		if err != nil {
			return err
		}
		fmt.Printf("installed and verified: %s\n%s\nrestart rmtd to use it.\n", path, h264.Notice)
	case "status":
		if _, err := os.Stat(p); err != nil {
			fmt.Println("H.264: not installed (video uses JPEG tiles). Run: rmtd codec install")
			return nil
		}
		fmt.Printf("H.264: installed at %s\n%s\n", p, h264.Notice)
	case "remove":
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Println("removed; video will use JPEG tiles after restart")
	default:
		return errors.New("usage: rmtd codec install|status|remove")
	}
	return nil
}
