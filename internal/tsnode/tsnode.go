// Package tsnode embeds a Tailscale node (tsnet) into the rmt binaries.
package tsnode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"rmt-desk/internal/authz"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
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

	quiet bool // don't log the login URL (Open: the caller shows State)
}

type Node struct {
	Srv *tsnet.Server
	LC  *local.Client

	stopWatch func()
	mu        sync.Mutex
	lastErr   string
}

// Start brings the node up and waits until it is connected to the tailnet.
// Without an auth key and saved state, it logs the login URL and waits for
// someone to open it (CLI use; the GUI uses Open instead).
func Start(ctx context.Context, o Options) (*Node, error) {
	srv, err := newServer(o)
	if err != nil {
		return nil, err
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

// Open starts the node without waiting for login. Progress (login URL,
// errors such as a refused tag) is reported by State; WaitRunning blocks
// until the node is connected.
func Open(o Options) (*Node, error) {
	o.quiet = true
	srv, err := newServer(o)
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		srv.Close()
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	lc, err := srv.LocalClient()
	if err != nil {
		srv.Close()
		return nil, err
	}
	n := &Node{Srv: srv, LC: lc}
	ctx, cancel := context.WithCancel(context.Background())
	n.stopWatch = cancel
	go n.watch(ctx)
	return n, nil
}

func newServer(o Options) (*tsnet.Server, error) {
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
	if o.quiet {
		srv.UserLogf = func(string, ...any) {} // State reports the URL instead
	}
	if !o.Verbose {
		srv.Logf = func(string, ...any) {}
	}
	return srv, nil
}

// State is a node's login/connection progress, for display.
type State struct {
	Backend   string // ipn state: NoState, NeedsLogin, Starting, Running, ...
	Running   bool
	AuthURL   string // open this to log in (NeedsLogin)
	Error     string // last critical error from the control plane
	TagDenied bool   // Error says our advertised tag isn't permitted
	Name      string // this node's MagicDNS name
	IP        string
	LoginName string // user the node is logged in as ("" for tagged nodes)
}

// State reports the node's current progress.
func (n *Node) State(ctx context.Context) (State, error) {
	st, err := n.LC.StatusWithoutPeers(ctx)
	if err != nil {
		return State{}, err
	}
	out := State{Backend: st.BackendState, Running: st.BackendState == ipn.Running.String(), AuthURL: st.AuthURL}
	n.mu.Lock()
	out.Error = n.lastErr
	n.mu.Unlock()
	if out.Running {
		out.Error = ""
	}
	out.TagDenied = IsTagError(out.Error)
	if st.Self != nil {
		out.Name = strings.TrimSuffix(st.Self.DNSName, ".")
		if len(st.Self.TailscaleIPs) > 0 {
			out.IP = st.Self.TailscaleIPs[0].String()
		}
		if u, ok := st.User[st.Self.UserID]; ok && (st.Self.Tags == nil || st.Self.Tags.Len() == 0) {
			out.LoginName = u.LoginName
		}
	}
	return out, nil
}

// WaitRunning blocks until the node is connected, ctx is done, or the
// control plane reports a critical error (returned).
func (n *Node) WaitRunning(ctx context.Context) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		st, err := n.State(ctx)
		if err == nil {
			if st.Running {
				return nil
			}
			if st.Error != "" {
				return errors.New(st.Error)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// watch records critical errors from the IPN bus (e.g. a refused tag),
// which StatusWithoutPeers doesn't carry.
func (n *Node) watch(ctx context.Context) {
	for ctx.Err() == nil {
		w, err := n.LC.WatchIPNBus(ctx, ipn.NotifyInitialState)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		for {
			nt, err := w.Next()
			if err != nil {
				break
			}
			n.mu.Lock()
			if nt.ErrMessage != nil {
				n.lastErr = *nt.ErrMessage
			}
			if nt.LoginFinished != nil || (nt.State != nil && *nt.State == ipn.Running) {
				n.lastErr = ""
			}
			n.mu.Unlock()
		}
		w.Close()
	}
}

// IsTagError reports whether a control-plane error means the advertised
// ACL tag isn't permitted for the logging-in user (missing tagOwners).
func IsTagError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "tag") && (strings.Contains(m, "not permitted") || strings.Contains(m, "invalid"))
}

// TagPolicySnippet is what to add to the tailnet policy file so that
// members can create hosts tagged tag:rmt-host.
const TagPolicySnippet = `"tagOwners": {
  "tag:rmt-host": ["autogroup:admin"],
},`

func (n *Node) Close() error {
	if n.stopWatch != nil {
		n.stopWatch()
	}
	return n.Srv.Close()
}

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
