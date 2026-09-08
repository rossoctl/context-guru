package store

import (
	"strings"
	"testing"
	"time"
)

// TestIdleExitFloorRefusesADestructiveThreshold is about money, not tidiness.
//
// Process exit wipes this store, and what it wipes includes frozen decisions. A frozen
// decision that dies mid-session is the 11.5x cache-WRITE regression FrozenLost exists to
// detect: the next turn re-creates the whole prefix at write prices instead of reading it. So a
// short idle-exit threshold does not degrade gracefully — it turns a convenience feature into a
// cost regression that presents as the proxy misbehaving, on the machine of the first-time
// evaluator this whole funnel is aimed at.
//
// Hence a startup error rather than a doc comment. The 30-minute case below is the one somebody
// will actually reach for ("exit quickly, it is only a laptop"), and it must not start.
func TestIdleExitFloorRefusesADestructiveThreshold(t *testing.T) {
	def := Options{} // ttl_seconds unset => DefaultTTL (10000s), floor 2x = 5h33m20s
	if got, want := IdleExitFloor(def), 2*DefaultTTL; got != want {
		t.Fatalf("IdleExitFloor(default) = %s, want %s", got, want)
	}

	for _, c := range []struct {
		name    string
		d       time.Duration
		o       Options
		wantErr bool
	}{
		{"off is always valid", 0, def, false},
		{"negative is off too", -time.Hour, def, false},
		{"30m on the default TTL is destructive", 30 * time.Minute, def, true},
		{"1h is still below the default floor", time.Hour, def, true},
		{"just under the floor", 2*DefaultTTL - time.Second, def, true},
		{"exactly the floor is allowed", 2 * DefaultTTL, def, false},
		{"the installer's 24h default", 24 * time.Hour, def, false},
		// A tiny configured TTL must not collapse the floor to seconds: 2x30s is 1m, which is
		// shorter than the keep-alive's own ping window, so the absolute 1h term takes over.
		{"tiny ttl falls back to the 1h term", 30 * time.Minute, Options{TTLSeconds: 30}, true},
		{"tiny ttl accepts 1h", time.Hour, Options{TTLSeconds: 30}, false},
		// A LONG configured TTL must raise the floor above 1h, or an operator who deliberately
		// widened the store's lifetime gets a threshold that expires it.
		{"long ttl raises the floor above 24h", 24 * time.Hour, Options{TTLSeconds: 100000}, true},
	} {
		err := ValidateIdleExit(c.d, c.o)
		if c.wantErr && err == nil {
			t.Errorf("%s: ValidateIdleExit(%s, ttl=%s) accepted a threshold below the %s floor",
				c.name, c.d, c.o.EffectiveTTL(), IdleExitFloor(c.o))
			continue
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: ValidateIdleExit(%s, ttl=%s) rejected a valid threshold: %v",
				c.name, c.d, c.o.EffectiveTTL(), err)
			continue
		}
		// The message has to tell the operator what to change. A bare "invalid value" here
		// leaves them guessing at a number they have no reason to know.
		if err != nil {
			for _, want := range []string{"idle-exit", "floor"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error message omits %q: %v", c.name, want, err)
				}
			}
		}
	}
}

// TestEffectiveTTLIsWhatNewMemoryUses closes the gap the floor depends on: IdleExitFloor sizes
// a process's whole lifetime from EffectiveTTL, so a store that actually ran with a DIFFERENT
// lifetime would be protected by a floor computed for a lifetime it never had. NewMemory calls
// EffectiveTTL rather than repeating the defaulting rule, and this holds it there.
func TestEffectiveTTLIsWhatNewMemoryUses(t *testing.T) {
	for _, o := range []Options{{}, {TTLSeconds: 0}, {TTLSeconds: -5}, {TTLSeconds: 42}, {TTLSeconds: 100000}} {
		if got := NewMemory(o).ttl; got != o.EffectiveTTL() {
			t.Errorf("NewMemory(%+v).ttl = %s but EffectiveTTL() = %s; the idle-exit floor is "+
				"computed from the second and would protect a lifetime the store is not using",
				o, got, o.EffectiveTTL())
		}
	}
}

// TestValidateIdleExitMessageNamesTheBindingTerm: this refusal is startup-fatal, so its message is
// the only evidence anyone gathers — and it was wrong in both halves.
//
// It credited the floor to "2x the store's entry lifetime" even when the absolute 1h term was the
// binding one (with ttl_seconds: 30 it announced a 1h floor derived from 2x30s, which is 1m), and it
// advised raising ttl_seconds — the one change that RAISES the floor, so an operator who followed it
// got the same refusal with a bigger number.
func TestValidateIdleExitMessageNamesTheBindingTerm(t *testing.T) {
	// (a) the store term binds: default TTL of 10000s puts the floor at 5h33m20s.
	err := ValidateIdleExit(30*time.Minute, Options{})
	if err == nil {
		t.Fatal("30m accepted against the default floor")
	}
	msg := err.Error()
	if !strings.Contains(msg, "2x the store's") {
		t.Errorf("the store term binds but the message does not say so: %v", err)
	}
	if !strings.Contains(msg, "LOWER store.ttl_seconds") {
		t.Errorf("message does not offer the remediation that actually lowers the floor: %v", err)
	}
	if strings.Contains(msg, "raise store.ttl_seconds") {
		t.Errorf("message still advises RAISING ttl_seconds, which raises the floor: %v", err)
	}

	// (b) the absolute term binds: a 30s TTL makes 2x TTL 1m, so the 1h minimum is doing the work
	// and ttl_seconds is not the lever.
	err = ValidateIdleExit(30*time.Minute, Options{TTLSeconds: 30})
	if err == nil {
		t.Fatal("30m accepted against the 1h minimum")
	}
	msg = err.Error()
	if !strings.Contains(msg, "absolute minimum") {
		t.Errorf("the 1h term binds but the message does not say so: %v", err)
	}
	if strings.Contains(msg, "which is 2x the store's") {
		t.Errorf("message attributes a 1h floor to 2x a 30s lifetime, which is 1m: %v", err)
	}
	// It should still tell the operator what 2x TTL actually is, so the arithmetic is checkable.
	if !strings.Contains(msg, "1m0s") {
		t.Errorf("message does not show what the 2x-TTL floor would be (1m0s): %v", err)
	}
}
