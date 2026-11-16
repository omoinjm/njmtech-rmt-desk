package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RFC 6238 TOTP: SHA-1, 6 digits, 30s step (what every authenticator app
// supports). ±1 step of clock skew is accepted.

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit base32 secret.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPURI returns an otpauth:// URI for enrolling in an authenticator app.
func TOTPURI(secret, account string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", "rmt-desk")
	return "otpauth://totp/" + url.PathEscape("rmt-desk:"+account) + "?" + v.Encode()
}

func totpAt(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1_000_000)
}

// TOTPCode returns the code for time t (used by tests / `rmtd totp test`).
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return totpAt(key, uint64(t.Unix()/30)), nil
}

// VerifyTOTP checks code against secret at time now, allowing ±1 step.
// It returns the matched counter so callers can reject replays.
func VerifyTOTP(secret, code string, now time.Time) (uint64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil || len(code) != 6 {
		return 0, false
	}
	c := uint64(now.Unix() / 30)
	for _, ctr := range []uint64{c - 1, c, c + 1} {
		if subtle.ConstantTimeCompare([]byte(totpAt(key, ctr)), []byte(code)) == 1 {
			return ctr, true
		}
	}
	return 0, false
}
