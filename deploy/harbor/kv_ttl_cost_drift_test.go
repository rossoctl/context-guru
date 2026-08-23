package harbor

// The Python KV-cache cost model and the Go one must be the same arithmetic.
//
// kv_ttl_cost_model.py exists because the survival predictor is Python and an evaluation
// loop that shelled into Go for every candidate threshold would not get run. But the
// SHIPPED implementation is `kvcache.Simulate`, which is what the dashboard reports from,
// and two implementations of one money question is precisely the drift this project has
// been bitten by — the browser has twice duplicated a table the server owns and diverged
// from it. So the Python file is a port, this test is the guard, and when they disagree Go
// is right.
//
// It hands BOTH the same trajectory and the same per-turn action list and compares the
// totals. Nothing is mocked: the Python side runs its real evaluator through its real
// `--fixture` entry point, which needs only the standard library — the scientific stack is
// imported lazily by the predictor bridge and the price-list reader, neither of which this
// path touches. So this guard runs in a plain CI container.

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// pythonBin is the interpreter to drive the port with. KVCACHE_PYTHON overrides it for a
// venv; the test SKIPS rather than fails when no usable interpreter is present, because a
// missing Python is an absent guard, not a broken product.
func pythonBin(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{os.Getenv("KVCACHE_PYTHON"), "python3", "python"} {
		if cand == "" {
			continue
		}
		p, err := exec.LookPath(cand)
		if err != nil {
			continue
		}
		// It must be able to import the module at all; a 3.8 interpreter cannot.
		if out, err := exec.Command(p, "-c",
			"import sys; sys.path.insert(0,'.'); import kv_ttl_cost_model").CombinedOutput(); err != nil {
			t.Logf("%s cannot import kv_ttl_cost_model, skipping it: %s", p, out)
			continue
		}
		return p
	}
	return ""
}

