package authz

import (
	"context"
	"errors"
	"testing"
)

type fakeResolver map[string]Identity

func (f fakeResolver) Resolve(_ context.Context, addr string) (Identity, error) {
	id, ok := f[addr]
	if !ok {
		return Identity{}, errors.New("unknown peer")
	}
	return id, nil
}

func TestAuthorize(t *testing.T) {
	p := Policy{AllowedLogins: []string{"Alice@Example.com"}, AllowedTags: []string{"tag:rmt-viewer"}}
	cases := []struct {
		name string
		id   Identity
		ok   bool
	}{
		{"allowed login case-insensitive", Identity{Login: "alice@example.com"}, true},
		{"allowed tag", Identity{Login: "tagged-devices", Tags: []string{"tag:rmt-viewer"}}, true},
		{"other login", Identity{Login: "mallory@example.com"}, false},
		{"other tag", Identity{Login: "tagged-devices", Tags: []string{"tag:server"}}, false},
		{"empty identity", Identity{}, false},
		{"tagged placeholder login never matches", Identity{Login: "tagged-devices"}, false},
	}
	for _, c := range cases {
		err := p.Authorize(c.id)
		if (err == nil) != c.ok {
			t.Errorf("%s: got err=%v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrDenied) {
			t.Errorf("%s: error should wrap ErrDenied", c.name)
		}
	}
}

func TestEmptyPolicyDeniesAll(t *testing.T) {
	if err := (Policy{}).Authorize(Identity{Login: "alice@example.com", Tags: []string{"tag:x"}}); err == nil {
		t.Fatal("empty policy must deny")
	}
}

func TestCheckResolveError(t *testing.T) {
	p := Policy{AllowedLogins: []string{"alice@example.com"}}
	if _, err := p.Check(context.Background(), fakeResolver{}, "100.64.0.9:1234"); err == nil {
		t.Fatal("unresolvable peer must be rejected")
	}
	r := fakeResolver{"100.64.0.1:1": {Login: "alice@example.com"}}
	if _, err := p.Check(context.Background(), r, "100.64.0.1:1"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}
