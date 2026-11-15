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
	"slices"
	"strconv"
	"syscall"

	"rmt-desk/internal/audit"
	"rmt-desk/internal/authz"
	"rmt-desk/internal/config"
	"rmt-desk/internal/host"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"
)

const usage = `rmtd - rmt-desk host agent

Usage:
  rmtd up                     join the tailnet and serve sessions
  rmtd status                 show config and allowlist
  rmtd allow <login|tag:x>    allowlist a tailnet user login or ACL tag
  rmtd revoke <login|tag:x>   remove from allowlist

Environment:
  TS_AUTHKEY   Tailscale auth key (first run only; use a tagged, pre-approved key)
  RMTD_DIR     state/config dir (default: user config dir/rmt-desk/host)
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("rmtd: ")
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	dir := os.Getenv("RMTD_DIR")
	if dir == "" {
		dir = tsnode.DefaultStateDir("host")
	}
	cfgPath := filepath.Join(dir, "rmtd.toml")

	var err error
	switch os.Args[1] {
	case "up":
		err = cmdUp(dir, cfgPath, os.Args[2:])
	case "status":
		err = cmdStatus(cfgPath)
	case "allow", "revoke":
		if len(os.Args) != 3 {
			err = errors.New("usage: rmtd " + os.Args[1] + " <login|tag:name>")
		} else {
			err = cmdEdit(cfgPath, os.Args[1], os.Args[2])
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdUp(dir, cfgPath string, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose tsnet logs")
	fs.Parse(args)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, err := tsnode.Start(ctx, tsnode.Options{
		Hostname: cfg.Hostname,
		StateDir: filepath.Join(dir, "tsnet"),
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Tags:     cfg.Tags,
		Verbose:  *verbose,
	})
	if err != nil {
		return err
	}
	defer node.Close()

	alog, err := audit.Open(filepath.Join(dir, "audit.log"))
	if err != nil {
		return err
	}
	defer alog.Close()

	// Listen ONLY on the tailnet interface provided by tsnet.
	ln, err := node.Srv.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return err
	}
	log.Printf("serving on tailnet as %q port %d", cfg.Hostname, cfg.Port)

	srv := &host.Server{
		Policy:   cfg.Policy,
		Resolver: node,
		Audit:    alog,
		HostName: cfg.Hostname,
		Handlers: map[string]host.StreamHandler{
			proto.StreamControl: func(_ context.Context, _ authz.Identity, c net.Conn) {
				// Phase 1: control stream echoes (used by `rmt ping`).
				defer c.Close()
				io.Copy(c, c)
			},
		},
	}
	return srv.Serve(ctx, ln)
}

func cmdStatus(cfgPath string) error {
	cfg, err := config.LoadHost(cfgPath)
	if err != nil {
		return err
	}
	fmt.Printf("config:   %s\nhostname: %s\nport:     %d\ntags:     %v\n", cfgPath, cfg.Hostname, cfg.Port, cfg.Tags)
	fmt.Printf("allowed logins: %v\nallowed tags:   %v\n", cfg.Policy.AllowedLogins, cfg.Policy.AllowedTags)
	return nil
}

func cmdEdit(cfgPath, op, who string) error {
	cfg, err := config.LoadHost(cfgPath)
	if err != nil {
		return err
	}
	list := &cfg.Policy.AllowedLogins
	if len(who) > 4 && who[:4] == "tag:" {
		list = &cfg.Policy.AllowedTags
	}
	if op == "allow" {
		if !slices.Contains(*list, who) {
			*list = append(*list, who)
		}
	} else {
		*list = slices.DeleteFunc(*list, func(s string) bool { return s == who })
	}
	if err := config.SaveHost(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("%s: %s (restart rmtd to apply)\n", op, who)
	return nil
}
