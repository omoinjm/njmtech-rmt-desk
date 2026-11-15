// rmt is the rmt-desk viewer. Phase 1 is a CLI (list/ping); the Wails GUI
// builds on the same internal/viewer package.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"text/tabwriter"
	"time"

	"rmt-desk/internal/config"
	"rmt-desk/internal/proto"
	"rmt-desk/internal/tsnode"
	"rmt-desk/internal/viewer"
)

const usage = `rmt - rmt-desk viewer

Usage:
  rmt list          list rmt hosts on your tailnet
  rmt ping <host>   connect, handshake, and measure round-trip over the session

Environment:
  TS_AUTHKEY  Tailscale auth key (first run only; or follow the login URL printed)
  RMT_DIR     state dir (default: user config dir/rmt-desk/viewer)
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	dir := os.Getenv("RMT_DIR")
	if dir == "" {
		dir = tsnode.DefaultStateDir("viewer")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	hn, _ := os.Hostname()
	node, err := tsnode.Start(ctx, tsnode.Options{
		Hostname: "rmt-viewer-" + hn,
		StateDir: dir,
		AuthKey:  os.Getenv("TS_AUTHKEY"),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer node.Close()

	switch os.Args[1] {
	case "list":
		err = cmdList(ctx, node)
	case "ping":
		if len(os.Args) != 3 {
			err = errors.New("usage: rmt ping <host>")
		} else {
			err = cmdPing(ctx, node, os.Args[2])
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdList(ctx context.Context, n *tsnode.Node) error {
	peers, err := n.Hosts(ctx, config.HostTag)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("no hosts tagged", config.HostTag, "visible (check ACLs)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tIP\tOS\tONLINE")
	for _, p := range peers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n", p.HostName, p.IP, p.OS, p.Online)
	}
	return tw.Flush()
}

func cmdPing(ctx context.Context, n *tsnode.Node, host string) error {
	addr := host + ":" + strconv.Itoa(config.DefaultPort)
	t0 := time.Now()
	s, err := viewer.Connect(ctx, n.Srv.Dial, addr, "")
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Printf("connected to %s (%s) as %s in %v\n", s.Info.Host, s.Info.OS, s.Info.Identity, time.Since(t0).Round(time.Millisecond))
	fmt.Printf("granted: %v\n", s.Info.Granted)

	st, err := s.Open(proto.StreamControl)
	if err != nil {
		return err
	}
	defer st.Close()
	buf := make([]byte, 32)
	for i := range 4 {
		t := time.Now()
		if _, err := st.Write(buf); err != nil {
			return err
		}
		if _, err := io.ReadFull(st, buf); err != nil {
			return err
		}
		fmt.Printf("seq=%d rtt=%v\n", i, time.Since(t).Round(time.Microsecond))
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}
