package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/proxyapp"
)

// price is one flat rate set, so every figure is reproducible.
type price struct{}

func (price) Price(context.Context, string) (proxyapp.Price, bool) {
	return proxyapp.Price{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}, true
}

// seed writes a real dashboard database with two accounts of different shape:
//
//	steady: Sunday-Thursday at 09:00 UTC, turns 4 minutes apart  (the 5m cache already holds)
//	gappy:  Sunday-Thursday at 14:00 UTC, turns 8 minutes apart  (a keep-alive pays here)
//
// and returns its path once the recorder has flushed and closed it.
func seed(t *testing.T) (string, int64, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cg.db")
	rec, err := dash.NewRecorder(dash.Options{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC) // a Sunday
	ev := func(tenant, session string, ts time.Time, read, write int64) *dash.Event {
		return &dash.Event{TS: ts.UnixMilli(), TenantID: tenant, SessionID: session, Model: "claude-sonnet-4-5",
			Provider: "anthropic", Agent: "claude-code", Mode: dash.ModeActive,
			TokensBefore: 120_000, TokensAfter: 120_000, FreshInput: 200, OutputTokens: 400,
			CacheRead: read, CacheWrite: write, CostUSD: 0.4, UpstreamMs: 2000,
			TokenAccounting: dash.AccountingComplete, CacheTTL: "ephemeral_5m"}
	}
	for d := 0; d < 14; d++ {
		day := start.AddDate(0, 0, d)
		if wd := day.Weekday(); wd == time.Friday || wd == time.Saturday {
			continue
		}
		for i := 0; i < 8; i++ {
			s1 := day.Add(9*time.Hour + time.Duration(i)*4*time.Minute)
			s2 := day.Add(14*time.Hour + time.Duration(i)*8*time.Minute)
			r1, w1 := int64(110_000), int64(1_000)
			if i == 0 {
				r1, w1 = 0, 111_000
			}
			rec.Record(ev("steady", "a"+day.Format("0102"), s1, r1, w1))
			rec.Record(ev("gappy", "b"+day.Format("0102"), s2, 0, 111_000)) // every 8-minute gap missed
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	return path, start.UnixMilli(), start.AddDate(0, 0, 14).UnixMilli()
}

// digest hashes the database file and its WAL CONTENT. A reader of a WAL-mode database may create
// an empty -wal and the -shm index beside a cleanly closed file: that is SQLite's own reader
// bookkeeping, not a write to the data. What must not change is the database and any WAL frames.
func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		if b, err := os.ReadFile(p); err == nil {
			all = append(all, b...)
		}
	}
	return sha256.Sum256(all)
}

func run(t *testing.T) (*Result, string) {
	t.Helper()
	path, since, until := seed(t)
	db, err := dash.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	res, err := Run(Inputs{DB: db, Pricer: price{}, Since: since, Until: until, Holdout: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	return res, path
}

func policy(t *testing.T, r *Report, name string) Policy {
	t.Helper()
	for _, p := range r.Policies {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no policy %q in %+v", name, r.Policies)
	return Policy{}
}

// THE FILE IS NOT TOUCHED. The tool runs beside a live proxy, so a read must leave the database and
// its WAL byte-identical.
func TestRunLeavesTheDatabaseByteIdentical(t *testing.T) {
	path, since, until := seed(t)
	before := digest(t, path)
	db, err := dash.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Inputs{DB: db, Pricer: price{}, Since: since, Until: until, Holdout: 0.3}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if after := digest(t, path); !bytes.Equal(before[:], after[:]) {
		t.Fatal("the database or its WAL frames changed during a read-only analysis")
	}
}

// THE ORDERING THAT MUST HOLD on any data: the ceiling is cheapest, the per-cell campaign is never
// worse than any single arm applied everywhere (each cell's winner is the cheapest of the same
// candidates), and the campaign never costs more than the baseline.
func TestThePolicyOrderingHolds(t *testing.T) {
	res, _ := run(t)
	r := res.Report
	opt, camp := policy(t, r, kvcache.StrategyOptimal), policy(t, r, "campaign")
	if opt.CostUSD > camp.CostUSD+1e-9 {
		t.Errorf("optimal $%.4f costs more than the campaign $%.4f: the ceiling must be cheapest", opt.CostUSD, camp.CostUSD)
	}
	for _, p := range r.Policies {
		if p.Kind == "arm" && camp.CostUSD > p.CostUSD+1e-9 {
			t.Errorf("the campaign ($%.4f) costs more than %s everywhere ($%.4f)", camp.CostUSD, p.Name, p.CostUSD)
		}
	}
	if camp.SavingUSD < -1e-9 {
		t.Errorf("campaign saving %.4f < 0: the baseline is a candidate in every cell", camp.SavingUSD)
	}
	if camp.OfOptimalPct > 100+1e-6 {
		t.Errorf("campaign keeps %.1f%% of optimal; above 100 is impossible", camp.OfOptimalPct)
	}
	// The weekday-aware campaign picks from the same candidates on finer cells: never worse.
	if wd := policy(t, r, "campaign-by-weekday"); wd.CostUSD > camp.CostUSD+1e-9 {
		t.Errorf("campaign-by-weekday $%.4f costs more than the campaign $%.4f", wd.CostUSD, camp.CostUSD)
	}
}

// THE TOTALS ARE CORE'S. The campaign's saving is exactly the sum of KVCacheSuggest's own cell
// savings, and the per-account breakdown adds back up to the whole.
func TestTotalsAreSumsOfCoreCells(t *testing.T) {
	res, _ := run(t)
	var sum float64
	for _, c := range res.Campaign.Cells {
		sum += c.SavingUSD
	}
	if got := policy(t, res.Report, "campaign").SavingUSD; math.Abs(got-sum) > 1e-9 {
		t.Errorf("campaign saving %.6f != sum of core's cell savings %.6f", got, sum)
	}
	var acct float64
	for _, b := range res.Report.Breakdowns["account"] {
		acct += b.BaselineUSD - b.CampaignUSD
	}
	if math.Abs(acct-sum) > 1e-9 {
		t.Errorf("per-account savings add to %.6f, want %.6f", acct, sum)
	}
}

// THE TWO ACCOUNTS GET DIFFERENT ARMS, which is the point of a per-cell campaign: the gappy account's
// 8-minute gaps are worth a keep-alive, the steady account's 4-minute gaps are not.
func TestTheCampaignDistinguishesTheAccounts(t *testing.T) {
	res, _ := run(t)
	arm := map[string]string{}
	for _, c := range res.Campaign.Cells {
		arm[c.User] = c.BestStrategy
	}
	if arm["steady"] == "" || arm["gappy"] == "" {
		t.Fatalf("missing cells: %v", arm)
	}
	if arm["steady"] == arm["gappy"] {
		t.Errorf("both accounts got %q; their gaps differ (4m vs 8m), so their best arms should too", arm["steady"])
	}
	for _, c := range res.Campaign.Cells {
		if c.User == "gappy" && c.SavingUSD <= 0 {
			t.Errorf("gappy's cell saves %.4f; every 8-minute gap was a full miss, so a keep-alive must pay", c.SavingUSD)
		}
	}
}

// THE UPLOAD IS THE DASHBOARD'S OWN FORMAT: it round-trips through dash.KVCacheSuggestions without
// loss, every cell has a tenant and an hour in range, and no (tenant, hour) repeats — the three
// things the campaign create route rejects.
func TestTheUploadIsAValidSuggestPayload(t *testing.T) {
	res, _ := run(t)
	b, err := json.Marshal(res.Campaign)
	if err != nil {
		t.Fatal(err)
	}
	var back dash.KVCacheSuggestions
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the upload does not decode as a suggest payload: %v", err)
	}
	seen := map[[2]any]bool{}
	for _, c := range back.Cells {
		if c.User == "" || c.HourUTC < 0 || c.HourUTC > 23 {
			t.Errorf("bad cell %+v", c)
		}
		k := [2]any{c.User, c.HourUTC}
		if seen[k] {
			t.Errorf("duplicate cell %v", k)
		}
		seen[k] = true
	}
	if len(back.Weekdays) == 0 {
		t.Error("no weekdays_included: the campaign would fall back to its default work week silently")
	}
}

// The upload is accepted by the create route's own decoder: it wraps the file as
// {name, source: "upload", suggest} and rejects unknown fields.
func TestTheUploadDecodesStrictlyInTheCreateEnvelope(t *testing.T) {
	res, _ := run(t)
	b, err := json.Marshal(map[string]any{"name": "t", "source": "upload", "suggest": res.Campaign})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Name    string                   `json:"name"`
		Source  string                   `json:"source"`
		Suggest *dash.KVCacheSuggestions `json:"suggest"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Suggest == nil || len(env.Suggest.Cells) != len(res.Campaign.Cells) {
		t.Fatalf("cells lost in the envelope: %+v", env.Suggest)
	}
}

// Two runs over the same history write the same report, policy order included.
func TestThePolicyOrderIsDeterministic(t *testing.T) {
	a, _ := run(t)
	for i := 0; i < 5; i++ {
		b, _ := run(t)
		if len(a.Report.Policies) != len(b.Report.Policies) {
			t.Fatal("policy count differs between runs")
		}
		for j := range a.Report.Policies {
			if a.Report.Policies[j].Name != b.Report.Policies[j].Name {
				t.Fatalf("policy %d is %s in one run and %s in another", j, a.Report.Policies[j].Name, b.Report.Policies[j].Name)
			}
		}
	}
}

// The report renders, and names every policy kind.
func TestTheMarkdownNamesEveryPolicy(t *testing.T) {
	res, _ := run(t)
	md := Markdown(res.Report)
	for _, want := range []string{"campaign", kvcache.StrategyOptimal, kvcache.StrategyObserved, "campaign-by-weekday", "## By account"} {
		if !bytes.Contains([]byte(md), []byte(want)) {
			t.Errorf("the Markdown report does not mention %q", want)
		}
	}
}

// noPrice knows no model, which is what the compiled-in table alone did on the first real run.
type noPrice struct{}

func (noPrice) Price(context.Context, string) (proxyapp.Price, bool) { return proxyapp.Price{}, false }

// AN UNPRICED HISTORY WRITES NO UPLOAD. Every cell would have no winner, and a campaign made from
// them would change nothing while looking like a decision.
func TestRunRefusesWhenNothingCanBePriced(t *testing.T) {
	path, since, until := seed(t)
	db, err := dash.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Run(Inputs{DB: db, Pricer: noPrice{}, Since: since, Until: until}); err == nil {
		t.Fatal("Run wrote a result for a history it could not price")
	}
}

// THE HOLDOUT SPLITS THE HISTORY THAT EXISTS. Asked for 60 days over 14 days of rows, a split of the
// asked-for window would leave the train side empty; the split must fall inside the retained span.
func TestTheHoldoutSplitsTheRetainedHistory(t *testing.T) {
	path, _, until := seed(t)
	db, err := dash.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := Run(Inputs{DB: db, Pricer: price{}, Since: until - 60*24*3600*1000, Until: until, Holdout: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	h := res.Report.Holdout
	if h == nil || h.TrainScanned == 0 || h.TestScanned == 0 {
		t.Fatalf("holdout train=%d test=%d rows, want both sides non-empty", h.TrainScanned, h.TestScanned)
	}
	if h.ComparableCells == 0 {
		t.Errorf("no comparable cells: the split produced nothing to score")
	}
}

// THE UPLOAD FITS the create route's 256 KiB body limit with room for its envelope, and carries no
// per-arm Candidates (they are in the report).
func TestTheUploadFitsTheCreateRoute(t *testing.T) {
	res, _ := run(t)
	b, err := json.Marshal(res.Campaign)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxUpload {
		t.Errorf("upload is %d bytes, above MaxUpload %d", len(b), MaxUpload)
	}
	for _, c := range res.Campaign.Cells {
		if len(c.Candidates) != 0 {
			t.Fatalf("cell %s/%d carries %d candidates; the upload must be lean", c.User, c.HourUTC, len(c.Candidates))
		}
	}
	// The report still has every arm's numbers: the breakdowns were computed from the full cells.
	if len(res.Report.Breakdowns["account"]) == 0 || res.Report.Breakdowns["account"][0].BestArm == "" {
		t.Error("the report lost its per-arm breakdown when the upload was slimmed")
	}
}

// EVERY UPLOADED WINNER IS ENFORCEABLE. A cell the create route records but never activates keeps the
// baseline while the campaign claims its saving, so the upload must name only arms with a live path.
func TestEveryUploadedWinnerCanRunLive(t *testing.T) {
	res, _ := run(t)
	for _, c := range res.Campaign.Cells {
		if c.BestStrategy == "" {
			continue
		}
		if !proxy.CampaignArmEnforceable(c.BestStrategy) {
			t.Errorf("cell %s/%d names %s, which a campaign cannot enforce", c.User, c.HourUTC, c.BestStrategy)
		}
	}
	// Every default arm has a live path, so nothing was re-picked and the report has no
	// "if every arm were live" row: the campaign IS the unrestricted per-cell best.
	if n := res.Report.Campaign.Repicked; n != 0 {
		t.Errorf("%d cells were re-picked, but every default arm can run live", n)
	}
	for _, p := range res.Report.Policies {
		if p.Name == "campaign-if-every-arm-were-live" {
			t.Errorf("the report has %q although every candidate can run live", p.Name)
		}
	}
}

// ONLY THE ALLOWED ARMS. By default every cell is decided over the 5-minute tier only: no cell may
// recommend no-cache or a 1-hour arm, and the K-ping arms are scored at their own budgets.
func TestTheDefaultCampaignUsesOnlyFiveMinuteArms(t *testing.T) {
	res, _ := run(t)
	allowed := map[string]bool{}
	for _, a := range DefaultArms() {
		allowed[a] = true
	}
	for _, c := range res.Campaign.Cells {
		if c.BestStrategy != "" && !allowed[c.BestStrategy] {
			t.Errorf("cell %s/%d recommends %q, outside the 5-minute arms", c.User, c.HourUTC, c.BestStrategy)
		}
	}
	for _, p := range res.Report.Policies {
		if p.Kind == "arm" && !allowed[p.Name] {
			t.Errorf("the report scores %q, which is not a candidate", p.Name)
		}
	}
	for _, banned := range []string{kvcache.StrategyNoCache, kvcache.StrategyFixed1h, kvcache.StrategyKeepAlive1h,
		kvcache.StrategyKeepAlive1hOnce, kvcache.StrategyExtend1h, kvcache.StrategyStickySession1h} {
		if allowed[banned] {
			t.Errorf("%s is in DefaultArms", banned)
		}
	}
	// x1 must equal keepalive-5m-once's schedule and the x11 arm must be scored at 11, not capped at 2.
	x2, x11 := policy(t, res.Report, "keepalive-5m-x2"), policy(t, res.Report, "keepalive-5m-x11")
	if x2.CostUSD == x11.CostUSD {
		t.Errorf("x2 and x11 cost the same ($%.6f): the simulator capped x11 at the default MaxPings", x2.CostUSD)
	}
}
