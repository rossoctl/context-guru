package tooldefer

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	cgschema "github.com/rossoctl/context-guru/schema"
)

func pipeFor(t *testing.T, doc string) (on bool, keep, always []string) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(metrics.NewAggregator())
	if err != nil {
		t.Fatal(err)
	}
	return PipeOn(pipe)
}

// DocOn and PipeOn answer "is deferral on" the same way for the same document.
func TestDocOnAndPipeOnAgree(t *testing.T) {
	for doc, want := range map[string]bool{
		"preset: ultrahigh\n": true,
		"preset: xhigh\n":     false,
		"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n": true,
		"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: off\n":  false,
		"pipeline: [format]\ncomponents:\n  toolfilter:\n    defer: auto\n":     false,
		"not: [yaml": false,
		"":           false,
	} {
		if got := DocOn(doc); got != want {
			t.Errorf("DocOn(%q) = %v, want %v", doc, got, want)
		}
		if cfg, err := config.LoadBytes([]byte(doc)); err == nil && cfg != nil {
			if on, _, _ := pipeFor(t, doc); on != want {
				t.Errorf("PipeOn(%q) = %v, want %v", doc, on, want)
			}
		}
	}
	if on, k, a := PipeOn(nil); on || k != nil || a != nil {
		t.Fatal("a nil pipeline defers")
	}
	on, keep, always := pipeFor(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    defer: auto\n    keep: [K]\n    always_defer: [mcp__b, A]\n")
	if !on || strings.Join(keep, ",") != "K" || strings.Join(always, ",") != "A,mcp__b" {
		t.Fatalf("on %v keep %v always %v", on, keep, always)
	}
}

func TestRepairFromError(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash","description":"d","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"tool_reference","tool_name":"mcp__gone__lookup"}]}]}]}`)
	fixed, ok := RepairFromError([]byte(`{"type":"error","error":{"message":"Tool reference 'mcp__gone__lookup' not found in available tools"}}`), body)
	if !ok || !strings.Contains(string(fixed), "mcp__gone__lookup") || string(fixed) == string(body) {
		t.Fatalf("ok=%v fixed=%s", ok, fixed)
	}
	if _, ok := RepairFromError([]byte(`{"error":{"message":"messages.3: text content blocks must be non-empty"}}`), body); ok {
		t.Fatal("repaired an unrelated error")
	}
	// A tool the body already declares is not stubbed again.
	if _, ok := RepairFromError([]byte(`Tool reference 'Bash' not found in available tools`), body); ok {
		t.Fatal("stubbed a declared tool")
	}
}

func TestSearchTallyAdds(t *testing.T) {
	msg := `{"content":[{"type":"server_tool_use","id":"s1","name":"tool_search_tool_bm25","input":{}},` +
		`{"type":"server_tool_use","id":"s2","name":"tool_search_tool_bm25","input":{}},` +
		`{"type":"tool_search_tool_result","tool_use_id":"s2","content":{"tool_references":[{"type":"tool_reference","tool_name":"Monitor"}]}}]}`
	var st SearchTally
	st.Add(false, []byte(msg))
	st.Add(false, []byte(`{"content":[{"type":"text","text":"no search here"}]}`))
	if st.N != 2 || st.Dropped != 1 || strings.Join(st.Loaded, ",") != "Monitor" {
		t.Fatalf("%+v", st)
	}
}

type onePrice struct{ ok bool }

func (p onePrice) Price(context.Context, string) (modelinfo.Price, bool) {
	return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, p.ok
}

func TestProbeAllowed(t *testing.T) {
	ctx := context.Background()
	if ProbeAllowed(ctx, nil, "claude-sonnet-5-5") {
		t.Fatal("probed with no price table")
	}
	if ProbeAllowed(ctx, onePrice{ok: false}, "unknown") {
		t.Fatal("probed a model with no price")
	}
	if !ProbeAllowed(ctx, onePrice{ok: true}, "claude-sonnet-5-5") {
		t.Fatal("a priced model is not probed")
	}
	if ProbeKind(ProbeKey{Route: "/v1/messages"}) != "server" || ProbeKind(ProbeKey{Route: "/v1/messages" + ClientProbeRoute}) != "client" {
		t.Fatal("ProbeKind")
	}
}

func testFlow(t *testing.T, doc string) (Flow, Parts) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := cfg.Build(metrics.NewAggregator())
	if err != nil {
		t.Fatal(err)
	}
	p := Parts{Env: Env{Gate: NewGate(), Prober: NewProber()}, Hist: NewMem(nil), Ledger: &Ledger{}}
	return Flow{Tenant: "t", Pipe: pipe, Provider: bschemas.Anthropic, Wire: cgschema.WireAnthropic,
		Upstream: "u", Route: "/v1/messages", Parts: func() Parts { return p }, Prices: onePrice{ok: true}}, p
}

// The ledger books a finished request once, and not at all without usage or without a changed request.
func TestFlowBook(t *testing.T) {
	f, p := testFlow(t, "preset: ultrahigh\n")
	di := &Info{Tenant: "t", Model: "claude-sonnet-5-5", Outcome: Outcome{DeferredTokens: 1000, ListTokens: 100}}
	if _, ok := f.Book(context.Background(), nil, Response{}, true, SearchTally{}); ok {
		t.Fatal("booked a request deferral did not change")
	}
	if _, ok := f.Book(context.Background(), di, Response{}, false, SearchTally{}); ok {
		t.Fatal("booked a request with no usage")
	}
	b, ok := f.Book(context.Background(), di, Response{CacheRead: 5000}, true, SearchTally{})
	if !ok || b.Request.DeferredTokens != 1000 || b.Request.Tier != TierRead || b.Money.Net <= 0 {
		t.Fatalf("ok=%v %+v", ok, b)
	}
	if s := p.Ledger.Snapshot(); s.Requests != 1 || s.DeferredTokens != 1000 {
		t.Fatalf("ledger = %+v", s)
	}
	b, _ = f.Book(context.Background(), di, Response{Searches: 2, SearchLoaded: []string{"A", "A", "B"}}, true, SearchTally{N: 1, Dropped: 1, Loaded: []string{"C"}})
	if b.Request.Searches != 3 || b.Request.DroppedSearches != 1 || b.Distinct != 3 {
		t.Fatalf("%+v", b)
	}
}

// A refusal where the original succeeds turns deferral off for the model; one where it fails too does not.
func TestFlowRetry(t *testing.T) {
	f, p := testFlow(t, "preset: ultrahigh\n")
	di := &Info{Tenant: "t", Model: "m", Probe: ProbeKey{Upstream: "u", Model: "m", Route: "r"}}
	f.Retry(di, 400, nil, false)
	if p.Env.Gate.Retries.Load() != 1 || p.Env.Gate.Broken("t", "m") {
		t.Fatal("a refusal the original shares turned deferral off")
	}
	f.Retry(di, 400, nil, true)
	if !p.Env.Gate.Broken("t", "m") {
		t.Fatal("deferral stayed on after the original succeeded")
	}
}

// The flow runs the probe the preset picker asked for, once, and defers nothing when it is off.
func TestFlowApplyGates(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"hi"}]}`)
	var probes []ProbeKey
	asks := 1
	f, _ := testFlow(t, "preset: xhigh\n")
	f.TakeProbeAsk = func() bool { asks--; return asks == 0 }
	f.StartProbe = func(k ProbeKey) { probes = append(probes, k) }
	got, di := f.Apply("s", "", body, body)
	if di != nil || string(got) != string(body) {
		t.Fatal("deferral off changed the request")
	}
	if len(probes) != 1 || probes[0] != (ProbeKey{Upstream: "u", Model: "claude-sonnet-5-5", Route: "/v1/messages"}) {
		t.Fatalf("probes = %v", probes)
	}
	f.Apply("s", "", body, body) // the ask was taken
	if len(probes) != 1 {
		t.Fatalf("a second probe ran: %v", probes)
	}
	// Another provider's wire: no probe, no change, even with an ask pending.
	f.TakeProbeAsk = func() bool { return true }
	f.Provider = bschemas.Gemini
	f.Wire = "gemini"
	if _, di := f.Apply("s", "", body, body); di != nil || len(probes) != 1 {
		t.Fatalf("a foreign wire reached deferral: %v %v", di, probes)
	}
}

