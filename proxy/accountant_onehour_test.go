package proxy

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
)

// #1243. A 1-hour-on-wait ping moves the entry of a 5-minute main thread to 1 hour, and a read 5 to 60
// minutes after the ping is a rescue: a 1.25x rebuild became a 0.1x read. The library books it only for a
// provider that sets Provider.ReadSetsTTL (accountant v0.5.0, "A read sets the TTL"), with the ping's TTL.
// The proxy sets ReadSetsTTL only on a route its keeper PROVED sets the TTL on a read, never from a name.

const ohPrefix = 100_000

func ohBody(turns int) []byte {
	// 4 bytes a token: the first message is the 100,000-token prefix.
	msgs := `[{"role":"user","content":"` + strings.Repeat("x", ohPrefix*4) + `","cache_control":{"type":"ephemeral"}}`
	for i := 1; i < turns; i++ {
		msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"more","cache_control":{"type":"ephemeral"}}`
	}
	return []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":` + msgs + `]}`)
}

// ohRun runs: a real 5-minute request that writes the prefix; a 1-hour ping 4 minutes later that reads it;
// a return at the given gap after the request that reads it again. readSets is Provider.ReadSetsTTL on the
// return's facts, as accEndNow sets it from the keeper's proof.
func ohRun(t *testing.T, returnAfter time.Duration, readSets, oneHourPing bool) accountant.Result {
	t.Helper()
	st, pings := newSavingsTracker(), NewPingStore()
	t0 := time.Unix(1_700_000_000, 0)
	turn := func(at time.Time, b []byte, u Usage, rs bool) accountant.Result {
		req := st.begin(accountant.RequestMeta{RequestID: at.String(), Session: "s", Model: "m", At: at}, b)
		th := accountant.Thread{ID: "main"}
		facts := entryFacts(bschemas.Anthropic, "m", b, pings, "s", th)
		facts.Provider.ReadSetsTTL = rs
		st.end(req, th, b, nil, facts)
		return st.compute(req, u)
	}
	turn(t0, ohBody(1), Usage{CacheWrite: ohPrefix}, false)
	r := components.KeepAliveReport{Session: "s", Model: "m", Route: "h", Status: 200, CacheRead: ohPrefix, Output: 1,
		TS: t0.Add(4 * time.Minute).UnixMilli(), Window: t0, OneHour: oneHourPing}
	if _, ok := PingSavings(r, pings); !ok {
		t.Fatal("the ping was not converted")
	}
	return turn(t0.Add(returnAfter), ohBody(2), Usage{CacheRead: ohPrefix, CacheWrite: 20}, readSets)
}

func ohRescue(res accountant.Result) (n int, found bool) {
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue {
			n += e.Saved.CacheWrite5m
			found = true
		}
	}
	return
}

// A return more than 5 minutes after the 1-hour ping books the rescue of the tokens it read: the
// 100,000-token prefix moves from a 5-minute write (1.25) to a read (0.10).
func TestOneHourPingReturnAfterFiveMinutesBooksTheRescue(t *testing.T) {
	res := ohRun(t, 20*time.Minute, true, true)
	n, found := ohRescue(res)
	if !found {
		t.Fatalf("no rescue booked: %+v", res.CacheEffects)
	}
	if n < ohPrefix*95/100 || n > ohPrefix {
		t.Errorf("rescue saved %d 5m-write tokens, want about %d (the prefix both read)", n, ohPrefix)
	}
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue && e.Caused.CacheRead != e.Saved.CacheWrite5m {
			t.Errorf("rescue moves %d write tokens to %d read tokens; want the same count", e.Saved.CacheWrite5m, e.Caused.CacheRead)
		}
	}
	// Priced at 1.25 for a 5-minute write and 0.10 for a read, per token: a saving.
	if gain := 1.25*float64(n) - 0.10*float64(n); gain <= 0 || len(res.Checks) != 0 {
		t.Errorf("gain %.0f checks %v", gain, res.Checks)
	}
}

// A return inside 5 minutes of the request was hot without the ping: no rescue.
func TestOneHourPingReturnInsideFiveMinutesBooksNoRescue(t *testing.T) {
	if _, found := ohRescue(ohRun(t, 4*time.Minute+30*time.Second, true, true)); found {
		t.Error("a rescue was booked for a return inside the 5-minute lifetime")
	}
}

// A route that is not proven to set the TTL on a read (ReadSetsTTL off) books no rescue: the library keeps
// the write-time rule, and the 5-minute entry is judged expired. This is the behavior before the property.
func TestNoRescueWhenTheRouteIsNotProvenToSetTheTTLOnARead(t *testing.T) {
	if _, found := ohRescue(ohRun(t, 20*time.Minute, false, true)); found {
		t.Error("a rescue was booked on a route that is not proven (ReadSetsTTL off)")
	}
}

// A plain ping (not 1-hour) books no 1-hour rescue even on a proven route: it asked no TTL.
func TestAPlainPingBooksNoOneHourRescue(t *testing.T) {
	if _, found := ohRescue(ohRun(t, 20*time.Minute, true, false)); found {
		t.Error("a 5-minute ping got a 1-hour rescue")
	}
}

// pingFromReport carries the TTL of a 1-hour ping, and leaves a plain ping at 0 (not known).
func TestPingFromReportCarriesTheOneHourTTL(t *testing.T) {
	r := okReport()
	if got := pingFromReport(r).TTL; got != 0 {
		t.Errorf("a plain ping carries TTL %v, want 0", got)
	}
	r.OneHour = true
	if got := pingFromReport(r).TTL; got != time.Hour {
		t.Errorf("a 1-hour ping carries TTL %v, want 1h", got)
	}
}

// accEndNow sets ReadSetsTTL from the keeper's proof and from nothing else.
func TestReadSetsTTLComesFromTheKeepersProofOnly(t *testing.T) {
	for _, proven := range []bool{false, true} {
		c := &capture{savings: newSavingsTracker(), pings: NewPingStore(), provider: string(bschemas.Anthropic), route: "/v1/messages"}
		c.noteReadSets("http://up", func(p bschemas.ModelProvider, endpoint, route, model string) bool {
			return proven && p == bschemas.Anthropic && endpoint == "http://up" && route == "/v1/messages" && model == "m"
		})
		c.accBegin(accountant.RequestMeta{RequestID: "r", Session: "s", Model: "m", At: time.Unix(1_700_000_000, 0)}, ohBody(1))
		c.accEnd(thread.Result{}, ohBody(1), nil)
		c.accWait()
		if c.accFacts.Provider.ReadSetsTTL != proven {
			t.Errorf("proven=%v: ReadSetsTTL = %v", proven, c.accFacts.Provider.ReadSetsTTL)
		}
	}
}

// prefixRewritten is true after the agent's own compaction or when the thread forks another one, and
// nowhere else. Nil-safe.
func TestCapturePrefixRewritten(t *testing.T) {
	var nilC *capture
	if nilC.prefixRewritten() {
		t.Error("a nil capture reports a rewritten prefix")
	}
	if (&capture{}).prefixRewritten() {
		t.Error("a plain request reports a rewritten prefix")
	}
	if !(&capture{compKnown: true, compVerdict: "unknown"}).prefixRewritten() {
		t.Error("a request after a client compaction does not report it")
	}
	if !(&capture{hasInherit: true}).prefixRewritten() {
		t.Error("a forked thread does not report it")
	}
	if (&capture{compKnown: false, compVerdict: "unknown"}).prefixRewritten() {
		t.Error("an unjudged request reports a compaction")
	}
}
