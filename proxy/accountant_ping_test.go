package proxy

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/thread"
)

var pingAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func okReport() components.KeepAliveReport {
	return components.KeepAliveReport{Session: "s", Thread: "", Model: "m", Route: "h", Status: 200,
		CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 100, FreshInput: 10, Output: 1,
		CostUSD: 9.99, TS: pingAt.UnixMilli(), Window: pingAt.Add(-4 * time.Minute)}
}

func TestPingFromReportMapsEveryField(t *testing.T) {
	p := pingFromReport(okReport())
	want := accountant.Ping{Session: "s", ThreadID: "", Model: "m", Host: "h",
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: 10, CacheRead: 5000, CacheWrite: 300, CacheWrite1h: 100}, Output: 1},
		At:    time.UnixMilli(pingAt.UnixMilli()), Window: pingAt.Add(-4 * time.Minute)}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
	if p.Usage.CostUSD != 0 {
		t.Fatal("the host prices a ping with its own rates")
	}
}

func TestPingSavingsPricesAndStoresFacts(t *testing.T) {
	st := NewPingStore()
	res, ok := PingSavings(okReport(), st)
	if !ok || len(res.Ops) != 1 || res.Ops[0].Call == nil {
		t.Fatalf("ok=%v res=%+v", ok, res)
	}
	f, ok := st.take(accPingKey("s", "main"))
	if !ok || f.PingWindow == "" || !f.LastPing.Equal(time.UnixMilli(pingAt.UnixMilli())) {
		t.Fatalf("facts %+v ok=%v", f, ok)
	}
}

func TestPingSavingsSkipRules(t *testing.T) {
	cases := map[string]func(*components.KeepAliveReport){
		"zero window": func(r *components.KeepAliveReport) { r.Window = time.Time{} },
		"status 500":  func(r *components.KeepAliveReport) { r.Status = 500 },
		"status 0":    func(r *components.KeepAliveReport) { r.Status = 0 },
		"zero usage": func(r *components.KeepAliveReport) {
			r.CacheRead, r.CacheWrite, r.CacheWrite1h, r.FreshInput, r.Output = 0, 0, 0, 0, 0
		},
	}
	for name, mut := range cases {
		st := NewPingStore()
		r := okReport()
		mut(&r)
		if _, ok := PingSavings(r, st); ok {
			t.Errorf("%s: converted", name)
		}
		if len(st.m) != 0 {
			t.Errorf("%s: facts stored", name)
		}
	}
	for name, mut := range map[string]func(*components.KeepAliveReport){
		"manual": func(r *components.KeepAliveReport) { r.Manual = true; r.Strategy = "manual" },
		"sweep":  func(r *components.KeepAliveReport) { r.SweepAnswerUSD = 0.5 },
	} {
		r := okReport()
		mut(&r)
		if _, ok := PingSavings(r, NewPingStore()); !ok {
			t.Errorf("%s: not converted", name)
		}
	}
}

func TestPingSavingsNilStoreIsFine(t *testing.T) {
	if _, ok := PingSavings(okReport(), nil); !ok {
		t.Fatal("result must still be returned")
	}
}

func TestPingStoreTakeDeletesAndOverwriteKeepsLatest(t *testing.T) {
	s := NewPingStore()
	s.put("k", accountant.PingFacts{PingWindow: "a"})
	s.put("k", accountant.PingFacts{PingWindow: "b"})
	f, ok := s.take("k")
	if !ok || f.PingWindow != "b" {
		t.Fatalf("%+v %v", f, ok)
	}
	if _, ok := s.take("k"); ok {
		t.Fatal("take must delete")
	}
	var nilStore *PingStore
	nilStore.put("k", accountant.PingFacts{})
	if _, ok := nilStore.take("k"); ok {
		t.Fatal("nil store")
	}
}

func TestPingStoreIsBounded(t *testing.T) {
	s := NewPingStore()
	for i := 0; i < pingStoreMax+500; i++ {
		s.put(fmt.Sprint("k", i), accountant.PingFacts{PingWindow: "w"})
	}
	if len(s.m) > pingStoreMax {
		t.Fatalf("len %d > %d", len(s.m), pingStoreMax)
	}
	if _, ok := s.take(fmt.Sprint("k", pingStoreMax+499)); !ok {
		t.Fatal("the newest entry must survive")
	}
}

func TestEntryFactsReadsPingFactsUntilDropped(t *testing.T) {
	st := NewPingStore()
	PingSavings(okReport(), st)
	th := accountant.Thread{ID: "main"}
	f := entryFacts("", "", nil, st, "s", th)
	if f.LastPing.IsZero() || f.PingWindow == "" || f.PingRead != nil {
		t.Fatalf("%+v", f)
	}
	st.drop(accPingKey("s", "main"), "") // the capture spends them once the request is priced
	g := entryFacts("", "", nil, st, "s", th)
	if !g.LastPing.IsZero() || g.PingWindow != "" {
		t.Fatalf("second request got %+v", g)
	}
	if entryFacts("", "", nil, nil, "s", th).TTL != 5*time.Minute {
		t.Fatal("nil store changes nothing")
	}
}

