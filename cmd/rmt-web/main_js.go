//go:build js && wasm

// rmt-web is the viewer compiled to WebAssembly: a Tailscale node that runs
// inside the browser tab (userspace netstack; DERP relays over WebSocket,
// since a page can't send UDP), plus the same session bridge the local
// viewer uses. The page reaches it through globalThis.rmtWeb.
//
// Node setup follows tailscale.com/cmd/tsconnect/wasm (BSD-3-Clause,
// Copyright (c) Tailscale Inc & contributors), minus its log upload.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"rmt-desk/internal/viewer/bridge"

	"github.com/coder/websocket"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnserver"
	"tailscale.com/logpolicy"
	"tailscale.com/logtail"
	"tailscale.com/net/netns"
	"tailscale.com/net/tsdial"
	"tailscale.com/safesocket"
	"tailscale.com/tsd"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
)

const hostTag = "tag:rmt-host"

type node struct {
	lb     *ipnlocal.LocalBackend
	dialer *tsdial.Dialer

	mu           sync.Mutex
	loginStarted bool
}

func main() {
	n, err := newNode()
	if err != nil {
		log.Fatal(err)
	}
	js.Global().Set("rmtWeb", map[string]any{
		// state() -> Promise<{Backend, Running, AuthURL, Error, LoginName}>
		"state": js.FuncOf(func(js.Value, []js.Value) any {
			return promise(func() (any, error) { return toJS(n.state()) })
		}),
		// listHosts() -> Promise<[{HostName, DNSName, IP, OS, Online}]>
		"listHosts": js.FuncOf(func(js.Value, []js.Value) any {
			return promise(func() (any, error) {
				h, err := n.hosts()
				if err != nil {
					return nil, err
				}
				return toJS(h)
			})
		}),
		// open(host, onMessage(isBinary, data), onClose()) -> {send(text), close()}
		"open": js.FuncOf(func(_ js.Value, a []js.Value) any {
			return n.open(a[0].String(), a[1], a[2])
		}),
		"signOut": js.FuncOf(func(js.Value, []js.Value) any {
			return promise(func() (any, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return nil, n.lb.Logout(ctx, nil)
			})
		}),
	})
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("rmtweb-ready"))
	select {}
}

func newNode() (*node, error) {
	netns.SetEnabled(false)
	store := &localStore{js.Global().Get("localStorage")}
	logf := log.Printf

	sys := tsd.NewSystem()
	sys.Set(store)
	dialer := &tsdial.Dialer{Logf: logf}
	dialer.SetBus(sys.Bus.Get())
	eng, err := wgengine.NewUserspaceEngine(logf, wgengine.Config{
		Dialer:        dialer,
		SetSubsystem:  sys.Set,
		ControlKnobs:  sys.ControlKnobs(),
		HealthTracker: sys.HealthTracker.Get(),
		ExtraRootCAs:  sys.ExtraRootCAs,
		Metrics:       sys.UserMetricsRegistry(),
		EventBus:      sys.Bus.Get(),
	})
	if err != nil {
		return nil, err
	}
	sys.Set(eng)
	ns, err := netstack.Create(logf, sys.Tun.Get(), eng, sys.MagicSock.Get(), dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		return nil, err
	}
	sys.Set(ns)
	ns.ProcessLocalIPs = true
	ns.ProcessSubnets = true
	dialer.UseNetstackForIP = func(netip.Addr) bool { return true }
	dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		c, err := ns.DialContextTCP(ctx, dst)
		if err != nil {
			return nil, err // not a typed nil
		}
		return c, nil
	}
	sys.NetstackRouter.Set(true)
	sys.Tun.Get().Start()

	logid := logPolicy(store).PublicID
	srv := ipnserver.New(logf, logid, sys.Bus.Get(), sys.NetMon.Get())
	lb, err := ipnlocal.NewLocalBackend(logf, logid, sys, controlclient.LoginEphemeral)
	if err != nil {
		return nil, err
	}
	if err := ns.Start(lb); err != nil {
		return nil, err
	}
	srv.SetLocalBackend(lb)
	go func() {
		ln, err := safesocket.Listen("")
		if err != nil {
			log.Printf("safesocket.Listen: %v", err)
			return
		}
		log.Printf("ipnserver exited: %v", srv.Run(context.Background(), ln))
	}()
	go func() {
		err := lb.Start(ipn.Options{UpdatePrefs: &ipn.Prefs{
			ControlURL:  ipn.DefaultControlURL,
			WantRunning: true,
			Hostname:    "rmt-web",
		}})
		if err != nil {
			log.Printf("start: %v", err)
		}
	}()
	return &node{lb: lb, dialer: dialer}, nil
}

// state mirrors tsnode.State's JSON (what the UI's sign-in gate polls).
type state struct {
	Backend   string
	Running   bool
	AuthURL   string
	Error     string
	LoginName string
}

