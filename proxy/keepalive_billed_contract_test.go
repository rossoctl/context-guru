package proxy

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// This test stays in proxy, beside the dash side of the contract it pins; the keep-alive
// substitute tests it borrows its offload half from live in package keepalive. The three
// helpers below are copies of theirs.

// usageSummaryModel is a summary model that BILLS, through the ambient usage sink, as a real
// client does.
type usageSummaryModel struct{ calls atomic.Int64 }

func (m *usageSummaryModel) Complete(ctx context.Context, _ string) (string, error) {
	return m.CompleteMessages(ctx, "", nil)
}
func (m *usageSummaryModel) CompleteMessages(ctx context.Context, _ string, _ []bschemas.ChatMessage) (string, error) {
	m.calls.Add(1)
	cheapmodel.ReplayUsage(ctx, "claude-sonnet-5", 281, 683, 0, 43400)
	return "<summary>explored the handler, 3 tests fail.</summary>", nil
}

// kaCandidate registers a cache_aware_summarizer keep-alive candidate for a session.
func kaCandidate(t *testing.T, session string, st store.Store, model *usageSummaryModel) {
	t.Helper()
	msg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&msg, "fix the failing tests, there is a lot of context here")
	span := []bschemas.ChatMessage{msg}
	ask := append([]bschemas.ChatMessage(nil), span...)
	ask = append(ask, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser})
	call := func(ctx context.Context) (string, error) { return model.CompleteMessages(ctx, "", ask) }
	offload.RegisterKeepAliveCandidateForTest(session, st, call, "messages", span, 1, "", 0)
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })
}

// readDeferred reads the usage the offload side deferred onto the session's next turn.
func readDeferred(t *testing.T, st store.Store, session string) (ledgerIn int, calls []map[string]any) {
	t.Helper()
	b, ok := st.Get(store.UsagePrefix + session)
	if !ok || len(b) == 0 {
		return 0, nil
	}
	var u struct {
		In    int              `json:"in"`
		Calls []map[string]any `json:"calls"`
	}
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatal(err)
	}
	return u.In, u.Calls
}

// The "_billed" marker is a string contract between two packages (#458 review): offload writes a
// committed keep-alive summary call's detail row with trigger "keepalive_substitute_billed", and
// dash's component-cost query matches the strategy "*_billed" to keep that row out of the
// gateway cost split. Neither side imports the other, so a rename on one side would bring the
// double count back with no compile error. This test drives the real offload side, takes the
// trigger it actually wrote, and feeds a row with that strategy to the real dash query.
func TestBilledElsewhereMarkerReachesTheDashboardCostQuery(t *testing.T) {
	// Offload side: a committed keep-alive summary call.
	k, _, clock := testKeeper(t, Limits{})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &usageSummaryModel{}
	kaCandidate(t, "sess-1", st, model)
	recordOne(t, k, kaPolicy(), kaToolBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.Stats().SummarySubstituted == 0 {
		time.Sleep(time.Millisecond)
	}
	if k.Stats().SummarySubstituted != 1 {
		t.Fatal("no committed summary call: the offload side never ran")
	}
	_, calls := readDeferred(t, st, "sess-1")
	if len(calls) != 1 {
		t.Fatalf("want one deferred detail row, got %v", calls)
	}
	path, _ := calls[0]["path"].(string)
	trigger, _ := calls[0]["trigger"].(string)
	// The same Strategy takeDeferredUsage builds for the extraction_calls row.
	strategy := path + "/" + trigger

	// Dash side: the real component-cost query, over one turn whose gateway cost belongs to
	// extract_llm only, and one earlier gateway-priced summarizer call.
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
		BatchSize: 1, FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	ev := func(ts int64, cg float64, ex ...dash.ExtractionRow) *dash.Event {
		comps := []dash.CompRow{{Component: "cache_aware_summarizer", Acted: true}}
		for _, x := range ex {
			if x.Component == "extract_llm" {
				comps = append(comps, dash.CompRow{Component: "extract_llm", Acted: true, Mutated: true,
					SavedGross: 1000, SavedUnique: 1000})
			}
		}
		return &dash.Event{TS: ts, SessionID: "s1", Model: "m1", TenantID: "t1",
			TokensBefore: 10000, TokensAfter: 9000, FreshInput: 10, CacheRead: 50000,
			TokenAccounting: dash.AccountingComplete, CGLLMCostUSD: cg,
			Components: comps, Extractions: ex}
	}
	rec.Record(ev(1000, 0.02, dash.ExtractionRow{Component: "cache_aware_summarizer", Model: "m1",
		CostUSD: 0.02, Strategy: "prefix_ask/turn"}))
	rec.Record(ev(2000, 0.05,
		dash.ExtractionRow{Component: "extract_llm", Model: "m1", CostUSD: 0.05},
		dash.ExtractionRow{Component: "cache_aware_summarizer", Model: "m1", CostUSD: 0.0161, Strategy: strategy}))

	var rows []*dash.ComponentRow
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows, err = rec.DB().Components(dash.Filter{TenantAll: true}); err == nil && len(rows) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := map[string]*dash.ComponentRow{}
	for _, r := range rows {
		got[r.Component] = r
	}
	el, ca := got["extract_llm"], got["cache_aware_summarizer"]
	if el == nil || ca == nil {
		t.Fatalf("component rows never appeared: %v", got)
	}
	if math.Abs(el.LLMCostGatewayUSD-0.05) > 1e-9 {
		t.Errorf("extract_llm gateway cost = %v, want 0.05: the dash query did not recognise the "+
			"strategy offload wrote (%q) as billed elsewhere", el.LLMCostGatewayUSD, strategy)
	}
	if math.Abs(ca.LLMCostUSD-0.0361) > 1e-9 {
		t.Errorf("cache_aware_summarizer verdict cost = %v, want 0.0361 (gateway 0.02 + the "+
			"ping-billed call 0.0161); strategy %q", ca.LLMCostUSD, strategy)
	}
}
