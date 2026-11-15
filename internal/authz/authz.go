// Package authz decides whether a tailnet peer may open a session on this host.
//
// Policy is deny-by-default: a peer is admitted only if its Tailscale login
// or one of its node tags is explicitly allowlisted. Identity comes from the
// tailnet itself (WhoIs), never from anything the peer claims in-band.
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Identity is the authenticated tailnet identity of a remote peer.
type Identity struct {
	Login    string   // e.g. "alice@example.com"; "tagged-devices" for tagged nodes
	NodeName string   // MagicDNS name of the peer node
	Tags     []string // ACL tags on the peer node, e.g. "tag:rmt-viewer"
}

func (id Identity) String() string {
	if len(id.Tags) > 0 {
		return fmt.Sprintf("%s (%s, tags=%s)", id.NodeName, id.Login, strings.Join(id.Tags, ","))
	}
	return fmt.Sprintf("%s (%s)", id.NodeName, id.Login)
}

// Resolver maps a remote address to a tailnet identity. In production this
// is backed by tsnet's LocalClient.WhoIs; tests use a fake.
type Resolver interface {
	Resolve(ctx context.Context, remoteAddr string) (Identity, error)
}

// Policy is the host's access allowlist.
type Policy struct {
	AllowedLogins []string `toml:"allowed_logins"`
	AllowedTags   []string `toml:"allowed_tags"`
}

var ErrDenied = errors.New("authz: peer not allowlisted")

// Authorize returns nil if id is allowed by p.
func (p Policy) Authorize(id Identity) error {
	// Tagged nodes carry a placeholder login; never match on it.
	if id.Login != "" && id.Login != "tagged-devices" {
		for _, l := range p.AllowedLogins {
			if strings.EqualFold(l, id.Login) {
				return nil
			}
		}
	}
	for _, t := range id.Tags {
		if slices.Contains(p.AllowedTags, t) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrDenied, id)
}

// Check resolves remoteAddr and authorizes it in one step.
func (p Policy) Check(ctx context.Context, r Resolver, remoteAddr string) (Identity, error) {
	id, err := r.Resolve(ctx, remoteAddr)
	if err != nil {
		return Identity{}, fmt.Errorf("authz: whois %s: %w", remoteAddr, err)
	}
	return id, p.Authorize(id)
}