func (n *node) state() state {
	st := n.lb.StatusWithoutPeers()
	s := state{Backend: st.BackendState, Running: st.BackendState == ipn.Running.String(), AuthURL: st.AuthURL}
	if st.Self != nil {
		if u, ok := st.User[st.Self.UserID]; ok {
			s.LoginName = u.LoginName
		}
	}
	// Ask control for a login link once we know we need one.
	if st.BackendState == ipn.NeedsLogin.String() && st.AuthURL == "" {
		n.mu.Lock()
		start := !n.loginStarted
		n.loginStarted = true
		n.mu.Unlock()
		if start {
			go n.lb.StartLoginInteractive(context.Background())
		}
	}
	return s
}

type peer struct {
	HostName, DNSName, IP, OS string
	Online                    bool
}

func (n *node) hosts() ([]peer, error) {
	st := n.lb.Status()
	out := []peer{}
	for _, p := range st.Peer {
		if p.Tags == nil || !slices.Contains(p.Tags.AsSlice(), hostTag) {
			continue
		}
		ip := ""
		if len(p.TailscaleIPs) > 0 {
			ip = p.TailscaleIPs[0].String()
		}
		out = append(out, peer{HostName: p.HostName, DNSName: p.DNSName, IP: ip, OS: p.OS, Online: p.Online})
	}
	slices.SortFunc(out, func(a, b peer) int { return strings.Compare(a.HostName, b.HostName) })
	return out, nil
}

// open starts a bridge session to host; the page sees it as a websocket.
func (n *node) open(host string, onMessage, onClose js.Value) map[string]any {
	c := &pageConn{onMessage: onMessage, onClose: onClose, closed: make(chan struct{}), wake: make(chan struct{}, 1)}
	go func() {
		bridge.Run(context.Background(), bridge.Config{
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return n.dialer.UserDial(ctx, network, addr)
			},
			Logf: log.Printf,
		}, host, c)
		c.Close(websocket.StatusNormalClosure, "")
	}()
	return map[string]any{
		"send": js.FuncOf(func(_ js.Value, a []js.Value) any {
			c.push([]byte(a[0].String()))
			return nil
		}),
		"close": js.FuncOf(func(js.Value, []js.Value) any {
			c.Close(websocket.StatusNormalClosure, "")
			return nil
		}),
	}
}

// pageConn implements bridge.Conn over JS callbacks: page -> Go messages
// queue in order (the page's send must never block), Go -> page messages
// are delivered by calling onMessage.
type pageConn struct {
	onMessage, onClose js.Value

	mu     sync.Mutex
	queue  [][]byte
	wake   chan struct{}
	closed chan struct{}
	once   sync.Once
}

var errClosed = errors.New("closed")

func (c *pageConn) push(b []byte) {
	c.mu.Lock()
	c.queue = append(c.queue, b)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *pageConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			b := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return websocket.MessageText, b, nil
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-c.closed:
			return 0, nil, errClosed
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

func (c *pageConn) Write(_ context.Context, typ websocket.MessageType, p []byte) error {
	select {
	case <-c.closed:
		return errClosed
	default:
	}
	if typ == websocket.MessageBinary {
		u := js.Global().Get("Uint8Array").New(len(p))
		js.CopyBytesToJS(u, p)
		c.onMessage.Invoke(true, u)
	} else {
		c.onMessage.Invoke(false, string(p))
	}
	return nil
}

func (c *pageConn) Close(websocket.StatusCode, string) error {
	c.once.Do(func() {
		close(c.closed)
		c.onClose.Invoke()
	})
	return nil
}

// localStore keeps node state in localStorage, so a reload doesn't need a
// new sign-in (until control expires the ephemeral node).
type localStore struct{ ls js.Value }

const storePrefix = "rmt-web:"

func (s *localStore) ReadState(id ipn.StateKey) ([]byte, error) {
	v := s.ls.Call("getItem", storePrefix+string(id))
	if v.IsNull() || v.String() == "" {
		return nil, ipn.ErrStateNotExist
	}
	return hex.DecodeString(v.String())
}

func (s *localStore) WriteState(id ipn.StateKey, b []byte) error {
	s.ls.Call("setItem", storePrefix+string(id), hex.EncodeToString(b))
	return nil
}

func logPolicy(store ipn.StateStore) *logpolicy.Config {
	const key = "log-policy"
	if b, err := store.ReadState(key); err == nil {
		if c, err := logpolicy.ConfigFromBytes(b); err == nil {
			return c
		}
	}
	c := logpolicy.NewConfig(logtail.CollectionNode)
	_ = store.WriteState(key, c.ToBytes())
	return c
}

// toJS converts via JSON (plain objects/arrays for the page).
func toJS(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return js.Global().Get("JSON").Call("parse", string(b)), nil
}

// promise runs f on a goroutine (callbacks must not block the event loop).
func promise(f func() (any, error)) js.Value {
	return js.Global().Get("Promise").New(js.FuncOf(func(_ js.Value, a []js.Value) any {
		resolve, reject := a[0], a[1]
		go func() {
			if v, err := f(); err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
			} else {
				resolve.Invoke(v)
			}
		}()
		return nil
	}))
}
