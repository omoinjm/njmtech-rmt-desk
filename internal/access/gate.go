package access

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"rmt-desk/internal/authz"
	"rmt-desk/internal/proto"
)

// Mode controls how an allowlisted peer is admitted.
type Mode string

const (
	// Attended: someone at the host must click Allow.
	Attended Mode = "attended"
	// Unattended: password (+TOTP if enrolled) required, no prompt.
	Unattended Mode = "unattended"
	// Both: a correct password admits immediately; with no password the
	// host user is prompted.
	Both Mode = "both"
)

// Config is persisted in rmtd.toml under [access].
type Config struct {
	Mode         Mode     `toml:"mode"`
	PasswordHash string   `toml:"password_hash"`
	TOTPSecret   string   `toml:"totp_secret"`
	Permissions  []string `toml:"permissions"` // granted to every admitted session
	PromptSecs   int      `toml:"prompt_seconds"`
}

var AllPermissions = []string{"view", "input", "clipboard", "files"}

func (c Config) EffectiveMode() Mode {
	switch c.Mode {
	case Attended, Unattended, Both:
	default:
		c.Mode = Both
	}
	// Without a password, only attended access is possible.
	if c.PasswordHash == "" {
		return Attended
	}
	return c.Mode
}

// ConsentFunc asks the person at the host to allow a session.
type ConsentFunc func(ctx context.Context, who string, timeout time.Duration) (bool, error)

// Gate applies Config to an incoming session request.
type Gate struct {
	Config  Config
	Consent ConsentFunc
	Now     func() time.Time // for tests

	limiter  Limiter
	promptMu sync.Mutex // one consent dialog at a time
	totpMu   sync.Mutex
	lastTOTP uint64 // last accepted TOTP counter (replay protection)
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Error is returned to the viewer; Code lets the UI react (e.g. show a
// password form) without parsing text.
type Error struct{ Wire proto.Error }

func (e *Error) Error() string { return e.Wire.Message }

func deny(code, msg string) *Error { return &Error{proto.Error{Code: code, Message: msg}} }

// Admit returns the permissions granted for this session, or an *Error.
func (g *Gate) Admit(ctx context.Context, id authz.Identity, h proto.Hello) ([]string, error) {
	key := id.NodeName + "|" + id.Login
	if until, locked := g.limiter.Locked(key, g.now()); locked {
		return nil, deny(proto.CodeLocked, fmt.Sprintf("too many failed attempts; try again after %s", until.Format(time.Kitchen)))
	}

	mode := g.Config.EffectiveMode()
	perms := g.Config.Permissions
	if len(perms) == 0 {
		perms = AllPermissions
	}
	// "view" is implied by any session.
	if !slices.Contains(perms, "view") {
		perms = append([]string{"view"}, perms...)
	}

	tryPassword := mode == Unattended || (mode == Both && h.Password != "")
	if tryPassword {
		if h.Password == "" {
			return nil, deny(proto.CodeAuthRequired, "password required")
		}
		if g.Config.TOTPSecret != "" && h.TOTP == "" {
			return nil, deny(proto.CodeAuthRequired, "password and authenticator code required")
		}
		if err := g.checkSecrets(h); err != nil {
			g.limiter.Fail(key, g.now())
			return nil, err
		}
		g.limiter.Success(key)
		return perms, nil
	}

	// Attended.
	if g.Consent == nil {
		return nil, deny(proto.CodeDenied, "no one available to approve the session")
	}
	if !g.promptMu.TryLock() {
		return nil, deny(proto.CodeBusy, "another connection request is pending on the host")
	}
	defer g.promptMu.Unlock()
	secs := g.Config.PromptSecs
	if secs <= 0 {
		secs = 30
	}
	ok, err := g.Consent(ctx, id.String(), time.Duration(secs)*time.Second)
	if err != nil {
		return nil, deny(proto.CodeDenied, "could not ask the host user: "+err.Error())
	}
	if !ok {
		// Count declines too, so a peer can't spam the host user with dialogs.
		g.limiter.Fail(key, g.now())
		return nil, deny(proto.CodeDenied, "the host user declined the connection")
	}
	return perms, nil
}

func (g *Gate) checkSecrets(h proto.Hello) error {
	ok, err := VerifyPassword(g.Config.PasswordHash, h.Password)
	if err != nil {
		return deny(proto.CodeDenied, "host password misconfigured")
	}
	// Always evaluate TOTP too, so timing doesn't reveal which factor failed.
	totpOK := true
	if g.Config.TOTPSecret != "" {
		g.totpMu.Lock()
		ctr, valid := VerifyTOTP(g.Config.TOTPSecret, h.TOTP, g.now())
		totpOK = valid && ctr > g.lastTOTP
		if ok && totpOK {
			g.lastTOTP = ctr
		}
		g.totpMu.Unlock()
	}
	if !ok || !totpOK {
		return deny(proto.CodeAuthFailed, "incorrect password or authenticator code")
	}
	return nil
}

// Limiter implements per-peer exponential lockout: after 5 failures the
// peer is locked for 1 min, doubling per further failure up to 1 h.
type Limiter struct {
	mu sync.Mutex
	m  map[string]*limitState
}

type limitState struct {
	fails int
	until time.Time
}

const freeAttempts = 5

func (l *Limiter) Locked(key string, now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[key]
	if s == nil || !now.Before(s.until) {
		return time.Time{}, false
	}
	return s.until, true
}

func (l *Limiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*limitState{}
	}
	s := l.m[key]
	if s == nil {
		s = &limitState{}
		l.m[key] = s
	}
	s.fails++
	if s.fails >= freeAttempts {
		d := time.Minute << min(s.fails-freeAttempts, 6) // 1m .. 64m
		s.until = now.Add(min(d, time.Hour))
	}
}

func (l *Limiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

// AsError extracts the viewer-facing error.
func AsError(err error) proto.Error {
	var e *Error
	if errors.As(err, &e) {
		return e.Wire
	}
	return proto.Error{Code: proto.CodeDenied, Message: "access denied"}
}
