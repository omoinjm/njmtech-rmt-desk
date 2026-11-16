// Package tsnode embeds a Tailscale node (tsnet) into the rmt binaries.
package tsnode

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"

	"rmt-desk/internal/authz"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

type Options struct {
	Hostname string   // tailnet node name, e.g. "rmt-office-pc"
	StateDir string   // persistent node state; created 0700
	AuthKey  string   // only needed on first login; read from env, never embedded
	Tags     []string // advertised ACL tags, e.g. tag:rmt-host
	Verbose  bool
	// PerUser locks StateDir to the current user instead of to
	// SYSTEM+Administrators (Windows); used by the viewer, which runs
	// unelevated.
	PerUser bool
}

type Node struct {
	Srv *tsnet.Server
	LC  *local.Client
}

// Start brings the node up and waits until it is connected to the tailnet.
func Start(ctx context.Context, o Options) (*Node, error) {
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return nil, err
	}
	// Tighten perms even if the dir pre-existed.
	secure := SecureDir
	if o.PerUser {
		secure = SecureUserDir
	}
	if err := secure(o.StateDir); err != nil {
		return nil, err
	}
	srv := &tsnet.Server{
		Hostname:      o.Hostname,
		Dir:           o.StateDir,
		AuthKey:       o.AuthKey,
		AdvertiseTags: o.Tags,
		// User-facing messages, notably the login URL when no auth key is set.
		UserLogf: log.Printf,
	}
	if !o.Verbose {
		srv.Logf = func(string, ...any) {}
	}
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, err
	}
	return &Node{Srv: srv, LC: lc}, nil
}

func (n *Node) Close() error { return n.Srv.Close() }

// Resolve implements authz.Resolver via WhoIs.
func (n *Node) Resolve(ctx context.Context, remoteAddr string) (authz.Identity, error) {
	who, err := n.LC.WhoIs(ctx, remoteAddr)
	if err != nil {
		return authz.Identity{}, err
	}
	id := authz.Identity{}
	if who.Node != nil {
		id.NodeName = who.Node.Name
		id.Tags = slices.Clone(who.Node.Tags)
	}
	if who.UserProfile != nil {
		id.Login = who.UserProfile.LoginName
	}
	return id, nil
}

// Peer is a tailnet device advertising the rmt host tag.
type Peer struct {
	HostName string
	DNSName  string
	IP       string
	OS       string
	Online   bool
}

// Hosts lists peers carrying tag.
func (n *Node) Hosts(ctx context.Context, tag string) ([]Peer, error) {
	st, err := n.LC.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, p := range st.Peer {
		if p.Tags == nil || !slices.Contains(p.Tags.AsSlice(), tag) {
			continue
		}
		ip := ""
		if len(p.TailscaleIPs) > 0 {
			ip = p.TailscaleIPs[0].String()
		}
		out = append(out, Peer{HostName: p.HostName, DNSName: p.DNSName, IP: ip, OS: p.OS, Online: p.Online})
	}
	slices.SortFunc(out, func(a, b Peer) int {
		if a.HostName < b.HostName {
			return -1
		}
		if a.HostName > b.HostName {
			return 1
		}
		return 0
	})
	return out, nil
}

// DefaultStateDir returns <user config dir>/rmt-desk/<name>.
func DefaultStateDir(name string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "rmt-desk", name)
}
