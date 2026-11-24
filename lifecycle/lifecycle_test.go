package lifecycle

import (
	"bytes"
	"context"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
)

func TestCompactionGate(t *testing.T) {
	for _, c := range []struct {
		name          string
		header, agent bool
		n             int
		wantBy        bool
		wantGate      string
	}{
		{"plain request", false, false, 10, false, ""},
		{"header bypass", true, false, 10, true, ""},
		{"header bypass beats a compaction", true, true, 10, true, ""},
		{"compaction detected", false, true, 10, false, GateDetected},
		{"compaction at the limit", false, true, 100, false, GateDetected},
		{"compaction over the limit", false, true, 101, true, GateOversizeBypass},
	} {
		by, gate := CompactionGate(c.header, c.agent, c.n, 100)
		if by != c.wantBy || gate != c.wantGate {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, by, gate, c.wantBy, c.wantGate)
		}
	}
}

func TestObservingAndEnforcing(t *testing.T) {
	if !Observing(components.ModeObserve, false) || Observing(components.ModeObserve, true) || Observing(components.ModeSync, false) {
		t.Error("Observing: only an unbypassed observe request is observed")
	}
	if Enforcing(components.ModeObserve, false) || Enforcing(components.ModeSync, true) || !Enforcing(components.ModeSync, false) {
		t.Error("Enforcing: only an unbypassed non-observe request is rewritten")
	}
}

type fakePricer map[string]modelinfo.Price

func (f fakePricer) Price(_ context.Context, m string) (modelinfo.Price, bool) {
	p, ok := f[m]
	return p, ok
}

func TestRates(t *testing.T) {
	p := fakePricer{"m": {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25}, "zero": {}}
	ctx := context.Background()
	if got := SelfRates(ctx, p, "m"); got != (components.TokenRates{Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25}) {
		t.Errorf("SelfRates = %+v", got)
	}
	for _, m := range []string{"", "unknown", "zero"} {
		if got := SelfRates(ctx, p, m); got != (components.TokenRates{}) {
			t.Errorf("SelfRates(%q) = %+v, want the zero value (unknown, never free)", m, got)
		}
	}
	if SelfRates(ctx, nil, "m") != (components.TokenRates{}) || RatesFor(ctx, nil) != nil {
		t.Error("a nil pricer gives no rates and no lookup")
	}
	if got := RatesFor(ctx, p)("m"); got.Input != 1 {
		t.Errorf("RatesFor lookup = %+v", got)
	}
}

type plainWindow struct{ w int }

func (p plainWindow) Window(context.Context, string) (int, bool) { return p.w, p.w > 0 }

type exactWindow struct{ w int }

func (e exactWindow) Window(context.Context, string) (int, bool) { return e.w, e.w > 0 }
func (e exactWindow) WindowExact(context.Context, string) (int, bool, bool) {
	return e.w, true, e.w > 0
}
func (e exactWindow) CompactionPoint(context.Context, string) (int, bool) { return 123456, true }

func TestResolveWindow(t *testing.T) {
	ctx := context.Background()
	if w, ex := ResolveWindow(ctx, nil, "m"); w != 0 || ex {
		t.Errorf("nil resolver = (%d, %v)", w, ex)
	}
	if w, ex := ResolveWindow(ctx, plainWindow{200000}, "m"); w != 200000 || ex {
		t.Errorf("a resolver without the exact capability never claims exactness: (%d, %v)", w, ex)
	}
	if w, ex := ResolveWindow(ctx, exactWindow{1000000}, "m"); w != 1000000 || !ex {
		t.Errorf("exact resolver = (%d, %v)", w, ex)
	}
	if w, ex := ResolveWindow(ctx, exactWindow{0}, "m"); w != 0 || ex {
		t.Errorf("unknown = (%d, %v)", w, ex)
	}
}

