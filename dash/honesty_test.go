package dash

import (
	"database/sql"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// TestContentComponentsRoundTrip: exact diff attribution is only useful if it survives
// the store. The UI reads it from the request payload, so it has to come back out in the
// order it went in.
func TestContentComponentsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(time.Now().UnixMilli(), "s1", "m", 1000, 800)
	e.Content = []ContentRow{
		{Path: "messages.2", Before: "long", After: "<<cg:abc>>", Components: []string{"mask", "collapse"}},
		{Path: "messages.3", Before: "x", After: "y"}, // no attribution recorded
	}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Request(e.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 2 {
		t.Fatalf("content rows = %d, want 2", len(got.Content))
	}
	if want := []string{"mask", "collapse"}; !reflect.DeepEqual(got.Content[0].Components, want) {
		t.Errorf("attribution did not survive the store: %v, want %v", got.Content[0].Components, want)
	}
	if got.Content[1].Components != nil {
		t.Errorf("an unattributed row must come back empty, not [\"\"]: %v", got.Content[1].Components)
	}
}

// TestArchiveReportsConfiguredRemoteAndReachability: with cold storage configured but
// the boot probe failed, the host leaves Options.Remote nil. The payload used to report
// an empty remote name while listing archived rows, so the UI said "cold storage is not
// configured on this deployment" directly above two archived sessions. "Not configured"
// and "configured but unreachable right now" are different facts.
func TestArchiveReportsConfiguredRemoteAndReachability(t *testing.T) {
	// Configured, probe failed: a name, no Remote.
	a, rec := newTestAPI(t, Options{RemoteName: "box:context-guru"})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "s-cold", "m", 100, 90))
	if err := rec.DB().markArchived(
		coldCandidate{SessionID: "s-cold", LastTS: 1, Requests: 1},
		ArchiveFull, "archive/_single/2026/08/s-cold.full.jsonl.gz", 10, "box:context-guru"); err != nil {
		t.Fatal(err)
	}
	w, body := get(t, a, "/api/archive", "127.0.0.1:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("/api/archive = %d", w.Code)
	}
	if got := body["remote"]; got != "box:context-guru" {
		t.Errorf("remote = %v, want the CONFIGURED name — an empty name reads as 'not configured' "+
			"while the list below it shows archived sessions", got)
	}
	if got := body["reachable"]; got != false {
		t.Errorf("reachable = %v, want false: the boot probe failed", got)
	}
	if rows, _ := body["archived"].([]any); len(rows) != 1 {
		t.Fatalf("expected the archive index to still be listed, got %v", body["archived"])
	}

	// Nothing configured at all: both facts empty/false, which is the OTHER answer.
	a2, _ := newTestAPI(t, Options{})
	_, body2 := get(t, a2, "/api/archive", "127.0.0.1:5000")
	if body2["remote"] != "" || body2["reachable"] != false {
		t.Errorf("with no cold storage configured, want remote=\"\" reachable=false, got %v / %v",
			body2["remote"], body2["reachable"])
	}

	// Configured AND reachable.
	a3, _ := newTestAPI(t, Options{Remote: newMemRemote(), RemoteName: "box:context-guru"})
	_, body3 := get(t, a3, "/api/archive", "127.0.0.1:5000")
	if body3["remote"] != "box:context-guru" || body3["reachable"] != true {
		t.Errorf("with cold storage up, want remote set and reachable=true, got %v / %v",
			body3["remote"], body3["reachable"])
	}
}

// TestCaptureDescriptionMatchesThePayload: the counters are zeroed for a non-manager
// (they are process-wide), so the paragraph explaining captured/written/dropped was
// describing data that is not in the response — which reads as a broken deployment.
func TestCaptureDescriptionMatchesThePayload(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	seed(t, rec, mkEvent(time.Now().UnixMilli(), "s1", "m", 100, 90))

	var manager bool
	a.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme", Manager: manager}, true
	})

	_, body := get(t, a, "/api/capture", "127.0.0.1:5000")
	desc, _ := body["description"].(string)
	for _, word := range []string{"Captured", "written", "dropped"} {
		if strings.Contains(desc, word) {
			t.Errorf("non-manager description still explains %q, a counter zeroed out of this payload: %q",
				word, desc)
		}
	}
	if desc == "" {
		t.Error("non-manager got no description at all; it should describe what IS in the payload")
	}

	manager = true
	_, mbody := get(t, a, "/api/capture", "127.0.0.1:5000")
	if !strings.Contains(mbody["description"].(string), "dropped") {
		t.Errorf("a manager DOES see the counters, so the full description must stay: %q", mbody["description"])
	}
}

