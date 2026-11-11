package keepalive

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// padBody returns kaBody with about n extra bytes of text. The token counts come from Usage, so the
// body size and the ping cost are independent, as with base64 images.
func padBody(n int) string {
	return strings.Replace(kaBody, `"text":"hi"`, `"text":"`+strings.Repeat("x", n)+`"`, 1)
}

// #1100: a body over 8 MiB with a cheap prefix is held and pinged.
func TestABodyOver8MiBUnderTheCostCapIsPinged(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-6, Output: 1e-6, CacheRead: 1e-6, CacheWrite: 1e-6}}})
	recordOne(t, k, kaPolicy(), padBody(9<<20), clock.now())
	if got := k.Stats().Live; got != 1 {
		t.Fatalf("held %d sessions, want 1 for a 9 MiB body under the cost cap", got)
	}
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("sent %d pings, want 1", n)
	}
	if fd.n() != 1 {
		t.Fatalf("dispatcher saw %d pings, want 1", fd.n())
	}
}

// A big body whose ping is over the cost cap is still refused, with its reason.
func TestABigBodyOverTheCostCapIsStillRefused(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-3, Output: 1e-3, CacheRead: 1e-3, CacheWrite: 1e-3}}})
	recordHold(k, sess1, clock.now(), padBody(9<<20), kaPolicy(), true)
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonOverBudget {
		t.Errorf("state = %+v, want %q", st, ReasonOverBudget)
	}
}

// The memory guard: a body over the per-body limit is refused, and many large bodies stay in MaxBytes.
func TestTheMemoryGuardHolds(t *testing.T) {
	k, fd0, clock := testKeeper(t)
	fd0.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-6, Output: 1e-6, CacheRead: 1e-6, CacheWrite: 1e-6}}})
	recordHold(k, sess1, clock.now(), padBody(MaxBodyBytes), kaPolicy(), true)
	if _, ok := k.Entry(sess1); ok {
		t.Error("held a body over the per-body limit")
	}
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonTooLarge {
		t.Errorf("state = %+v, want %q", st, ReasonTooLarge)
	}
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-6, Output: 1e-6, CacheRead: 1e-6, CacheWrite: 1e-6}}})
	big := padBody(MaxBodyBytes - 4096)
	for i := 0; i < 12; i++ {
		recordHold(k, Key{Tenant: "t1", Session: "big" + string(rune('a'+i))}, clock.now(), big, kaPolicy(), true)
	}
	k.mu.Lock()
	held, live := k.bytes, len(k.live)
	k.mu.Unlock()
	if held > MaxBytes {
		t.Errorf("%d bytes held, budget %d", held, MaxBytes)
	}
	if live == 0 {
		t.Error("held nothing: the budget must evict, not refuse at the door")
	}
}

// The switch: Policy.MaxBodyBytes = LegacyMaxBodyBytes restores the old refusal.
func TestTheLegacyLimitSwitchRestoresTheOldRefusal(t *testing.T) {
	k, _, clock := testKeeper(t)
	pol := kaPolicy()
	pol.MaxBodyBytes = LegacyMaxBodyBytes
	recordHold(k, sess1, clock.now(), padBody(9<<20), pol, true)
	if _, ok := k.Entry(sess1); ok {
		t.Error("held a 9 MiB body under the 8 MiB switch")
	}
	if st := stateOf(k, sess1); st.Pingable || st.Reason != ReasonTooLarge {
		t.Errorf("state = %+v, want %q", st, ReasonTooLarge)
	}
}

func TestBodyLimitDefaultsAndClamps(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, MaxBodyBytes}, {-1, MaxBodyBytes}, {LegacyMaxBodyBytes, LegacyMaxBodyBytes}, {MaxBytes * 2, MaxBytes}} {
		if got := (Policy{MaxBodyBytes: c.in}).BodyLimit(); got != c.want {
			t.Errorf("BodyLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// An unpriced model has no cost cap to judge a big body, so the old 8 MiB limit stays for it.
func TestAnUnpricedModelKeepsThe8MiBLimit(t *testing.T) {
	k, _, clock := testKeeper(t) // the fake has no prices
	recordHold(k, sess1, clock.now(), padBody(9<<20), kaPolicy(), true)
	if _, ok := k.Entry(sess1); ok {
		t.Error("held a 9 MiB body for an unpriced model")
	}
	if st := stateOf(k, sess1); st.Reason != ReasonTooLarge {
		t.Errorf("reason = %q, want %q", st.Reason, ReasonTooLarge)
	}
	k, _, clock = testKeeper(t)
	recordHold(k, sess1, clock.now(), padBody(7<<20), kaPolicy(), true)
	if _, ok := k.Entry(sess1); !ok {
		t.Error("refused a 7 MiB body for an unpriced model")
	}
}

func TestTheConfigBodyLimitMirrorsTheLibrary(t *testing.T) {
	if config.DefaultKeepAliveMaxBodyBytes != MaxBodyBytes {
		t.Errorf("config default %d != keepalive.MaxBodyBytes %d", config.DefaultKeepAliveMaxBodyBytes, MaxBodyBytes)
	}
}
