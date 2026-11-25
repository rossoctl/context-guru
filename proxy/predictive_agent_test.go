package proxy

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/campaign"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/testutil"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/predictive"
)

// trainingAgent runs one request through the real path that training uses. capture.finish stores
// the dashboard row. The row's agent and provider go into a kvcache.Request, and campaign.Rows
// turns that into the RowFacts whose Agent the model trains on.
func trainingAgent(t *testing.T, cl agent.Client, provider string) string {
	t.Helper()
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	c := &capture{rec: rec, agent: cl, tenant: "t1", provider: provider, model: "claude-opus-5-5",
		route: "/v1/messages", start: time.Now().Add(-time.Minute), status: 200, trace: apply.Trace{Session: "s"}}
	c.finish(Usage{FreshInput: 10, CacheWrite: 1000}, true, false, 0, 0)
	var row *dash.Event
	testutil.WaitFor(t, "the stored row", func() bool {
		pg, err := rec.DB().Requests(dash.Filter{TenantAll: true}, 0, 10)
		if err != nil || len(pg.Requests) != 1 {
			return false
		}
		row = pg.Requests[0]
		return true
	})
	req := &kvcache.Request{ID: 1, User: "t1", ConversationID: "s", TS: 1000, Model: row.Model, Provider: row.Provider, Agent: row.Agent,
		UpstreamMs: 1000, CostKnown: true, CachedContext: 100000}
	price := func(model string, promptTokens int64) (float64, float64, float64, float64, bool) { return 5, 25, 0.5, 6.25, true }
	_, facts := campaign.Rows([]*kvcache.Request{req}, price, false, 1<<60)
	if len(facts) != 1 {
		t.Fatalf("campaign.Rows kept %d rows, want 1", len(facts))
	}
	return facts[0].Agent
}

// The live agent value must equal the value training reads for the same request (#1349).
func TestLiveAgentEqualsTrainingAgent(t *testing.T) {
	cases := []struct {
		name, ua, pi, provider, want string
	}{
		{"claude code cli", "claude-cli/2.1.0 (external, cli)", "", "anthropic", "claude-cli"},
		{"claude agent sdk", "claude-code/2.1.0", "", "anthropic", "claude-code"},
		{"pi on anthropic", "Anthropic/JS 0.60.0", "pi", "anthropic", "anthropic"},
		{"pi on openai", "OpenAI/JS 5.0.0", "pi", "openai", "openai"},
		{"codex", "codex_cli_rs/0.50.0", "", "openai", "codex"},
		{"opencode", "opencode/1.2.3", "", "anthropic", "opencode"},
		{"empty user agent", "", "", "anthropic", "unknown"},
		{"unknown user agent", "mytool/9 extra", "", "anthropic", "mytool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
			if tc.ua != "" {
				r.Header.Set("User-Agent", tc.ua)
			}
			if tc.pi != "" {
				r.Header.Set("x-context-guru-client", tc.pi)
			}
			cl := identify(r)
			live := (&capture{agent: cl, provider: tc.provider}).liveFacts(0).Agent
			training := trainingAgent(t, cl, tc.provider)
			if live != training {
				t.Errorf("live %q != training %q", live, training)
			}
			if live != tc.want {
				t.Errorf("live agent = %q, want %q", live, tc.want)
			}
		})
	}
}

// rowModel is a campaign model that keeps the feature row it was given.
type rowModel struct{ row **predictive.Row }

func (rowModel) Kind() string { return "row" }
func (rowModel) Rule() predictive.Rule {
	return predictive.Rule{Lam: 1, XS: 280, KMax: 4, TCS: 295}
}
func (m rowModel) Survival(r *predictive.Row, _ []float64, _ []float64) { *m.row = r }

// The fallback of predictiveDecide (no capture facts) gives the model the same agent value. A Pi
// client is marked by its header: the User-Agent says nothing, the provider gives the token.
func TestPredictiveDecideFallbackUsesTheModelAgent(t *testing.T) {
	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = stubPrices{}
	var got *predictive.Row
	rt, _ := campaignRuntime(t, modelSpec, rowModel{&got}, campaignModeShadow)
	k.pred = rt
	r, _ := http.NewRequest("POST", "http://x/v1/responses", nil)
	r.Header.Set("User-Agent", "node")
	r.Header.Set("x-context-guru-client", "pi")
	cl := identify(r)
	t0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	arr := rt.arrive("t1", "s", "", t0)
	body := []byte(`{"model":"aws/claude-opus-4","messages":[]}`)
	if _, ok := k.predictiveDecide(arr, false, "t1", "s", "", "openai", t0, body, cl, Usage{CacheRead: 120000, StopReason: "end_turn"}); !ok {
		t.Fatal("decision refused")
	}
	if got == nil {
		t.Fatal("the model was never asked")
	}
	if got.Cat[1] != "openai" {
		t.Errorf("model agent = %q, want %q (the raw User-Agent is %q)", got.Cat[1], "openai", "node")
	}
}

// A live claude-cli request sets the claude-cli one-hot of the model input.
func TestLiveClaudeCLISetsTheOneHot(t *testing.T) {
	r, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
	r.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	c := &capture{agent: identify(r), provider: "anthropic"}
	tr := predictive.NewTracker()
	rq := predictive.Request{Tenant: "t", Session: "s", Model: "claude-opus-4", TsMs: 1000}
	arr := tr.Arrive(rq)
	resp := &predictive.Response{Request: rq, EndMs: 2000, DecMs: 2000, Agent: c.liveFacts(0).Agent, StopReason: "end_turn"}
	tr.Ended(arr, resp)
	row := predictive.NewRow()
	tr.Features(arr, resp, row)
	idx := -1
	for i, n := range predictive.FeatureNames {
		if n == "ag_claude-cli" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no ag_claude-cli feature")
	}
	if row.F[idx] != 1 {
		t.Errorf("ag_claude-cli = %v, want 1", row.F[idx])
	}
}