// hostedCaptureAPI is a hosted API for one tenant, with both halves of the capture
// decision under the test's control: the operator's process-wide flag and that tenant's
// own consent. The pairing is the whole point — the reported state has to be the AND of
// them, and the reported cause has to name whichever one is off.
func hostedCaptureAPI(t *testing.T, operator, tenantConsent bool) (*API, *Recorder) {
	t.Helper()
	a, rec := newTestAPI(t, Options{CaptureContent: operator, ContentCap: 4096})
	a.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme"}, true
	})
	a.SetTenantCapture(func(id string) bool { return id == "acme" && tenantConsent })
	return a, rec
}

// TestCapturedReportsTheEffectiveDecisionAndWhoCanChangeIt.
//
// `content_captured` used to be the PROCESS-GLOBAL Options.CaptureContent, but the real
// decision is proxy.captureContentFor: the operator's gate AND that tenant's consent.
// Reporting the flag alone is wrong in both directions on a hosted service — it claims
// "captured" to an account that never consented, and when the OPERATOR's gate is the one
// that is off it still renders as the tenant's own setting, so the dashboard told users
// to go and enable something they had already enabled. That was the live symptom.
func TestCapturedReportsTheEffectiveDecisionAndWhoCanChangeIt(t *testing.T) {
	for _, tc := range []struct {
		name             string
		operator, tenant bool
		wantCaptured     bool
		wantBlockedBy    string
	}{
		{"both on", true, true, true, ""},
		// The one the process flag got backwards: capture is on service-wide, this
		// account never opted in, and the flag said its content was being captured.
		{"tenant has not consented", true, false, false, CaptureBlockedByTenant},
		// The live defect: the operator's gate is off, so no message about the tenant's
		// own setting can be true — they cannot fix this and must be told who can.
		{"operator gate off", false, true, false, CaptureBlockedByOperator},
		// Both off: the operator's gate is the outer one, so it is the honest answer —
		// turning on their consent would still capture nothing.
		{"both off", false, false, false, CaptureBlockedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, rec := hostedCaptureAPI(t, tc.operator, tc.tenant)
			e := mkEvent(time.Now().UnixMilli(), "sess-1", "m", 1000, 1000)
			e.TenantID = "acme"
			seed(t, rec, e)

			for _, path := range []string{
				"/api/sessions/sess-1/transcript",
				"/api/requests/" + itoa(e.ID),
			} {
				_, body := get(t, a, path, "127.0.0.1:5000")
				if body["content_captured"] != tc.wantCaptured {
					t.Errorf("%s: content_captured = %v, want %v (operator=%v tenant=%v)",
						path, body["content_captured"], tc.wantCaptured, tc.operator, tc.tenant)
				}
				if got, _ := body["capture_blocked_by"].(string); got != tc.wantBlockedBy {
					t.Errorf("%s: capture_blocked_by = %q, want %q — the UI cannot say which "+
						"party has to act without it", path, got, tc.wantBlockedBy)
				}
			}
		})
	}
}

// Single-tenant mode has no consent layer, so the operator's flag IS the effective
// decision — and nothing may be blamed on a tenant that does not exist.
func TestSingleTenantCaptureStateIsJustTheOperatorFlag(t *testing.T) {
	for _, operator := range []bool{true, false} {
		a, rec := newTestAPI(t, Options{CaptureContent: operator, ContentCap: 4096})
		e := mkEvent(time.Now().UnixMilli(), "sess-local", "m", 1000, 1000)
		seed(t, rec, e)
		_, body := get(t, a, "/api/sessions/sess-local/transcript", "127.0.0.1:5000")
		if body["content_captured"] != operator {
			t.Errorf("single-tenant content_captured = %v, want %v", body["content_captured"], operator)
		}
		want := ""
		if !operator {
			want = CaptureBlockedByOperator
		}
		if got, _ := body["capture_blocked_by"].(string); got != want {
			t.Errorf("single-tenant capture_blocked_by = %q, want %q: there is no tenant here "+
				"to hold responsible", got, want)
		}
	}
}

