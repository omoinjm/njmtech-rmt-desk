package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"
)

func TestPasswordRoundTrip(t *testing.T) {
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password accepted: %v", err)
	}
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	if ok, err := VerifyPassword(h, "correct horse battery"); !ok || err != nil {
		t.Fatalf("verify: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "correct horse batterY"); ok {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "$argon2id$v=19$m=1,t=1$x$y", "$argon2id$v=19$m=99999999,t=3,p=2$AAAA$AAAA"} {
		if ok, err := VerifyPassword(bad, "x"); ok || err == nil {
			t.Errorf("malformed hash %q: ok=%v err=%v", bad, ok, err)
		}
	}
}

func TestTOTPRFC6238Vector(t *testing.T) {
	// RFC 6238 Appendix B, SHA-1 secret "12345678901234567890", T=59 -> 94287082 (8 digits) -> 287082 (6).
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	code, err := TOTPCode(secret, time.Unix(59, 0))
	if err != nil || code != "287082" {
		t.Fatalf("got %s %v want 287082", code, err)
	}
	if _, ok := VerifyTOTP(secret, "287082", time.Unix(59+30, 0)); !ok {
		t.Fatal("one step of skew should be accepted")
	}
	if _, ok := VerifyTOTP(secret, "287082", time.Unix(59+90, 0)); ok {
		t.Fatal("three steps of skew must be rejected")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newGate(t *testing.T, cfg Config, consent ConsentFunc) (*Gate, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	return &Gate{Config: cfg, Consent: consent, Now: c.now}, c
}

var alice = authz.Identity{Login: "alice@example.com", NodeName: "laptop"}

func code(err error) string { return AsError(err).Code }

func TestUnattendedPasswordAndLockout(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	g, clk := newGate(t, Config{Mode: Unattended, PasswordHash: h}, nil)
	ctx := context.Background()

	if _, err := g.Admit(ctx, alice, proto.Hello{}); code(err) != proto.CodeAuthRequired {
		t.Fatalf("no password: %v", err)
	}
	perms, err := g.Admit(ctx, alice, proto.Hello{Password: "correct horse battery"})
	if err != nil || len(perms) != 4 {
		t.Fatalf("good password: %v %v", perms, err)
	}
	for i := range 5 {
		if _, err := g.Admit(ctx, alice, proto.Hello{Password: "nope"}); code(err) != proto.CodeAuthFailed {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Locked now — even the right password is refused.
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: "correct horse battery"}); code(err) != proto.CodeLocked {
		t.Fatalf("expected lockout, got %v", err)
	}
	// Other peers are unaffected.
	bob := authz.Identity{Login: "bob@example.com", NodeName: "phone"}
	if _, err := g.Admit(ctx, bob, proto.Hello{Password: "correct horse battery"}); err != nil {
		t.Fatalf("bob: %v", err)
	}
	clk.t = clk.t.Add(61 * time.Second)
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: "correct horse battery"}); err != nil {
		t.Fatalf("after lockout expiry: %v", err)
	}
}

func TestTOTPRequiredAndReplayRejected(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	sec, _ := NewTOTPSecret()
	g, clk := newGate(t, Config{Mode: Unattended, PasswordHash: h, TOTPSecret: sec}, nil)
	ctx := context.Background()
	pw := "correct horse battery"

	if _, err := g.Admit(ctx, alice, proto.Hello{Password: pw}); code(err) != proto.CodeAuthRequired {
		t.Fatalf("missing totp: %v", err)
	}
	c, _ := TOTPCode(sec, clk.t)
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: pw, TOTP: c}); err != nil {
		t.Fatalf("valid totp: %v", err)
	}
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: pw, TOTP: c}); code(err) != proto.CodeAuthFailed {
		t.Fatalf("replayed totp accepted: %v", err)
	}
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: "wrong password", TOTP: "000000"}); code(err) != proto.CodeAuthFailed {
		t.Fatal("wrong both")
	}
}

func TestAttendedConsent(t *testing.T) {
	answer := false
	var asked string
	consent := func(_ context.Context, who string, _ time.Duration) (bool, error) {
		asked = who
		return answer, nil
	}
	g, _ := newGate(t, Config{}, consent) // no password => attended
	ctx := context.Background()
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: "ignored in attended"}); code(err) != proto.CodeDenied {
		t.Fatalf("declined: %v", err)
	}
	if asked == "" {
		t.Fatal("consent not asked")
	}
	answer = true
	if _, err := g.Admit(ctx, alice, proto.Hello{}); err != nil {
		t.Fatalf("accepted: %v", err)
	}
}

func TestBothMode(t *testing.T) {
	h, _ := HashPassword("correct horse battery")
	prompted := 0
	consent := func(context.Context, string, time.Duration) (bool, error) { prompted++; return true, nil }
	g, _ := newGate(t, Config{Mode: Both, PasswordHash: h, Permissions: []string{"input"}}, consent)
	ctx := context.Background()

	perms, err := g.Admit(ctx, alice, proto.Hello{Password: "correct horse battery"})
	if err != nil || prompted != 0 {
		t.Fatalf("password path: %v prompted=%d", err, prompted)
	}
	if len(perms) != 2 || perms[0] != "view" {
		t.Fatalf("view must be implied: %v", perms)
	}
	if _, err := g.Admit(ctx, alice, proto.Hello{}); err != nil || prompted != 1 {
		t.Fatalf("prompt path: %v prompted=%d", err, prompted)
	}
	// Wrong password does NOT fall back to the prompt.
	if _, err := g.Admit(ctx, alice, proto.Hello{Password: "bad"}); code(err) != proto.CodeAuthFailed || prompted != 1 {
		t.Fatalf("wrong password: %v prompted=%d", err, prompted)
	}
}

func TestConcurrentPromptRejected(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	consent := func(context.Context, string, time.Duration) (bool, error) {
		close(started)
		<-release
		return true, nil
	}
	g, _ := newGate(t, Config{}, consent)
	done := make(chan error)
	go func() { _, err := g.Admit(context.Background(), alice, proto.Hello{}); done <- err }()
	<-started
	if _, err := g.Admit(context.Background(), alice, proto.Hello{}); code(err) != proto.CodeBusy {
		t.Fatalf("second prompt: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