func TestResolveCompactionPointUsesTheOperatorFigure(t *testing.T) {
	ctx := context.Background()
	got := ResolveCompactionPoint(ctx, exactWindow{1000000}, "claude-opus-5", 1000000)
	if got.Tokens(1000000) != 123456 {
		t.Errorf("operator figure ignored: %d", got.Tokens(1000000))
	}
	if ResolveCompactionPoint(ctx, nil, "claude-opus-5", 1000000).Tokens(1000000) == 123456 {
		t.Error("a nil resolver must not state a figure")
	}
}

func TestRequestOptsCarriesTheSharedFields(t *testing.T) {
	q := Request{Provider: bschemas.Anthropic, API: "a", Body: []byte("b"), Session: "s", Tenant: "t",
		Window: 5, WindowExact: true, CompactionPoint: 7, CompactionPointSource: "measured", CacheMode: "on"}
	o := q.Opts()
	if o.Provider != q.Provider || o.API != "a" || string(o.Body) != "b" || o.Session != "s" || o.Tenant != "t" ||
		o.Window != 5 || !o.WindowExact || o.CompactionPoint != 7 || o.CompactionPointSource != "measured" || o.CacheMode != "on" {
		t.Errorf("Opts lost a field: %+v", o)
	}
	if o.Bypass || o.ThreadOf != nil || o.Mode != "" {
		t.Error("the caller's own fields must stay unset")
	}
}

const toolsBody = `{"model":"m","tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`

func preFor(mode components.Mode, bypassed bool, log *[]string) Pre {
	orig := []byte(toolsBody)
	return Pre{
		Wire: schema.WireFor(string(bschemas.Anthropic), ""), Provider: bschemas.Anthropic, Mode: mode, Bypassed: bypassed,
		Pipe: components.NewPipeline(nil, nil), Store: persists(true), InjectExpand: expand.InjectAlways, Orig: orig,
		MakeThreadOf: func() func(string) thread.Result {
			*log = append(*log, "makeThreadOf")
			return func(s string) thread.Result { *log = append(*log, "threadOf:"+s); return thread.Result{ID: "t1"} }
		},
		RepairExpand: func(b []byte) ([]byte, int) { *log = append(*log, "repair"); return b, 1 },
		OnRepair:     func(int) { *log = append(*log, "onrepair") },
		Apply: func(b []byte, _ func(string) thread.Result) ([]byte, time.Duration, apply.Trace) {
			*log = append(*log, "apply")
			return b, time.Millisecond, apply.Trace{Session: "s"}
		},
		Applied:  func(apply.Trace, time.Duration) { *log = append(*log, "applied") },
		Observed: func(th thread.Result, tr apply.Trace, _ time.Duration) { *log = append(*log, "observed:"+th.ID) },
		Arrive:   func(s, th string) { *log = append(*log, "arrive:"+s+"/"+th) },
		Settled:  func(apply.Trace, time.Duration) { *log = append(*log, "settled") },
		ToolDefer: func(_ *apply.Trace, _ thread.Result, b []byte) []byte {
			*log = append(*log, "defer")
			return b
		},
		Done: func(_ thread.Result, _ []byte, _ apply.Trace) { *log = append(*log, "done") },
	}
}

type persists bool

func (p persists) Persists() bool { return bool(p) }