// An unknown session id must carry a state like every other answer this route gives.
// Nine states and one code path answering with a bare error means every client needs a
// state machine plus a special case — and the drawer is becoming linkable, so this is
// about to be reachable from a stale bookmark rather than unreachable.
func TestUnknownSessionStillCarriesAState(t *testing.T) {
	a, _ := newTestAPI(t, Options{CaptureContent: true})
	w, body := get(t, a, "/api/sessions/no-such-session/transcript", "127.0.0.1:5000")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: it really is absent", w.Code)
	}
	if body["state"] != TranscriptUnknownSession {
		t.Errorf("state = %v, want %q", body["state"], TranscriptUnknownSession)
	}
	if body["error"] == nil {
		t.Error("the human-readable error is gone; the UI still shows it")
	}
}

// TestConfigSaysWhoseConfigurationItIs.
//
// Observed live: this route reported preset "codesmart" with extract_llm in the pipeline
// while all 26 requests that day ran preset "custom" and extract_llm never ran once —
// because the tenant followed its own configuration. The page is read as "what compacts
// my traffic", so the payload has to say whose configuration it is. Per-tenant config
// stays with the control plane's /api/me: one source of truth per question.
func TestConfigSaysWhoseConfigurationItIs(t *testing.T) {
	opts := Options{Effective: map[string]any{"preset": "codesmart"}}

	// Single-tenant: it IS every request's configuration, and says so.
	a, _ := newTestAPI(t, opts)
	_, body := get(t, a, "/api/config", "127.0.0.1:5000")
	desc, _ := body["description"].(string)
	if desc == "" {
		t.Error("no description: the page cannot say what configuration this is")
	}
	if body["scope"] != "server" {
		t.Errorf("scope = %v, want \"server\"", body["scope"])
	}
	cfg, _ := body["config"].(map[string]any)
	if cfg["preset"] != "codesmart" {
		t.Fatalf("the configuration itself is missing from the payload: %v", body)
	}

	// Hosted: it is the server DEFAULT, which a tenant may not be running at all. The
	// description has to say so and point at where the caller's own answer lives.
	a2, _ := newTestAPI(t, opts)
	a2.SetAuth(func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "acme", Manager: true}, true
	})
	_, hbody := get(t, a2, "/api/config", "127.0.0.1:5000")
	hdesc, _ := hbody["description"].(string)
	for _, want := range []string{"NOT necessarily", "own configuration"} {
		if !strings.Contains(hdesc, want) {
			t.Errorf("hosted description does not mention %q, so nothing on the page keeps a "+
				"reader from taking the server default for their own: %q", want, hdesc)
		}
	}
}

// ibmOpus is what this deployment actually bills for the opus family: $4.75/MTok fresh and
// $0.38/MTok cached, with the 1.25x creation premium. It is in this file for a reason —
// the UI used to improvise a per-component dollar figure from hardcoded sonnet-class rates
// (3.75/0.30), which is 27% wrong on exactly this deployment. The rate has to come from the
// request's own model at write time, and these tests assert it does.
var ibmOpus = modelinfo.Price{Input: 4.75e-6, Output: 23.75e-6, CacheRead: 3.8e-7, CacheWrite: 5.9375e-6}