func flowOver(t *testing.T, doc string, e Env, provider bschemas.ModelProvider, wire string) Flow {
	t.Helper()
	f, _ := testFlow(t, doc)
	f.Provider, f.Wire = provider, wire
	f.Route = "r"
	f.Upstream = "u"
	f.Parts = func() Parts { return Parts{Env: e, Hist: NewMem(nil), Ledger: &Ledger{}} }
	return f
}

// A host with no probe callback still works: a model with no verdict is never deferred, and the
// request goes out unchanged. With a verdict the same flow defers.
func TestFlowApplyWithoutAProbeCallbackDefersNothing(t *testing.T) {
	e := env(t, threeUnused())
	f := flowOver(t, "preset: ultrahigh\n", e, bschemas.Anthropic, cgschema.WireAnthropic)
	f.StartProbe = nil
	f.TakeProbeAsk = func() bool { return true } // the ask path starts a probe too
	in := body(catalogue(), "")
	got, di := f.Apply("s", "", in, in)
	if di != nil || string(got) != string(in) {
		t.Fatalf("a model with no verdict was changed: %v", di)
	}
	// Control: with a verdict the flow defers, so the test above reached the probe step.
	passProbe(e, ProbeKey{"u", "claude-sonnet-5-5", "r"})
	if _, di := f.Apply("s2", "", in, in); di == nil || !di.Outcome.Changed {
		t.Fatal("control: a probed model was not deferred")
	}
}