func join(l []string) string {
	var b bytes.Buffer
	for i, s := range l {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}

func TestPrepareOrder(t *testing.T) {
	var log []string
	out := Prepare(preFor(components.ModeSync, false, &log))
	// The thread comes from apply when it resolved it; here the fake apply did not, so Prepare resolves it.
	want := "repair onrepair makeThreadOf apply applied threadOf:s observed:t1 arrive:s/t1 settled defer done"
	if got := join(log); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
	if out.Panic != nil || out.ThreadID != "t1" || out.Trace.Session != "s" || out.Added != time.Millisecond {
		t.Errorf("result: %+v", out)
	}
	if !expand.HasTool(schema.WireFor(string(bschemas.Anthropic), ""), out.Body) {
		t.Error("the expand tool was not injected")
	}
	if string(out.PreBody) != toolsBody {
		t.Error("PreBody must be the body the pipeline received")
	}
}

func TestPrepareDoesNotRewriteInObserveOrBypass(t *testing.T) {
	for _, c := range []struct {
		mode     components.Mode
		bypassed bool
	}{{components.ModeObserve, false}, {components.ModeSync, true}} {
		var log []string
		out := Prepare(preFor(c.mode, c.bypassed, &log))
		want := "makeThreadOf apply applied threadOf:s observed:t1 arrive:s/t1 settled done"
		if got := join(log); got != want {
			t.Errorf("%v bypass=%v: order\n got %s\nwant %s", c.mode, c.bypassed, got, want)
		}
		if string(out.Body) != toolsBody {
			t.Errorf("%v bypass=%v: the body must stay byte-identical", c.mode, c.bypassed)
		}
	}
}

func TestPrepareUsesTheThreadApplyResolved(t *testing.T) {
	var log []string
	p := preFor(components.ModeSync, false, &log)
	p.Apply = func(b []byte, _ func(string) thread.Result) ([]byte, time.Duration, apply.Trace) {
		return b, 0, apply.Trace{Session: "s", Thread: thread.Result{ID: "fromapply"}, ThreadResolved: true}
	}
	out := Prepare(p)
	if out.ThreadID != "fromapply" || bytes.Contains([]byte(join(log)), []byte("threadOf")) {
		t.Errorf("thread %q, log %s", out.ThreadID, join(log))
	}
}

func TestPrepareFailsOpenOnAPanic(t *testing.T) {
	for _, stage := range []string{"repair", "apply", "observed", "arrive", "defer"} {
		var log []string
		p := preFor(components.ModeSync, false, &log)
		boom := func() { panic("boom") }
		switch stage {
		case "repair":
			p.RepairExpand = func([]byte) ([]byte, int) { boom(); return nil, 0 }
		case "apply":
			p.Apply = func([]byte, func(string) thread.Result) ([]byte, time.Duration, apply.Trace) {
				boom()
				return nil, 0, apply.Trace{}
			}
		case "observed":
			p.Observed = func(thread.Result, apply.Trace, time.Duration) { boom() }
		case "arrive":
			p.Arrive = func(string, string) { boom() }
		case "defer":
			p.ToolDefer = func(*apply.Trace, thread.Result, []byte) []byte { boom(); return nil }
		}
		out := Prepare(p)
		if out.Panic == nil || string(out.Body) != toolsBody {
			t.Errorf("%s: panic=%v body=%q, want the original body", stage, out.Panic, out.Body)
		}
	}
}

func TestInjectGates(t *testing.T) {
	wire := schema.WireFor(string(bschemas.Anthropic), "")
	pipe := components.NewPipeline(nil, nil)
	b := []byte(toolsBody)
	// Auto injects only when the pipeline can mint a marker; this pipeline has no offload.
	if out := Inject(wire, bschemas.Anthropic, pipe, expand.InjectAuto, true, b); !bytes.Equal(out, b) {
		t.Error("auto must not inject for a pipeline without an offload")
	}
	if out := Inject(wire, bschemas.Anthropic, pipe, expand.InjectAlways, true, b); !expand.HasTool(wire, out) {
		t.Error("always must inject")
	}
	if out := Inject(wire, bschemas.Anthropic, pipe, expand.InjectNever, true, b); !bytes.Equal(out, b) {
		t.Error("never must not inject")
	}
}

func newStore() store.Store { return store.NewMemory(store.Options{}) }

func billedOf(st store.Store, key string) string {
	b, _ := st.Get(key)
	return string(b)
}

func TestFinishOrderAndBilledInput(t *testing.T) {
	st := newStore()
	var log []string
	Finish(Fin{
		Store: st, Mode: components.ModeSync, Session: "s", ThreadID: "t1", UsageOK: true, Billed: 1234,
		Touch:  func() { log = append(log, "touch") },
		Ledger: func() { log = append(log, "ledger") },
	})
	if got := join(log); got != "touch ledger" {
		t.Errorf("order: %s", got)
	}
	if billedOf(st, store.BilledPrefix+"s") != "1234" {
		t.Error("the session's billed input was not kept")
	}
	if billedOf(st, store.BilledThreadPrefix+thread.Key("s", "t1")) != "1234" {
		t.Error("the thread's billed input was not kept under thread.Key(session, thread)")
	}
}

func TestFinishKeepsThePreviousFigureWithoutUsage(t *testing.T) {
	st := newStore()
	apply.RecordBilledInput(st, "s", 99)
	ran := false
	Finish(Fin{Store: st, Session: "s", ThreadID: "t1", UsageOK: false, Billed: 777, Touch: func() { ran = true }})
	if billedOf(st, store.BilledPrefix+"s") != "99" {
		t.Error("a response without usage must leave the previous figure")
	}
	if !ran {
		t.Error("Touch runs whether or not usage is OK")
	}
}

func TestFinishObserveWritesTheShadowStore(t *testing.T) {
	live, shadow := newStore(), newStore()
	Finish(Fin{
		Store: live, Shadow: shadow, Mode: components.ModeObserve, Session: "", ThreadID: thread.Primary,
		UsageOK: true, Billed: 500,
		ObserveSession: func() string { return "sid" },
		ObserveThread:  func(string) thread.Result { return thread.Result{ID: "obs"} },
	})
	if billedOf(shadow, store.BilledPrefix+"sid") != "500" {
		t.Error("shadow session record missing")
	}
	if billedOf(shadow, store.BilledThreadPrefix+thread.Key("sid", "obs")) != "500" {
		t.Error("shadow thread record must use the resolved observe thread")
	}
	if billedOf(live, store.BilledPrefix+"sid") != "" {
		t.Error("the live store must stay clean in observe mode")
	}
	// An enforcing request never writes the shadow store.
	shadow2 := newStore()
	Finish(Fin{Store: live, Shadow: shadow2, Mode: components.ModeSync, Session: "s", ThreadID: "m", UsageOK: true, Billed: 1,
		ObserveSession: func() string { return "sid" }})
	if billedOf(shadow2, store.BilledPrefix+"sid") != "" {
		t.Error("shadow written outside observe mode")
	}
}

// The order of side effects is the contract. These three pins each fail when two steps swap.

func TestPrepareOrderPinsThreadFallbackAfterTheApplyNotes(t *testing.T) {
	var log []string
	Prepare(preFor(components.ModeSync, false, &log))
	if i, j := index(log, "applied"), index(log, "threadOf:s"); i < 0 || j < 0 || i > j {
		t.Errorf("the thread fallback must run after the latency and trace notes: %s", join(log))
	}
}

func TestPrepareOrderPinsMakeThreadOfAfterRepairBeforeApply(t *testing.T) {
	var log []string
	Prepare(preFor(components.ModeSync, false, &log))
	r, m, a := index(log, "repair"), index(log, "makeThreadOf"), index(log, "apply")
	if r < 0 || !(r < m && m < a) {
		t.Errorf("makeThreadOf must run after repair and before apply: %s", join(log))
	}
}

func index(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}

func TestFinishOrderPinsCountersBeforeBilledInput(t *testing.T) {
	st := newStore()
	var log []string
	Finish(Fin{
		Store: st, Session: "s", ThreadID: "t1", UsageOK: true, Billed: 10,
		Touch:  func() { log = append(log, "touch") },
		Ledger: func() { log = append(log, "ledger") },
		Counters: func() {
			if billedOf(st, store.BilledPrefix+"s") != "" {
				log = append(log, "counters-after-billed")
			}
			log = append(log, "counters")
		},
	})
	if got := join(log); got != "touch ledger counters" {
		t.Errorf("the counters run after the ledger and before the billed-input writes: %s", got)
	}
}