// perComponentEvent is one request whose component rows sum to its request-level savings —
// the shape the reconciliation identity is about. gross/unique across the four rows are
// 20,000 and 2,000, matching TokensBefore−TokensAfter and SavedUnique.
func perComponentEvent(read, write, fresh int64) *Event {
	return &Event{
		TS: 1000, SessionID: "s", Model: "aws/claude-opus-5",
		TokensBefore: 100_000, TokensAfter: 80_000, SavedUnique: 2_000,
		FreshInput: fresh, CacheRead: read, CacheWrite: write, OutputTokens: 500,
		Components: []CompRow{
			{Component: "mask", Kind: "reformat", Acted: true, Mutated: true, SavedGross: 12_000, SavedUnique: 900},
			{Component: "extract_llm", Kind: "offload", Acted: true, Mutated: true, SavedGross: 6_000, SavedUnique: 1_100},
			// Gross with no unique: a reduction made on an EARLIER turn, replaying. It is
			// most of realized value (~93% on measured traffic) and it must be valued at the
			// tier this request paid, not at the creation rate.
			{Component: "collapse", Kind: "offload", Acted: true, Mutated: true, SavedGross: 2_000},
			// Ran, saved nothing. Worth $0, not worth "unknown".
			{Component: "cacheinject", Kind: "reformat", Mutated: true},
		},
	}
}

// tierCases are the three billing outcomes a removed token can be valued at, which is the
// whole of the tier rule: a warm turn replays from cache, a cold/TTL-expired turn has the
// entire prompt re-billed as creation, and a non-caching backend bills fresh.
var tierCases = []struct {
	name               string
	read, write, fresh int64
	tier               func(modelinfo.Price) float64
}{
	{"warm", 80_000, 0, 10, func(p modelinfo.Price) float64 { return p.CacheRead }},
	{"cold_ttl", 0, 80_000, 10, func(p modelinfo.Price) float64 { return p.CacheWrite }},
	{"non_caching", 0, 0, 80_000, func(p modelinfo.Price) float64 { return p.Input }},
}

// TestPerComponentSavedUSDReconcilesWithTheBaseline is the identity that makes the
// per-component dollars trustworthy: they are a PARTITION of the request's own saving, not
// a second, independently-computed estimate that can drift from the headline. Reconciled
// against production data at 0.9%; on a fixture whose component rows sum exactly to the
// request's, it has to be exact to float noise.
func TestPerComponentSavedUSDReconcilesWithTheBaseline(t *testing.T) {
	for _, tc := range tierCases {
		t.Run(tc.name, func(t *testing.T) {
			e := perComponentEvent(tc.read, tc.write, tc.fresh)
			e.Price(ibmOpus, true)
			var sum float64
			for _, c := range e.Components {
				sum += c.SavedUSD
			}
			want := e.BaselineCostUSD - e.CostUSD
			if want <= 0 {
				t.Fatalf("fixture removed nothing: baseline %.10f cost %.10f", e.BaselineCostUSD, e.CostUSD)
			}
			if math.Abs(sum-want) > 1e-12 {
				t.Errorf("Σ per-component saved_usd = %.10f, request-level saving = %.10f (error %.3g); "+
					"the components view and the headline would disagree",
					sum, want, math.Abs(sum-want)/want)
			}
		})
	}
}

// TestComponentSavingUsesTheTierTheRequestPaid: the same removals are worth different money
// depending on how the provider billed THIS request, and a component whose saving is pure
// replay earns that tier — not the creation rate. Pricing replay as creation is the ~12.5x
// overstatement the request-level figure already avoids; this pins the per-component path
// against the same mistake.
func TestComponentSavingUsesTheTierTheRequestPaid(t *testing.T) {
	for _, tc := range tierCases {
		t.Run(tc.name, func(t *testing.T) {
			e := perComponentEvent(tc.read, tc.write, tc.fresh)
			e.Price(ibmOpus, true)
			tier := tc.tier(ibmOpus)
			for _, want := range []struct {
				comp           string
				unique, replay float64
			}{
				{"mask", 900, 11_100},
				{"extract_llm", 1_100, 4_900},
				{"collapse", 0, 2_000}, // pure replay
				{"cacheinject", 0, 0},
			} {
				got := compByName(t, e, want.comp).SavedUSD
				exp := want.unique*ibmOpus.CacheWrite + want.replay*tier
				if math.Abs(got-exp) > 1e-12 {
					t.Errorf("%s saved_usd = %.10f, want %.10f (unique at the write rate, "+
						"replay at this request's %s tier)", want.comp, got, exp, tc.name)
				}
			}
			// And on a warm turn the replay term must NOT be the creation rate, which is the
			// specific inflation the tier rule exists to prevent.
			if tc.name == "warm" {
				if got, ceiling := compByName(t, e, "collapse").SavedUSD, 2_000*ibmOpus.CacheWrite; got >= ceiling {
					t.Errorf("a pure-replay component on a warm turn is valued at %.10f, at or above "+
						"the creation rate %.10f — replay sits in the cached prefix", got, ceiling)
				}
			}
		})
	}
}