func TestNamedThreadKeyRoundTrips(t *testing.T) {
	st := NewPingStore()
	r := okReport()
	r.Thread = "sub-1"
	if _, ok := PingSavings(r, st); !ok {
		t.Fatal("not converted")
	}
	if f := entryFacts("", "", nil, st, "s", accountant.Thread{ID: "main"}); !f.LastPing.IsZero() {
		t.Fatal("the main thread must not take a named thread's ping")
	}
	f := entryFacts("", "", nil, st, "s", accountant.Thread{ID: "sub-1"})
	if f.LastPing.IsZero() || f.PingWindow == "" {
		t.Fatalf("%+v", f)
	}
}

// The proxy's keep-alive path credits a rescue end to end: turn 1 caches the body, the keeper's ping
// replays it and reads it, and turn 2 after the TTL but inside the ping's renewal is hot with a
// rescue on the ping's window. The extent the ping read is derived from its usage (accountant
// design §5.2): the proxy passes no PingRead.
func TestThePingRescueIsCreditedFromTheUsage(t *testing.T) {
	st, pings := newSavingsTracker(), NewPingStore()
	t0 := time.Unix(1_700_000_000, 0)
	body := func(n int) []byte {
		msgs := `[{"role":"user","content":"` + strings.Repeat("x", 4000) + `","cache_control":{"type":"ephemeral"}}`
		for i := 1; i < n; i++ {
			msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"more","cache_control":{"type":"ephemeral"}}`
		}
		return []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":` + msgs + `]}`)
	}
	turn := func(at time.Time, b []byte, u Usage) accountant.Result {
		req := st.begin(accountant.RequestMeta{RequestID: at.String(), Session: "s", Model: "m", At: at}, b)
		th := accountant.Thread{ID: "main"}
		// The lifetime comes from the production rule, not set by hand: an unknown lifetime would
		// never rescue after a gap past the TTL.
		f := entryFacts(bschemas.Anthropic, "m", b, pings, "s", th)
		if f.Lifetime != accountant.LifetimeExact {
			t.Fatalf("lifetime %v, want exact", f.Lifetime)
		}
		st.end(req, th, b, nil, f)
		return st.compute(req, u)
	}
	b1 := body(1)
	turn(t0, b1, Usage{CacheWrite: int64(len(b1))})
	r := okReport()
	r.Session, r.Thread, r.TS = "s", "", t0.Add(4*time.Minute).UnixMilli()
	r.Window = t0
	r.FreshInput, r.CacheRead, r.CacheWrite, r.Output = 0, int64(len(b1)), 0, 1
	if _, ok := PingSavings(r, pings); !ok {
		t.Fatal("the ping was not converted")
	}
	b2 := body(2)
	res := turn(t0.Add(8*time.Minute), b2, Usage{CacheRead: int64(len(b1)), CacheWrite: int64(len(b2) - len(b1))})
	if res.Phase != accountant.PhaseHot {
		t.Fatalf("phase %q, want hot: the ping renewed the entry", res.Phase)
	}
	var rescue *accountant.CacheEffect
	for i := range res.CacheEffects {
		if res.CacheEffects[i].Kind == accountant.EffectRescue {
			rescue = &res.CacheEffects[i]
		}
	}
	if rescue == nil || rescue.Saved.CacheWrite5m == 0 || rescue.ChangeID == "" {
		t.Fatalf("effects %+v, want a rescue on the ping's window", res.CacheEffects)
	}
}