// The fixture wire format, and it is the Python dataclass's own field names: the port is
// the thing under test, so the bridge into it stays as thin as possible.
type fixtureRequest struct {
	RequestID     int64  `json:"request_id"`
	User          string `json:"user"`
	Conversation  string `json:"conversation"`
	TSms          int64  `json:"ts_ms"`
	Model         string `json:"model"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CachedContext int64  `json:"cached_context"`
	MissReason    string `json:"miss_reason"`
}

type fixtureRate struct {
	Input            float64 `json:"input"`
	Output           float64 `json:"output"`
	CacheRead        float64 `json:"cache_read"`
	Write5m          float64 `json:"write_5m"`
	Write1h          float64 `json:"write_1h"`
	PingInputTokens  int64   `json:"ping_input_tokens"`
	PingOutputTokens int64   `json:"ping_output_tokens"`
	Known            bool    `json:"known"`
}

type fixture struct {
	WindowEndMs int64                  `json:"window_end_ms"`
	Rates       map[string]fixtureRate `json:"rates"`
	Semantics   map[string]bool        `json:"semantics"`
	Schedule    map[string]int64       `json:"schedule"`
	Requests    []fixtureRequest       `json:"requests"`
	// Actions is keyed by REQUEST ID, never positional. The fixture deliberately contains
	// two rows with the same timestamp, and both implementations order by (ts, id) — so a
	// positional list is assigned in a different order than the one it was written in, and
	// the two sides silently score different action sets. That is the first thing this test
	// caught, and it caught it in itself.
	Actions map[string]string `json:"actions"`
	// Policy runs a named arm instead of Actions, so the guard can lock the two exact
	// CEILINGS together as well as the action-list evaluator. A ceiling the two sides
	// disagree about is one neither can be quoted against.
	Policy string `json:"policy,omitempty"`
}

// The rates are this gateway's own opus-5 and sonnet-5 numbers, per TOKEN, plus a model
// with no rates at all — because "an unpriced request is counted and never valued" is one
// of the behaviours the two implementations have to agree about.
var driftRates = map[string]fixtureRate{
	"aws/claude-opus-5": {Input: 3.8e-6, Output: 19e-6, CacheRead: 0.38e-6,
		Write5m: 4.75e-6, Write1h: 7.6e-6, PingInputTokens: 1, PingOutputTokens: 1, Known: true},
	"aws/claude-sonnet-5": {Input: 1.52e-6, Output: 7.6e-6, CacheRead: 0.152e-6,
		Write5m: 1.9e-6, Write1h: 3.04e-6, PingInputTokens: 1, PingOutputTokens: 1, Known: true},
	"no-such-model": {PingInputTokens: 1, PingOutputTokens: 1},
}

// A trajectory set built to exercise every branch the two share, and every one of these
// rows is here because getting it wrong is invisible in a total:
//
//   - gaps on both sides of the 5-minute and 1-hour edges, and one EXACTLY on 5 minutes;
//   - two accounts presenting the SAME session id, which must not be spliced;
//   - tied timestamps, broken by id;
//   - a `prefix_change` and a `cold_start`, which no TTL may rescue;
//   - a conversation whose last request leaves a PINGING action open at the window's end;
//   - a model with no rates;
//   - a keep-alive interval wider than the lifetime it protects, so a ping lands on a
//     lapsed entry and is billed as a re-creation rather than a read;
//   - the write-cheap-then-extend action, whose keep-alive UPGRADES a live entry to the hourly
//     tier and is therefore a write on purpose — the one ping that costs 20x a read by design,
//     and the easiest thing on this page to price as if it were free.
func driftFixture() fixture {
	const base = int64(1_786_967_311_185)
	const min5, hour = int64(300_000), int64(3_600_000)
	rows := []fixtureRequest{
		// A: short gaps, then exactly 5 minutes, then past it.
		{1, "acct-a", "sess-1", base, "aws/claude-opus-5", 120, 40, 100_000, "cold_start"},
		{2, "acct-a", "sess-1", base + 30_000, "aws/claude-opus-5", 90, 55, 180_000, "hit"},
		{3, "acct-a", "sess-1", base + 30_000 + min5, "aws/claude-opus-5", 70, 30, 220_000, "hit"},
		{4, "acct-a", "sess-1", base + 30_000 + min5 + min5 + 1, "aws/claude-opus-5", 60, 20, 240_000, "ttl_expiry"},
		// A long gap that only a 1-hour hold or a ping schedule could survive.
		{5, "acct-a", "sess-1", base + 40*60_000, "aws/claude-opus-5", 50, 25, 500_000, "ttl_expiry"},
		// Past an hour: nothing on offer reaches.
		{6, "acct-a", "sess-1", base + 40*60_000 + hour + 1, "aws/claude-opus-5", 40, 15, 520_000, "ttl_expiry"},

		// B: another ACCOUNT with the SAME session id. Must never join A's trajectory.
		{7, "acct-b", "sess-1", base + 1_000, "aws/claude-sonnet-5", 200, 60, 90_000, "cold_start"},
		{8, "acct-b", "sess-1", base + 61_000, "aws/claude-sonnet-5", 150, 45, 140_000, "hit"},
		// A prefix change: alive or not, this one misses.
		{9, "acct-b", "sess-1", base + 121_000, "aws/claude-sonnet-5", 130, 35, 160_000, "prefix_change"},

		// C: tied timestamps, resolved by id, and a zero-length gap.
		{11, "acct-b", "sess-2", base + 5_000, "aws/claude-sonnet-5", 80, 20, 60_000, "cold_start"},
		{10, "acct-b", "sess-2", base + 5_000, "aws/claude-sonnet-5", 80, 20, 60_000, "hit"},

		// D: an unpriced model. Counted, never valued.
		{12, "acct-c", "sess-3", base + 2_000, "no-such-model", 300, 90, 70_000, "cold_start"},
		{13, "acct-c", "sess-3", base + 400_000, "no-such-model", 100, 30, 80_000, "ttl_expiry"},

		// E: one turn only, and the action left open at the window's end pings.
		{14, "acct-c", "sess-4", base + 3_000, "aws/claude-opus-5", 60, 18, 300_000, "cold_start"},
	}
	// One action per row, BY ID, chosen to cover all five actions and to leave a pinging
	// action open at the window's end on two different conversations.
	actions := map[string]string{
		"1": "write_5m", "2": "write_5m", "3": "write_1h", "4": "expire", "5": "write_5m_ping_1h",
		"6": "write_5m",
		"7": "write_5m", "8": "ping_1h", "9": "write_5m",
		"10": "write_5m", "11": "expire",
		"12": "write_1h", "13": "write_5m_ping_1h",
		"14": "ping_5m",
	}
	var end int64
	for _, r := range rows {
		if r.TSms > end {
			end = r.TSms
		}
	}
	return fixture{
		WindowEndMs: end,
		Rates:       driftRates,
		// Anthropic's documented behaviour, stated rather than defaulted, so the two
		// implementations cannot silently disagree about what the default IS.
		Semantics: map[string]bool{"hit_refreshes_ttl": true, "ping_refreshes_ttl": true,
			"zero_generation": false},
		// A 5-minute interval WIDER than the 5-minute lifetime, deliberately: it is the only
		// way to reach the "a keep-alive fired after the entry lapsed, so it re-created the
		// prefix at the write rate" branch, which is the most expensive thing either
		// implementation can do and the one a reader would never notice being wrong.
		Schedule: map[string]int64{"idle_5m_ms": 330_000, "idle_1h_ms": 3_360_000,
			"max_pings": 2},
		Requests: rows,
		Actions:  actions,
	}
}

// goFixtureInputs turns a fixture into the dataset and config the shipped simulator takes.
func goFixtureInputs(t *testing.T, f fixture) ([]*kvcache.Request, kvcache.Config) {
	t.Helper()
	// The rate table, laid in as overrides so the fixture is the only source of truth and no
	// network price map can move this test.
	overrides := map[string]kvcache.Override{}
	models := make([]string, 0, len(f.Rates))
	for name, r := range f.Rates {
		models = append(models, name)
		if !r.Known {
			continue
		}
		in, out, cr, w5, w1 := r.Input, r.Output, r.CacheRead, r.Write5m, r.Write1h
		pin, pout := r.PingInputTokens, r.PingOutputTokens
		overrides[name] = kvcache.Override{Input: &in, Output: &out, CacheRead: &cr,
			Write5m: &w5, Write1h: &w1, PingInputTokens: &pin, PingOutputTokens: &pout}
	}
	book := kvcache.NewPriceList(t.Context(), models, nil, kvcache.Multipliers{}, overrides)

	reqs := make([]*kvcache.Request, 0, len(f.Requests))
	act := map[int64]kvcache.Action{}
	for _, r := range f.Requests {
		reqs = append(reqs, &kvcache.Request{
			ID: r.RequestID, User: r.User, ConversationID: r.Conversation, TS: r.TSms,
			HourUTC: time.UnixMilli(r.TSms).UTC().Hour(), Bucket: kvcache.BucketAt(r.TSms),
			Model: r.Model, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			CachedContext: r.CachedContext, MissReason: r.MissReason,
			TTL: kvcache.TTL5m, TTLSource: kvcache.TTLSourceConfigured,
		})
		act[r.RequestID] = kvcache.Action(f.Actions[strconv.FormatInt(r.RequestID, 10)])
	}
	kvcache.Derive(reqs)
	cfg := kvcache.Config{
		Prices: book,
		Semantics: kvcache.Semantics{HitRefreshesTTL: f.Semantics["hit_refreshes_ttl"],
			PingRefreshesTTL: f.Semantics["ping_refreshes_ttl"],
			ZeroGeneration:   f.Semantics["zero_generation"]},
		PingIdle:   time.Duration(f.Schedule["idle_5m_ms"]) * time.Millisecond,
		PingIdle1h: time.Duration(f.Schedule["idle_1h_ms"]) * time.Millisecond,
		MaxPings:   int(f.Schedule["max_pings"]),
		WindowEnd:  f.WindowEndMs,
	}
	return reqs, cfg
}

// goTotal replays the fixture's own action list through the SHIPPED implementation.
func goTotal(t *testing.T, f fixture) *kvcache.Result {
	t.Helper()
	reqs, cfg := goFixtureInputs(t, f)
	act := map[int64]kvcache.Action{}
	for _, r := range f.Requests {
		act[r.RequestID] = kvcache.Action(f.Actions[strconv.FormatInt(r.RequestID, 10)])
	}
	return kvcache.Simulate(reqs, kvcache.NewReplay("fixture", act, kvcache.ActionExpire), cfg)
}

// pyResult is the subset of the port's output this test compares. Every field here is one
// the two implementations could disagree about while both looking plausible.
type pyResult struct {
	TotalUSD          float64 `json:"total_usd"`
	FreshInputUSD     float64 `json:"fresh_input_usd"`
	CacheReadUSD      float64 `json:"cache_read_usd"`
	CacheWriteUSD     float64 `json:"cache_write_usd"`
	OutputUSD         float64 `json:"output_usd"`
	PingUSD           float64 `json:"ping_usd"`
	UncachedUSD       float64 `json:"uncached_usd"`
	CachePremiumUSD   float64 `json:"cache_premium_usd"`
	Requests          int64   `json:"requests"`
	Conversations     int64   `json:"conversations"`
	Hits              int64   `json:"hits"`
	Misses            int64   `json:"misses"`
	ForcedMisses      int64   `json:"forced_misses"`
	Pings             int64   `json:"pings"`
	PingsThatRewrote  int64   `json:"pings_that_rewrote"`
	PingsThatUpgraded int64   `json:"pings_that_upgraded"`
	PingsOnOpenSpans  int64   `json:"pings_on_open_spans"`
	Writes5m          int64   `json:"writes_5m"`
	Writes1h          int64   `json:"writes_1h"`
	Expires           int64   `json:"expires"`
	AvoidedTokens     int64   `json:"avoided_tokens"`
	RetainedMs        int64   `json:"retained_ms"`
	Unpriced          int64   `json:"unpriced"`
	Valued            bool    `json:"valued"`
}

// scoreWithPort writes the fixture and returns what the Python evaluator made of it.
func scoreWithPort(t *testing.T, py string, f fixture) pyResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.json")
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "kv_ttl_cost_model.py", "--fixture", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the port failed on the fixture: %v\nstdout: %s\nstderr: %s",
			err, out, stderr.String())
	}
	var got pyResult
	// The port prints one JSON object; take the last line so a stray warning cannot break it.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("cannot read the port's output: %v\n%s", err, out)
	}
	return got
}

// Both sides solve the same dynamic program for the cheapest possible plan. If they disagree,
// the ceiling every saving on the page is measured against is not one number.
func TestTheTwoExactCeilingsAgree(t *testing.T) {
	py := pythonBin(t)
	if py == "" {
		t.Skip("no interpreter that can import kv_ttl_cost_model")
	}
	f := driftFixture()
	f.Policy = "optimal"
	f.Actions = nil
	got := scoreWithPort(t, py, f)

	reqs, cfg := goFixtureInputs(t, f)
	want := kvcache.Simulate(reqs, kvcache.NewOptimal(reqs, cfg), cfg)
	if math.Abs(got.TotalUSD-want.TotalUSD) > 1e-4 {
		t.Errorf("the exact ceilings disagree: port $%.8f, kvcache.NewOptimal $%.8f\n"+
			"One of the two dynamic programs is wrong, and every saving quoted against the "+
			"ceiling is wrong with it.", got.TotalUSD, want.TotalUSD)
	}
	// The plans need not be identical — ties exist — but the counts should line up when the
	// costs do, and a wild divergence there means one side is solving a different problem.
	if got.Hits != want.Hits {
		t.Errorf("same cost, different hits: port %d, kvcache %d", got.Hits, want.Hits)
	}
	// And it must actually be a ceiling on the fixture, on both sides.
	plain := kvcache.Simulate(reqs, kvcache.Fixed5m(), cfg)
	if want.TotalUSD > plain.TotalUSD+1e-9 {
		t.Errorf("the optimum ($%.6f) is dearer than fixed-5m ($%.6f) on the fixture",
			want.TotalUSD, plain.TotalUSD)
	}
	t.Logf("both ceilings agree at $%.6f (fixed-5m costs $%.6f)", want.TotalUSD, plain.TotalUSD)
}

// The deliberate tier UPGRADE needs a keep-alive that lands while the entry is still ALIVE,
// so it needs an interval INSIDE the five-minute lifetime — the opposite of the main
// fixture's, which is deliberately wider so that pings land on a lapsed entry. One schedule
// cannot reach both branches, so this is a second scenario over the same trajectory rather
// than a compromise that reaches neither.
func TestTheUpgradingKeepAliveAgrees(t *testing.T) {
	py := pythonBin(t)
	if py == "" {
		t.Skip("no interpreter that can import kv_ttl_cost_model")
	}
	f := driftFixture()
	// 280 s: inside the 300 s lifetime, which is the shipped keep-alive cadence.
	f.Schedule["idle_5m_ms"] = 280_000
	got := scoreWithPort(t, py, f)
	want := goTotal(t, f)

	if want.PingsThatUpgraded == 0 {
		t.Fatal("no keep-alive upgraded a live entry; this scenario is not exercising the one " +
			"ping that is a write on purpose")
	}
	if want.PingsThatRewrote != 0 {
		t.Errorf("%d pings re-created a lapsed entry; with a cadence inside the lifetime none "+
			"should, so this scenario is measuring the wrong branch", want.PingsThatRewrote)
	}
	if math.Abs(got.TotalUSD-want.TotalUSD) > 1e-4 {
		t.Errorf("total_usd: port %.8f, kvcache.Simulate %.8f — the two disagree about what "+
			"extending a live entry by an hour costs", got.TotalUSD, want.TotalUSD)
	}
	if got.PingsThatUpgraded != want.PingsThatUpgraded {
		t.Errorf("pings_that_upgraded: port %d, kvcache %d",
			got.PingsThatUpgraded, want.PingsThatUpgraded)
	}
	if got.PingUSD != 0 && math.Abs(got.PingUSD-want.PingUSD) > 1e-4 {
		t.Errorf("ping_usd: port %.8f, kvcache %.8f", got.PingUSD, want.PingUSD)
	}
	// An upgrade is a WRITE. If it were priced as a read it would cost 20x less, so this pins
	// the magnitude rather than only the agreement.
	reqs, cfg := goFixtureInputs(t, f)
	_ = reqs
	price := cfg.Prices.For("aws/claude-opus-5")
	if price.Write1h <= price.CacheRead*10 {
		t.Errorf("the fixture's 1-hour write rate (%.9f) is not meaningfully dearer than its "+
			"read rate (%.9f), so this test cannot tell a write from a read",
			price.Write1h, price.CacheRead)
	}
	t.Logf("agreed at $%.6f with %d upgrading keep-alives", want.TotalUSD, want.PingsThatUpgraded)
}

func TestPythonCostModelAgreesWithTheShippedSimulator(t *testing.T) {
	py := pythonBin(t)
	if py == "" {
		t.Skip("no interpreter that can import kv_ttl_cost_model")
	}
	f := driftFixture()
	got := scoreWithPort(t, py, f)
	want := goTotal(t, f)

	// A hundredth of a cent. Both sides sum the same products of the same float64s in the
	// same order, so the difference should be zero; the epsilon is for the JSON round trip,
	// not for a licence to differ.
	const eps = 1e-4
	for _, c := range []struct {
		name     string
		got, exp float64
	}{
		{"total_usd", got.TotalUSD, want.TotalUSD},
		{"fresh_input_usd", got.FreshInputUSD, want.FreshInputUSD},
		{"cache_read_usd", got.CacheReadUSD, want.CacheReadUSD},
		{"cache_write_usd", got.CacheWriteUSD, want.CacheWriteUSD},
		{"output_usd", got.OutputUSD, want.OutputUSD},
		{"ping_usd", got.PingUSD, want.PingUSD},
		{"uncached_usd", got.UncachedUSD, want.UncachedUSD},
		{"cache_premium_usd", got.CachePremiumUSD, want.CachePremium},
	} {
		if math.Abs(c.got-c.exp) > eps {
			t.Errorf("%s: port %.8f, kvcache.Simulate %.8f (delta %.2e)\n"+
				"The Python cost model has drifted from the arithmetic the dashboard ships. "+
				"Go is right; fix deploy/harbor/kv_ttl_cost_model.py.",
				c.name, c.got, c.exp, c.got-c.exp)
		}
	}
	for _, c := range []struct {
		name     string
		got, exp int64
	}{
		{"requests", got.Requests, want.Requests},
		{"conversations", got.Conversations, want.Conversations},
		{"hits", got.Hits, want.Hits},
		{"misses", got.Misses, want.Misses},
		{"forced_misses", got.ForcedMisses, want.ForcedMisses},
		{"pings", got.Pings, want.Pings},
		{"pings_that_rewrote", got.PingsThatRewrote, want.PingsThatRewrote},
		{"pings_that_upgraded", got.PingsThatUpgraded, want.PingsThatUpgraded},
		{"pings_on_open_spans", got.PingsOnOpenSpans, want.PingsOnOpenSpans},
		{"writes_5m", got.Writes5m, want.Writes5m},
		{"writes_1h", got.Writes1h, want.Writes1h},
		{"expires", got.Expires, want.Expires},
		{"avoided_tokens", got.AvoidedTokens, want.AvoidedTokens},
		{"retained_ms", got.RetainedMs, want.RetainedMs},
		{"unpriced", got.Unpriced, want.Unpriced},
	} {
		if c.got != c.exp {
			t.Errorf("%s: port %d, kvcache.Simulate %d", c.name, c.got, c.exp)
		}
	}
	// Valued gates every cost the page renders, so the two must agree about it too — and the
	// fixture must actually be priced, or this comparison is two falses agreeing and the whole
	// dollar comparison above is zero on both sides.
	if got.Valued != want.Valued {
		t.Errorf("valued: port %v, kvcache.Simulate %v", got.Valued, want.Valued)
	}
	if !want.Valued {
		t.Error("the fixture prices nothing, so every dollar compared here is zero on both " +
			"sides and this test proves nothing")
	}
	for _, c := range []struct {
		name     string
		got, exp int64
	}{} {
		if c.got != c.exp {
			t.Errorf("%s: port %d, kvcache.Simulate %d", c.name, c.got, c.exp)
		}
	}
	// A fixture that exercised nothing would pass this test while proving nothing, so the
	// branches it exists for are asserted to have actually been reached.
	if want.Pings == 0 || want.PingsThatRewrote == 0 || want.PingsOnOpenSpans == 0 {
		t.Errorf("the fixture no longer reaches the keep-alive branches (pings=%d rewrote=%d "+
			"open=%d); the agreement it proves is narrower than it looks",
			want.Pings, want.PingsThatRewrote, want.PingsOnOpenSpans)
	}
	if want.Unpriced == 0 || want.ForcedMisses == 0 || want.Hits == 0 || want.Expires == 0 {
		t.Errorf("the fixture no longer reaches unpriced/forced-miss/hit/expire (unpriced=%d "+
			"forced=%d hits=%d expires=%d)",
			want.Unpriced, want.ForcedMisses, want.Hits, want.Expires)
	}
	if want.Conversations != 5 {
		t.Errorf("conversations = %d, want 5 — two accounts share a session id and must not "+
			"be spliced into one trajectory", want.Conversations)
	}
	t.Logf("agreed: total $%.6f over %d requests, %d conversations, %d pings (%d re-created)",
		want.TotalUSD, want.Requests, want.Conversations, want.Pings, want.PingsThatRewrote)
}

// The ping schedule is restated in the port; this checks the two agree over a table rather
// than by inspection. kvcache.PingsPerSpan is the original.
func TestPingScheduleMatchesThePort(t *testing.T) {
	py := pythonBin(t)
	if py == "" {
		t.Skip("no interpreter that can import kv_ttl_cost_model")
	}
	// A [3]int64 rather than a struct: a struct with unexported fields marshals to `{}`, so
	// the port would receive a list of empty objects and unpack nothing. This test caught
	// that in itself too.
	var cases [][3]int64
	for _, gap := range []int64{0, 1, 279_999, 280_000, 280_001, 560_000, 560_001, 860_000,
		3_600_000, 86_400_000} {
		for _, idle := range []int64{0, 1_000, 280_000, 3_360_000} {
			for _, maxPings := range []int64{0, 1, 2, 4} {
				cases = append(cases, [3]int64{gap, idle, maxPings})
			}
		}
	}
	var script strings.Builder
	script.WriteString("import sys,json; sys.path.insert(0,'.')\n")
	script.WriteString("from kv_ttl_cost_model import pings_per_span as f\n")
	script.WriteString("print(json.dumps([f(g,i,m) for g,i,m in json.load(sys.stdin)]))\n")
	in, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-c", script.String())
	cmd.Stdin = strings.NewReader(string(in))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the port's ping schedule failed: %v\nstdout: %s\nstderr: %s",
			err, out, stderr.String())
	}
	var got []int
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d answers for %d cases", len(got), len(cases))
	}
	for i, c := range cases {
		gap, idle, maxPings := c[0], c[1], c[2]
		want := kvcache.PingsPerSpan(time.Duration(gap)*time.Millisecond,
			time.Duration(idle)*time.Millisecond, int(maxPings))
		if got[i] != want {
			t.Errorf("pings_per_span(gap=%d, idle=%d, max=%d): port %d, kvcache %d",
				gap, idle, maxPings, got[i], want)
		}
	}
}