func compByName(t *testing.T, e *Event, name string) CompRow {
	t.Helper()
	for _, c := range e.Components {
		if c.Component == name {
			return c
		}
	}
	t.Fatalf("no component %q on the event", name)
	return CompRow{}
}

// TestComponentsViewReportsNetNotABareCost.
//
// The components view had a COST for the components that spend and no dollar value at all
// for what they saved, so extract_llm read as pure expense and the honest conclusion was
// unavailable. This pins both halves through the store: the saving survives the round trip
// un-multiplied by the extraction-call join, and net is saving minus spend — including when
// that is negative, which is a real outcome the dashboard shows rather than hides.
func TestComponentsViewReportsNetNotABareCost(t *testing.T) {
	db := openTestDB(t)
	e := perComponentEvent(80_000, 0, 10)
	// Two calls on ONE component: if the saving were summed in the query that joins these,
	// every figure on the row would double.
	e.Extractions = []ExtractionRow{
		{Component: "extract_llm", Model: "aws/claude-haiku-5", SavedTokens: 1_100, CostUSD: 0.004, Accepted: true},
		{Component: "extract_llm", Model: "aws/claude-haiku-5", SavedTokens: 0, CostUSD: 0.004},
	}
	e.Price(ibmOpus, true)
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Components(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*ComponentRow{}
	for _, r := range rows {
		by[r.Component] = r
	}
	for _, c := range e.Components {
		got, ok := by[c.Component]
		if !ok {
			t.Fatalf("component %q missing from the view", c.Component)
		}
		if math.Abs(got.SavedUSD-c.SavedUSD) > 1e-12 {
			t.Errorf("%s saved_usd = %.10f, want the stored %.10f (a 2x here is the "+
				"extraction-call join multiplying the component sums)", c.Component, got.SavedUSD, c.SavedUSD)
		}
		if math.Abs(got.NetUSD-(got.SavedUSD-got.LLMCostUSD)) > 1e-12 {
			t.Errorf("%s net_usd = %.10f, want saved %.10f − spend %.10f",
				c.Component, got.NetUSD, got.SavedUSD, got.LLMCostUSD)
		}
	}
	// The honest number on this fixture: one warm turn, two calls at $0.004 each, against a
	// saving whose replay term is billed at the cached rate. It lands within a rounding error
	// of break-even, which is the actual shape of a single-turn verdict on an LLM component —
	// and the reason a session still inside the cache TTL is marked in_flight rather than
	// judged.
	x := by["extract_llm"]
	if x.LLMCostUSD <= 0 {
		t.Fatalf("the spend did not survive the store: %+v", x)
	}
	t.Logf("extract_llm on this fixture: net %+.6f (saved %.6f, spent %.6f). One warm turn "+
		"barely covers two calls; the sign is data, not a requirement, and the field reports "+
		"whichever it is.", x.NetUSD, x.SavedUSD, x.LLMCostUSD)
	// A deterministic component spends nothing, so its net is exactly its saving. If that
	// were ever not true the view would be inventing a cost for components that have none.
	if m := by["mask"]; math.Abs(m.NetUSD-m.SavedUSD) > 1e-15 || m.LLMCostUSD != 0 {
		t.Errorf("a deterministic component's net is not its saving: %+v", m)
	}
}

// TestComponentSavedUSDArrivesWithoutDiscardingRows: the same rule the requests columns
// follow (see TestAdditiveColumnKeepsExistingRows) applies to request_components. A version
// bump renames the file aside and discards every component row on the live service to gain
// one column, so this one is an ALTER TABLE on open — and the read path has to work over
// rows that predate it, which is where a "successful" migration still fails.
func TestComponentSavedUSDArrivesWithoutDiscardingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{perComponentEvent(80_000, 0, 10)}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE request_components DROP COLUMN saved_usd`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the column failed: %v", err)
	}
	defer db2.Close()
	var n int64
	if err := db2.sql.QueryRow(`SELECT COUNT(*) FROM request_components`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("%d component rows survived the migration, want 4", n)
	}
	rows, err := db2.Components(Filter{})
	if err != nil {
		t.Fatalf("Components over migrated rows: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("the components view lost rows across the migration: %d", len(rows))
	}
	for _, r := range rows {
		if r.SavedUSD != 0 || r.NetUSD != 0 {
			t.Errorf("%s: a pre-column row must read 0, not a fabricated value: %+v", r.Component, r)
		}
	}
	// And the per-request read path, which names the column explicitly.
	var id int64
	if err := db2.sql.QueryRow(`SELECT id FROM requests LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Request(id, false); err != nil {
		t.Fatalf("Request over migrated rows: %v", err)
	}
}

