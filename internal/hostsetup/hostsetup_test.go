package hostsetup

import (
	"testing"
	"time"

	"rmt-desk/internal/access"
	"rmt-desk/internal/config"
)

func TestConfigEdits(t *testing.T) {
	dir := t.TempDir()
	err := Edit(dir, func(c *config.Host) error {
		if err := Allow(c, "you@example.com"); err != nil {
			return err
		}
		if err := Allow(c, "tag:ops"); err != nil {
			return err
		}
		return Allow(c, "you@example.com") // idempotent
	})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Load(dir)
	if len(c.Policy.AllowedLogins) != 1 || len(c.Policy.AllowedTags) != 1 {
		t.Fatalf("allowlist %+v", c.Policy)
	}
	if Allow(&c, "a b") == nil || Allow(&c, " ") == nil {
		t.Fatal("bad identities accepted")
	}
	Revoke(&c, "tag:ops")
	if len(c.Policy.AllowedTags) != 0 {
		t.Fatal("revoke failed")
	}

	if _, _, err := NewTOTP(&c); err == nil {
		t.Fatal("TOTP without password")
	}
	if SetPassword(&c, "short") == nil {
		t.Fatal("short password accepted")
	}
	if err := SetPassword(&c, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	sec, uri, err := NewTOTP(&c)
	if err != nil || uri == "" {
		t.Fatal(err)
	}
	code, err := access.TOTPCode(sec, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if EnableTOTP(&c, sec, wrong) == nil {
		t.Fatal("wrong code accepted")
	}
	if err := EnableTOTP(&c, sec, code); err != nil || c.Access.TOTPSecret != sec {
		t.Fatalf("enable: %v", err)
	}
	if SetMode(&c, "bogus") == nil || SetMode(&c, access.Unattended) != nil {
		t.Fatal("mode validation")
	}
	ClearPassword(&c)
	if c.Access.PasswordHash != "" || c.Access.TOTPSecret != "" || c.Access.EffectiveMode() != access.Attended {
		t.Fatal("clear")
	}
	if LoggedIn(dir) {
		t.Fatal("not logged in yet")
	}
}
