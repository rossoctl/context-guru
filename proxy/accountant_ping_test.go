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

// Issue #1079. Two threads of one session: the main thread is pinged, and a sub-agent thread sends a
// request that shares the main thread's first message inside the TTL. That prefix was warm without the
// ping, so the rescue credits only the part past it (accountant R54). The shared prefix comes from
// accountant.SharedWarmFrom through siblingStore, end to end: no hand-set EntryFacts.SharedWarm.
func TestARescueLeavesOutThePrefixASiblingKeptWarm(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	user := func(c byte) string {
		return `{"role":"user","content":[{"type":"text","text":"` + strings.Repeat(string(c), 2000) +
			`","cache_control":{"type":"ephemeral"}}]}`
	}
	asst := `{"role":"assistant","content":"ok"}`
	body := func(msgs ...string) []byte {
		return []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":[` + strings.Join(msgs, ",") + `]}`)
	}
	mainBody := body(user('x'), asst, user('y'))
	grown := body(user('x'), asst, user('y'), asst, user('z'))
	subBody := body(user('x'), asst, user('q')) // shares the system block and message 0 with the main thread

	rescueAfter := func(withSibling bool) accountant.CacheEffect {
		st, pings := newSavingsTracker(), NewPingStore()
		turn := func(thread string, at time.Time, b []byte, u Usage) accountant.Result {
			req := st.begin(accountant.RequestMeta{RequestID: thread + at.String(), Session: "s", Model: "m", At: at}, b)
			th := accountant.Thread{ID: thread}
			f := entryFacts(bschemas.Anthropic, "m", b, pings, "s", th)
			st.end(req, th, b, nil, f)
			return st.compute(req, u)
		}
		turn("main", t0, mainBody, Usage{CacheWrite: int64(len(mainBody))})
		r := okReport()
		r.Session, r.Thread, r.TS, r.Window = "s", "", t0.Add(4*time.Minute).UnixMilli(), t0
		r.FreshInput, r.CacheRead, r.CacheWrite, r.Output = 0, int64(len(mainBody)), 0, 1
		if _, ok := PingSavings(r, pings); !ok {
			t.Fatal("the ping was not converted")
		}
		if withSibling {
			turn("sub", t0.Add(5*time.Minute), subBody, Usage{CacheWrite: int64(len(subBody))})
		}
		res := turn("main", t0.Add(8*time.Minute), grown,
			Usage{CacheRead: int64(len(mainBody)), CacheWrite: int64(len(grown) - len(mainBody))})
		for _, e := range res.CacheEffects {
			if e.Kind == accountant.EffectRescue {
				return e
			}
		}
		return accountant.CacheEffect{}
	}
	alone, shared := rescueAfter(false), rescueAfter(true)
	if alone.Saved.CacheWrite5m <= 0 {
		t.Fatalf("control: no rescue without a sibling: %+v", alone)
	}
	if shared.Saved.CacheWrite5m <= 0 || shared.Saved.CacheWrite5m >= alone.Saved.CacheWrite5m {
		t.Fatalf("rescue with a warm sibling = %d tokens, alone = %d: want a smaller credit, the part past the shared prefix",
			shared.Saved.CacheWrite5m, alone.Saved.CacheWrite5m)
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

// A gateway that asks for 1h and bills 5m must not make a 20-minute gap look hot (context-guru#919).
// The entry TTL is lowered to the billed tier after the usage arrives, before the library prices the
// request. A request billed 1h, or one that wrote nothing, keeps the asked 1h.
func TestTheEntryTTLFollowsTheBilledTier(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	body := []byte(`{"model":"claude-sonnet-5","system":[{"type":"text","text":"` + strings.Repeat("x", 4000) +
		`","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	phaseAfter := func(u Usage) accountant.CachePhase {
		st, pings := newSavingsTracker(), NewPingStore()
		step := func(at time.Time, u Usage) accountant.Result {
			c := &capture{savings: st, pings: pings, provider: string(bschemas.Anthropic)}
			c.accBegin(accountant.RequestMeta{RequestID: at.String(), Session: "s", Model: "claude-sonnet-5", At: at}, body)
			c.accEnd(thread.Result{}, body, nil)
			c.accWait()
			if c.accFacts.TTL != time.Hour {
				t.Fatalf("asked 1h on every breakpoint, facts TTL = %v", c.accFacts.TTL)
			}
			c.accBilledTTL(u)
			return st.compute(c.accReq, u)
		}
		step(t0, u)
		// The second turn bills a write, not a read: a billed read would beat the clock (accountant R45).
		return step(t0.Add(20*time.Minute), Usage{CacheWrite: int64(len(body)), CacheWrite1h: int64(len(body))}).Phase
	}
	n := int64(len(body))
	if got := phaseAfter(Usage{CacheWrite: n, CacheWrite1h: 0}); got == accountant.PhaseHot {
		t.Errorf("billed 5m, gap 20m: phase = %q, want not hot (the entry expired after 5 minutes)", got)
	}
	if got := phaseAfter(Usage{CacheWrite: n, CacheWrite1h: n}); got != accountant.PhaseHot {
		t.Errorf("billed 1h, gap 20m: phase = %q, want hot", got)
	}
	if got := phaseAfter(Usage{CacheRead: n}); got != accountant.PhaseHot {
		t.Errorf("wrote nothing, gap 20m: phase = %q, want hot (no tier billed: the asked 1h stands)", got)
	}
}

// Issue #1078: a read-only turn bills no tier, so the entry keeps the tier of the thread's last
// write, as keepalive.ResolveTier decides. A gateway that bills 1h as 5m on turn 1 must not look
// like a 1h cache on turn 2 because turn 2 only read: the entry the read found is a 5m one, and
// the library would call a 20-minute gap after it hot and overstate a rescue.
func TestAReadOnlyTurnKeepsTheLastBilledTier(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	body := []byte(`{"model":"claude-sonnet-5","system":[{"type":"text","text":"` + strings.Repeat("x", 4000) +
		`","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	n := int64(len(body))
	phaseAfter := func(first Usage) accountant.CachePhase {
		st, pings := newSavingsTracker(), NewPingStore()
		step := func(at time.Time, u Usage) accountant.Result {
			c := &capture{savings: st, pings: pings, provider: string(bschemas.Anthropic)}
			c.accBegin(accountant.RequestMeta{RequestID: at.String(), Session: "s", Model: "claude-sonnet-5", At: at}, body)
			c.accEnd(thread.Result{}, body, nil)
			c.accWait()
			c.accBilledTTL(u)
			return st.compute(c.accReq, u)
		}
		step(t0, first)
		step(t0.Add(2*time.Minute), Usage{CacheRead: n}) // reads only: no tier billed
		return step(t0.Add(22*time.Minute), Usage{CacheWrite: n, CacheWrite1h: n}).Phase
	}
	if got := phaseAfter(Usage{CacheWrite: n, CacheWrite1h: 0}); got == accountant.PhaseHot {
		t.Errorf("billed 5m, then a read-only turn, gap 20m: phase = %q, want not hot", got)
	}
	if got := phaseAfter(Usage{CacheWrite: n, CacheWrite1h: n}); got != accountant.PhaseHot {
		t.Errorf("billed 1h, then a read-only turn, gap 20m: phase = %q, want hot", got)
	}
}

// A panic in the library while a ping is priced is recovered: the keeper's goroutine must live (issue #918).
func TestPingSavingsRecoversFromALibraryPanic(t *testing.T) {
	origOps, origStd := pingOps, accountantStandalone
	defer func() { pingOps, accountantStandalone = origOps, origStd }()
	pings := NewPingStore()

	pingOps = func(accountant.Ping, accountant.Billing) (accountant.RequestOps, accountant.PingFacts) {
		panic("boom in PingOps")
	}
	if _, ok := PingSavings(okReport(), pings); ok {
		t.Error("a panicking PingOps reported a figure")
	}
	pingOps = origOps
	accountantStandalone = func(accountant.RequestOps) accountant.Result { panic("boom in ComputeStandalone") }
	if _, ok := PingSavings(okReport(), pings); ok {
		t.Error("a panicking ComputeStandalone reported a figure")
	}
	accountantStandalone = origStd
	if _, ok := PingSavings(okReport(), pings); !ok {
		t.Error("after the panics a normal ping was not priced")
	}
}

// context-guru#913: a manual ping stores no facts. The next real request then has no LastPing, and the gap
// from the request before the ping is longer than the TTL. The issue feared a cold phase, with the request priced
// at the write tier although the usage shows a cache read. The library takes a billed read as proof that the
// entry was alive (design §5.2, R45), so the request is hot. The arithmetic: request at t0 writes 100,000
// tokens, a manual ping at t0+4m reads them, a request at t0+8m reads them again. The gap of 8 minutes is past
// the 5 minute TTL, and the read of 100,000 tokens covers the whole extent of the first request.
//
// The control is the same sequence with an automatic ping. It books a rescue; the manual one books none, since
// its avoided write is already on its own row as ManualSavedUSD (an estimate by the user's ask). The two phases
// must agree, and the manual run must carry no unpriced entry and no check failure.
func TestTheRequestAfterAManualPingIsHotNotCold(t *testing.T) {
	const prefix = 100_000
	run := func(manual bool) accountant.Result {
		st, pings := newSavingsTracker(), NewPingStore()
		t0 := time.Unix(1_700_000_000, 0)
		body := func(n int) []byte {
			// 4 bytes a token: the first message is the 100,000-token prefix.
			msgs := `[{"role":"user","content":"` + strings.Repeat("x", prefix*4) + `","cache_control":{"type":"ephemeral"}}`
			for i := 1; i < n; i++ {
				msgs += `,{"role":"assistant","content":"ok"},{"role":"user","content":"more","cache_control":{"type":"ephemeral"}}`
			}
			return []byte(`{"model":"m","system":[{"type":"text","text":"sys"}],"messages":` + msgs + `]}`)
		}
		turn := func(at time.Time, b []byte, u Usage) accountant.Result {
			req := st.begin(accountant.RequestMeta{RequestID: at.String(), Session: "s", Model: "m", At: at}, b)
			th := accountant.Thread{ID: "main"}
			st.end(req, th, b, nil, entryFacts(bschemas.Anthropic, "m", b, pings, "s", th))
			return st.compute(req, u)
		}
		turn(t0, body(1), Usage{CacheWrite: prefix})
		r := okReport()
		r.Session, r.Thread, r.TS, r.Window, r.Manual = "s", "", t0.Add(4*time.Minute).UnixMilli(), t0, manual
		r.FreshInput, r.CacheRead, r.CacheWrite, r.CacheWrite1h, r.Output = 0, prefix, 0, 0, 1
		if _, ok := PingSavings(r, pings); !ok {
			t.Fatal("the ping was not converted")
		}
		return turn(t0.Add(8*time.Minute), body(2), Usage{CacheRead: prefix, CacheWrite: 20})
	}
	auto, manual := run(false), run(true)
	for name, res := range map[string]accountant.Result{"automatic": auto, "manual": manual} {
		if res.Phase != accountant.PhaseHot {
			t.Errorf("%s ping: the next request is %q, want hot", name, res.Phase)
		}
		if len(res.Unpriced) != 0 || len(res.Checks) != 0 {
			t.Errorf("%s ping: unpriced %v checks %v", name, res.Unpriced, res.Checks)
		}
	}
	rescue := func(res accountant.Result) (n int) {
		for _, e := range res.CacheEffects {
			if e.Kind == accountant.EffectRescue {
				n += e.Saved.CacheWrite5m
			}
		}
		return n
	}
	if rescue(auto) == 0 {
		t.Error("the automatic ping booked no rescue: the control is broken")
	}
	if n := rescue(manual); n != 0 {
		t.Errorf("the manual ping's write is booked a second time as a rescue of %d tokens", n)
	}
}
