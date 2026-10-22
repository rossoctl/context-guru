package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/predictive"
)

// panicModel is a campaign model that fails: the keeper must behave exactly as if no campaign existed.
type panicModel struct{}

func (panicModel) Kind() string                                   { return "panic" }
func (panicModel) Rule() predictive.Rule                          { return predictive.Rule{Lam: 1, XS: 280, KMax: 4, TCS: 295} }
func (panicModel) Survival(*predictive.Row, []float64, []float64) { panic("model failure") }

const planSpec = `{"schema":"cg.cache.campaign/2","version":"2.0.0","menu":{"u280k2":{"gaps_s":[280,280],"max_pings":2,"tier":"5m"}},
 "policy":{"kind":"rules","segments":[{"id":"all","when":[],"rule":{"kind":"plan","plan":"u280k2"}}],"fallback":{"kind":"none"}}}`

const modelSpec = `{"schema":"cg.cache.campaign/2","version":"2.0.0","menu":{},
 "policy":{"kind":"model+segments","model":{"ref":"m.json","sha256":"x"},"segments":[{"id":"all","when":[],"rule":{"kind":"model"}}],"fallback":{"kind":"none"}}}`

func campaignRuntime(t *testing.T, spec string, model predictive.Model, mode string) (*predRuntime, *predCampaign) {
	t.Helper()
	sp, err := predictive.ParseSpec([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	pc := &predCampaign{meta: campaignMeta{ID: "cmp-000000000001", Tenant: "t1", Mode: mode}, spec: sp, model: model, tr: predictive.NewTracker(), pending: map[string][]predPending{}}
	return &predRuntime{byTenant: map[string]*predCampaign{"t1": pc}}, pc
}

func recordWith(k *keeper, cache CachePolicy, at time.Time) {
	tn := &Tenancy{ID: "t1", Cache: cache}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	arr := k.pred.arrive("t1", "sess-1", "", at)
	u := Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn", Live: &LiveFacts{Arrival: arr, Messages: 3}}
	k.record(tn, "sess-1", "", at, []byte(kaBody), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages", http.StatusOK, u, true)
}

func liveEntries(k *keeper) int { return k.Stats().Live }

// Shadow mode is inert: with a campaign that panics, the keeper registers exactly what it registers with no campaign; with the kill switch on, the campaign sees nothing.
func TestShadowCampaignIsInert(t *testing.T) {
	base, _, _ := testKeeper(t, Limits{})
	recordWith(base, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	recordWith(base, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 5, 0, time.UTC))
	want := liveEntries(base)
	if want == 0 {
		t.Fatal("control: no entry registered")
	}

	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = stubPrices{}
	rt, pc := campaignRuntime(t, modelSpec, panicModel{}, campaignModeShadow)
	k.pred = rt
	recordWith(k, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	recordWith(k, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 5, 0, time.UTC))
	if got := liveEntries(k); got != want {
		t.Errorf("a panicking shadow campaign changed the keeper: %d entries, want %d", got, want)
	}

	kc, _, _ := testKeeper(t, Limits{}) // control: the same campaign scores when the keep-alive is on
	kc.h.opts.Prices = stubPrices{}
	rtc, pcc := campaignRuntime(t, planSpec, nil, campaignModeShadow)
	kc.pred = rtc
	recordWith(kc, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if s, _ := pcc.snapshot(); s.Decisions != 1 {
		t.Fatalf("control campaign scored %d decisions, want 1", s.Decisions)
	}

	k2, _, _ := testKeeper(t, Limits{})
	k2.h.opts.Prices = stubPrices{}
	rt2, pc2 := campaignRuntime(t, planSpec, nil, campaignModeShadow)
	k2.pred = rt2
	t.Setenv("CONTEXT_GURU_KEEPALIVE", "off")
	recordWith(k2, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	recordWith(k2, kaPolicy(), time.Date(2026, 8, 22, 12, 0, 5, 0, time.UTC))
	if s, _ := pc2.snapshot(); s.Decisions != 0 {
		t.Errorf("the kill switch was on and the campaign still scored %d decisions", s.Decisions)
	}
	if s, _ := pc.snapshot(); s.Decisions != 0 {
		t.Errorf("a failed decision was counted: %+v", s)
	}
}

// Active mode never turns the keep-alive on for a tenant whose own policy has it off, and never replaces what an account that opted in has configured beyond the ping count.
func TestActiveCampaignNeverOverridesTenantOptOut(t *testing.T) {
	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = stubPrices{}
	rt, pc := campaignRuntime(t, planSpec, nil, campaignModeActive)
	k.pred = rt
	out := kaPolicy()
	out.KeepAlive = false // the tenant opted out
	recordWith(k, out, time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	recordWith(k, out, time.Date(2026, 8, 22, 12, 0, 5, 0, time.UTC))
	if got := liveEntries(k); got != 0 {
		t.Fatalf("an opted-out tenant has %d held entries under an active campaign", got)
	}
	if s, _ := pc.snapshot(); s.OptedOut == 0 {
		t.Errorf("the skipped activation was not counted: %+v", s)
	}
	// the same campaign for a tenant that opted in does act
	k2, _, _ := testKeeper(t, Limits{})
	k2.h.opts.Prices = stubPrices{}
	rt2, _ := campaignRuntime(t, planSpec, nil, campaignModeActive)
	k2.pred = rt2
	in := kaPolicy()
	in.MaxPings = 1
	recordWith(k2, in, time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	recordWith(k2, in, time.Date(2026, 8, 22, 12, 0, 5, 0, time.UTC))
	e, ok := k2.Entry(keepalive.Key{Tenant: "t1", Session: "sess-1"})
	if !ok {
		t.Fatal("no entry for the opted-in tenant")
	}
	if e.Policy.MaxPings != 2 || e.Tag != campaignStrategyID("cmp-000000000001") {
		t.Errorf("campaign not applied for an opted-in tenant: MaxPings %d, strategy %q", e.Policy.MaxPings, e.Tag)
	}
}

// Two overlapping requests on one thread keep their own identity and their own scored decision.
func TestOverlappingRequestsOnAThreadKeepTheirOwnState(t *testing.T) {
	k, _, _ := testKeeper(t, Limits{})
	k.h.opts.Prices = stubPrices{}
	rt, pc := campaignRuntime(t, planSpec, nil, campaignModeShadow)
	k.pred = rt
	t0 := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	a1 := rt.arrive("t1", "s", "", t0)
	a2 := rt.arrive("t1", "s", "", t0.Add(2*time.Second)) // starts while the first is still running
	if a1 == nil || a2 == nil || a1 == a2 {
		t.Fatal("overlapping requests did not get distinct arrivals")
	}
	body := []byte(`{"model":"aws/claude-opus-4","messages":[]}`)
	u := Usage{CacheRead: 120000, StopReason: "end_turn"}
	// responses end in the opposite order
	if _, ok := k.predictiveDecide(a2, false, "t1", "s", "", "anthropic", t0.Add(2*time.Second), body, "claude-cli", u); !ok {
		t.Fatal("second decision refused")
	}
	if _, ok := k.predictiveDecide(a1, false, "t1", "s", "", "anthropic", t0, body, "claude-cli", u); !ok {
		t.Fatal("first decision refused")
	}
	pc.mu.Lock()
	n := len(pc.pending["s\x00"])
	pc.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d decisions kept for the thread, want 2", n)
	}
	rt.arrive("t1", "s", "", k.now().Add(60*time.Second)) // the user comes back: both are scored as returned
	if s, _ := pc.snapshot(); s.Resolved != 2 || s.Returned != 2 {
		t.Errorf("both decisions should resolve: %+v", s)
	}
}

func TestBuildEnvPassesNoCredentials(t *testing.T) {
	env := buildEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=secret", "CG_TOKEN=x", "CG_CAMPAIGN_PYTHON=/p", "HOME=/h", "OPENAI_API_KEY=s2", "LANG=C"})
	joined := strings.Join(env, " ")
	for _, bad := range []string{"secret", "CG_TOKEN", "HOME=", "s2"} {
		if strings.Contains(joined, bad) {
			t.Errorf("build environment leaks %q: %v", bad, env)
		}
	}
	for _, good := range []string{"PATH=/bin", "CG_CAMPAIGN_PYTHON=/p", "LANG=C", "OMP_NUM_THREADS=2"} {
		if !strings.Contains(joined, good) {
			t.Errorf("build environment lost %q: %v", good, env)
		}
	}
}