// Our own search on a Chat Completions request: the flow decides, rewrites, keeps the thread's
// facts in Info, and books a forced tool as an add-back.
func TestFlowApplyClientSearch(t *testing.T) {
	e := env(t, unusedHist())
	f := flowOver(t, "preset: ultrahigh\n", e, bschemas.OpenAI, cgschema.WireOpenAI)
	in := chatBody()
	got, di := f.Apply("s", "", in, in)
	if di == nil || !di.Search || di.Session != "s" || di.Tenant != "t" || di.Model != "gpt-5.5" ||
		strings.Join(di.Outcome.Deferred, ",") != "Monitor,Workflow" {
		t.Fatalf("di = %+v", di)
	}
	if string(di.Orig) != string(in) || wireToolNames(cgschema.WireOpenAI, got) != "Bash,Read,"+ClientSearchName {
		t.Fatalf("tools = %s", wireToolNames(cgschema.WireOpenAI, got))
	}
	forced := []byte(strings.Replace(string(chatBody()), `"messages":`, `"tool_choice":{"type":"function","function":{"name":"Workflow"}},"messages":`, 1))
	got, di = f.Apply("s", "", forced, forced)
	if di == nil || !strings.Contains(wireToolNames(cgschema.WireOpenAI, got), "Workflow") {
		t.Fatalf("forced tool not declared: %s", got)
	}
	if e.Gate.AddBacks.Load() != 1 {
		t.Fatalf("add-backs = %d, want 1", e.Gate.AddBacks.Load())
	}
	// Off: nothing changes.
	off := flowOver(t, "preset: xhigh\n", e, bschemas.OpenAI, cgschema.WireOpenAI)
	if got, di := off.Apply("s9", "", in, in); di != nil || string(got) != string(in) {
		t.Fatal("deferral off changed a client-search request")
	}
}
