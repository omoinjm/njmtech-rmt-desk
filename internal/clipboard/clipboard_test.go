package clipboard

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func init() { PollInterval = 20 * time.Millisecond }

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSyncBothWaysNoLeakNoEcho(t *testing.T) {
	hostCB := &Memory{text: "host secret before connect"}
	viewCB := &Memory{text: "viewer secret before connect"}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Sync(ctx, hostCB, a, t.Logf)
	go Sync(ctx, viewCB, b, t.Logf)

	// Pre-existing contents must not cross.
	time.Sleep(150 * time.Millisecond)
	if v, _ := viewCB.Read(); v != "viewer secret before connect" {
		t.Fatalf("host clipboard leaked on connect: %q", v)
	}
	if h, _ := hostCB.Read(); h != "host secret before connect" {
		t.Fatalf("viewer clipboard leaked on connect: %q", h)
	}

	viewCB.Write("copied on laptop")
	waitFor(t, "viewer->host", func() bool { h, _ := hostCB.Read(); return h == "copied on laptop" })

	hostCB.Write("copied on host ✓")
	waitFor(t, "host->viewer", func() bool { v, _ := viewCB.Read(); return v == "copied on host ✓" })

	// Stable state: no ping-pong.
	time.Sleep(150 * time.Millisecond)
	if v, _ := viewCB.Read(); v != "copied on host ✓" {
		t.Fatalf("echo/ping-pong: viewer=%q", v)
	}
}

func TestOversizeIgnored(t *testing.T) {
	hostCB, viewCB := &Memory{}, &Memory{}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Sync(ctx, hostCB, a, t.Logf)
	go Sync(ctx, viewCB, b, t.Logf)
	viewCB.Write(strings.Repeat("x", 2<<20))
	time.Sleep(150 * time.Millisecond)
	if h, _ := hostCB.Read(); h != "" {
		t.Fatal("oversize clipboard was sent")
	}
	viewCB.Write("small")
	waitFor(t, "small after oversize", func() bool { h, _ := hostCB.Read(); return h == "small" })
}
