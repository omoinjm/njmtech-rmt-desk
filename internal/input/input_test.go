package input

import (
	"errors"
	"math"
	"testing"
)

type rec struct{ calls []string }

func (r *rec) MoveTo(int, float64, float64) error { r.calls = append(r.calls, "move"); return nil }
func (r *rec) Button(b Button, d bool) error {
	r.calls = append(r.calls, map[bool]string{true: "bdown", false: "bup"}[d])
	return nil
}
func (r *rec) Scroll(dx, dy int) error { r.calls = append(r.calls, "scroll"); return nil }
func (r *rec) Key(k Key, d bool) error {
	r.calls = append(r.calls, map[bool]string{true: "kdown", false: "kup"}[d])
	return nil
}
func (r *rec) Close() error { return nil }

func TestApplyValidation(t *testing.T) {
	r := &rec{}
	bad := []Event{
		{T: "move", X: 1.5, Y: 0.5},
		{T: "move", X: math.NaN(), Y: 0},
		{T: "down", Button: 7},
		{T: "keydown", Code: "NotAKey"},
		{T: "explode"},
	}
	for _, ev := range bad {
		if err := Apply(r, ev); err == nil {
			t.Errorf("%+v: expected error", ev)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("invalid events reached injector: %v", r.calls)
	}
	if err := Apply(r, Event{T: "keydown", Code: "Nope"}); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
	good := []Event{{T: "move", X: 0, Y: 1}, {T: "down"}, {T: "wheel", DY: 3}, {T: "keyup", Code: "KeyA"}}
	for _, ev := range good {
		if err := Apply(r, ev); err != nil {
			t.Errorf("%+v: %v", ev, err)
		}
	}
	// zero wheel is dropped
	n := len(r.calls)
	_ = Apply(r, Event{T: "wheel", DY: 0.2})
	if len(r.calls) != n {
		t.Fatal("sub-click wheel should be ignored")
	}
}

func TestReleaserReleasesHeld(t *testing.T) {
	r := &rec{}
	rel := NewReleaser(r)
	_ = rel.Key(Keys["ControlLeft"], true)
	_ = rel.Key(Keys["KeyC"], true)
	_ = rel.Key(Keys["KeyC"], false)
	_ = rel.Button(Left, true)
	r.calls = nil
	rel.ReleaseAll()
	if len(r.calls) != 2 {
		t.Fatalf("want ctrl-up + button-up, got %v", r.calls)
	}
}

func TestKeyTableUnique(t *testing.T) {
	ev, sc, mac := map[uint16]string{}, map[uint16]string{}, map[uint16]string{}
	for code, k := range Keys {
		for _, c := range []struct {
			m map[uint16]string
			v uint16
		}{{ev, k.Evdev}, {sc, k.Scan}, {mac, k.Mac}} {
			if other, dup := c.m[c.v]; dup {
				t.Errorf("duplicate native code %#x: %s and %s", c.v, code, other)
			}
			c.m[c.v] = code
		}
	}
}