// TestInFlightSessionIsNotAVerdict: a session whose last turn is inside a provider cache
// TTL may still replay its reduction on the next turn, so its net is an incomplete
// amortization. A young session with one extraction call reads underwater and then stops
// reading underwater, and the UI cannot say so without this flag.
func TestInFlightSessionIsNotAVerdict(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	if err := db.insertBatch([]*Event{
		mkEvent(now-1_000, "young", "m", 10_000, 9_000),
		mkEvent(now-30*60*1000, "settled", "m", 10_000, 9_000),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := db.Sessions(Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SessionID] = r.InFlight
	}
	if !got["young"] {
		t.Error("a session that spoke a second ago is not marked in_flight; its net is still being earned")
	}
	if got["settled"] {
		t.Error("a session idle for half an hour is marked in_flight; its amortization is over")
	}
}

// TestPrefixChangeCostIsAnObservationNotADebt. The figure is bigger than every saving on
// the dashboard, so it has to be visible; mutation is not randomly assigned, so it may not
// be netted off. Both halves are the test.
func TestPrefixChangeCostIsAnObservationNotADebt(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, session string, mutated bool, reason string, cost float64) *Event {
		e := &Event{TS: ts, SessionID: session, Model: "m", Provider: "anthropic",
			TokensBefore: 10_000, TokensAfter: 10_000, CostUSD: cost, BaselineCostUSD: cost,
			TokenAccounting: AccountingComplete, CacheMissReason: reason,
			Components: []CompRow{{Component: "mask", Kind: "reformat", Mutated: mutated, Skipped: !mutated}}}
		return e
	}
	if err := db.insertBatch([]*Event{
		// The population that counts: we rewrote history, the next turn missed on a changed prefix.
		mk(1_000, "blamed", true, CacheHit, 0.10),
		mk(2_000, "blamed", false, CachePrefixChange, 0.50),
		// Same miss, but nothing had mutated on the previous turn — not ours to look at.
		mk(1_000, "clean", false, CacheHit, 0.10),
		mk(2_000, "clean", false, CachePrefixChange, 0.70),
		// Mutated, but the miss was the TTL, which wins ties and is not a prefix change.
		mk(1_000, "expired", true, CacheHit, 0.10),
		mk(2_000, "expired", false, CacheTTLExpiry, 0.90),
	}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(o.PrefixChangeCost-0.50) > 1e-12 {
		t.Errorf("prefix_change_cost_usd = %.4f, want 0.50 — only the turn that missed on a "+
			"changed prefix AFTER a mutating turn belongs in it", o.PrefixChangeCost)
	}
	// It is a diagnostic. Net is baseline − cost − our spend and nothing else; subtracting an
	// unrandomized correlation from it would book a hypothesis as money owed.
	if want := o.BaselineCostUSD - o.CostUSD - o.CGLLMCostUSD; math.Abs(o.NetSavedUSD-want) > 1e-12 {
		t.Errorf("net_saved_usd = %.6f, want %.6f: the prefix-change diagnostic has been "+
			"subtracted from net", o.NetSavedUSD, want)
	}
	for _, s := range o.Waterfall {
		if strings.Contains(s.Key, "prefix_change") {
			t.Errorf("the prefix-change diagnostic is a step in the savings waterfall: %+v", s)
		}
	}
}