// The facts carry the lifetime the cold gate uses, per provider.
func TestEntryFactsTakeTheLifetimeFromTheProvider(t *testing.T) {
	th := accountant.Thread{ID: "main"}
	if f := entryFacts(bschemas.Anthropic, "claude-sonnet-5", nil, nil, "s", th); f.Lifetime != accountant.LifetimeExact || f.TTL != 5*time.Minute {
		t.Fatalf("anthropic: %+v", f)
	}
	one := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
	if f := entryFacts(bschemas.Anthropic, "claude-sonnet-5", one, nil, "s", th); f.TTL != time.Hour {
		t.Fatalf("every breakpoint asks 1h: %+v", f)
	}
	// A 1h head over a 5m tail: the tail expires in five minutes, so the entry's TTL is five.
	mixed := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"t","cache_control":{"type":"ephemeral"}}]}]}`)
	if f := entryFacts(bschemas.Anthropic, "claude-sonnet-5", mixed, nil, "s", th); f.TTL != 5*time.Minute {
		t.Fatalf("a mixed ask keeps the five-minute TTL: %+v", f)
	}
	if f := entryFacts("", "m", nil, nil, "s", th); f.Lifetime != accountant.LifetimeUnknown {
		t.Fatalf("unknown provider: %+v", f)
	}
}

// A request that books nothing leaves the ping facts for the next one: the facts are read when the
// request ends and spent only when it is priced.
func TestPingFactsSurviveARequestThatBooksNothing(t *testing.T) {
	st := NewPingStore()
	PingSavings(okReport(), st)
	key := accPingKey("s", "main")
	if _, ok := st.peek(key); !ok {
		t.Fatal("no facts after the ping")
	}
	// Reading the facts for a request (entryFacts) does not spend them.
	entryFacts("", "", nil, st, "s", accountant.Thread{ID: "main"})
	if _, ok := st.peek(key); !ok {
		t.Fatal("entryFacts spent the ping facts: a request that books nothing would lose them")
	}
	st.drop(key, "")
	if _, ok := st.peek(key); ok {
		t.Fatal("drop kept the facts")
	}
	st.drop(key, "") // idempotent
	var nilStore *PingStore
	nilStore.drop(key, "")
}

// A ping that arrives while a request is in flight is not spent by that request: finish drops only
// the facts the request read, and the newer ping stays for the next one.
func TestDropKeepsANewerPing(t *testing.T) {
	st := NewPingStore()
	key := accPingKey("s", "main")
	st.put(key, accountant.PingFacts{PingWindow: "w1", LastPing: pingAt})
	read, _ := st.peek(key) // the request reads window w1 ...
	st.put(key, accountant.PingFacts{PingWindow: "w2", LastPing: pingAt.Add(time.Minute)})
	st.drop(key, read.PingWindow) // ... and is priced after w2 arrived
	f, ok := st.peek(key)
	if !ok || f.PingWindow != "w2" {
		t.Fatalf("the newer ping was dropped with the older one: %+v %v", f, ok)
	}
	st.drop(key, "w2")
	if _, ok := st.peek(key); ok {
		t.Fatal("the facts the request read were kept")
	}
}

// The capture spends the ping facts only when it prices the request: finish with no usage (nothing
// computed) leaves them for the next request, finish with usage spends them. A finish that dropped
// them unconditionally, or under another key, fails here.
func TestFinishSpendsThePingFactsOnlyWhenItPrices(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"}]}`)
	run := func(usage Usage, usageOK bool) (spent bool) {
		rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rec.Close() })
		pings := NewPingStore()
		PingSavings(okReport(), pings)
		key := accPingKey("s", "main")
		if _, ok := pings.peek(key); !ok {
			t.Fatal("the ping stored nothing")
		}
		c := &capture{rec: rec, savings: newSavingsTracker(), pings: pings, tenant: "t1", provider: "anthropic",
			model: "m", route: "/v1/messages", start: time.Now(), status: 200, trace: apply.Trace{Session: "s"}}
		c.accBegin(accountant.RequestMeta{RequestID: "r1", Session: "s", Model: "m", At: time.Now()}, body)
		c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
		if _, ok := pings.peek(key); !ok {
			t.Fatal("accEnd spent the facts before the request was priced")
		}
		c.finish(usage, usageOK, false, 0, 0)
		_, still := pings.peek(key)
		return !still
	}
	if run(Usage{}, false) {
		t.Error("a request with no usage spent the ping facts: the next request loses the ping gap")
	}
	if !run(Usage{FreshInput: 10, CacheWrite: 100}, true) {
		t.Error("a priced request left the ping facts for the next one")
	}
}

// A manual ping is priced, but its facts are not stored: its avoided write is already on its row as
// ManualSavedUSD, and a stored fact would let the next real request credit the same write again as a
// rescue (context-guru#768 B3).
func TestPingSavingsDoesNotStoreFactsForAManualPing(t *testing.T) {
	st := NewPingStore()
	r := okReport()
	r.Manual = true
	res, ok := PingSavings(r, st)
	if !ok || len(res.Ops) != 1 || res.Ops[0].Call == nil {
		t.Fatalf("a manual ping must still be priced: ok=%v res=%+v", ok, res)
	}
	if f, ok := st.take(accPingKey("s", "main")); ok {
		t.Fatalf("a manual ping stored facts %+v: the next request would credit its write twice", f)
	}
	// The automatic ping next to it still stores them.
	if _, ok := PingSavings(okReport(), st); !ok {
		t.Fatal("an automatic ping was not converted")
	}
	if _, ok := st.take(accPingKey("s", "main")); !ok {
		t.Fatal("an automatic ping stored no facts")
	}
}

// The report's TS is the completion time. The library's entry gap runs from the START of the ping, so the mapped
// At must be TS minus the ping's duration (context-guru#768 minor).
func TestPingAtIsTheStartOfThePing(t *testing.T) {
	r := components.KeepAliveReport{TS: 1_700_000_100_000, DurationMs: 90_000}
	if got, want := pingFromReport(r).At, time.UnixMilli(1_700_000_010_000); !got.Equal(want) {
		t.Fatalf("At = %v, want %v (completion minus 90 s)", got, want)
	}
	if got := pingFromReport(components.KeepAliveReport{TS: 1_700_000_100_000}).At; !got.Equal(time.UnixMilli(1_700_000_100_000)) {
		t.Fatalf("a ping with no duration: At = %v, want TS", got)
	}
}
