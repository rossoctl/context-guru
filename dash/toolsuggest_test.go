package dash

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// day is a millisecond offset helper; the fixtures below are dated because the sufficiency
// rule has a SPAN clause and a same-instant fixture cannot exercise it.
func day(n int) int64 {
	return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n).UnixMilli()
}

// seedRemoval writes sessions that differ in exactly the ways the sufficiency rule cares
// about, so each clause of it is exercised by a real row rather than by a unit call:
//
//	Workflow   6 sessions over 10 days, never invoked  -> sufficient
//	Bash       the same 6 sessions, invoked            -> used, never offered
//	Fresh      2 sessions on one day, never invoked    -> too few, too short
//	Spread     6 sessions on ONE day, never invoked    -> enough sessions, no variety
//	nocapture  a session with request rows and NO inventory
func seedRemoval(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	mk := func(session string, ts int64, filtered int) {
		for i := 0; i < 5; i++ { // 5 cache-hit requests per session
			e := mkEvent(ts+int64(i), session, "claude", 100, 90)
			e.TenantID, e.Tools = "t1", 4
			e.FilteredDeclTokens = filtered
			evs = append(evs, e)
		}
	}
	var msgs []invMsg
	decl := func(session string, ts int64, names ...string) {
		var ds []Decl
		for _, n := range names {
			ds = append(ds, Decl{Kind: KindTool, Name: n, Tokens: 1000})
		}
		msgs = append(msgs, invMsg{tenant: "t1", session: session, ts: ts, inv: &Inventory{
			Digest: session, Decls: ds,
			Used:           []Used{{Name: "Bash", Calls: 3}},
			UseFingerprint: uint64(ts),
		}})
	}
	for i := 0; i < 6; i++ {
		s := "long" + string(rune('a'+i))
		mk(s, day(2*i), 0) // days 0,2,4,6,8,10
		decl(s, day(2*i), "Bash", "Workflow")
	}
	for i := 0; i < 6; i++ {
		s := "same" + string(rune('a'+i))
		mk(s, day(20)+int64(i), 0)
		decl(s, day(20)+int64(i), "Spread")
	}
	for i := 0; i < 2; i++ {
		s := "few" + string(rune('a'+i))
		mk(s, day(30)+int64(i), 0)
		decl(s, day(30)+int64(i), "Fresh")
	}
	mk("nocapture", day(40), 0)
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	return db
}

func suggestionsByName(rep *ToolFilterDoc) map[string]Suggestion {
	out := map[string]Suggestion{}
	for _, s := range rep.Suggestions {
		out[s.Name] = s
	}
	return out
}

// TestSuggestionsRequireSufficientObservation is safety rule 7: absence of evidence never
// authorises removal. Each excluded item below is excluded for a DIFFERENT reason, and all
// three reasons have to hold or the rule is decorative.
func TestSuggestionsRequireSufficientObservation(t *testing.T) {
	db := seedRemoval(t)
	rep, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	got := suggestionsByName(rep)
	s, ok := got["Workflow"]
	if !ok {
		t.Fatalf("Workflow was not suggested; got %v", rep.Suggestions)
	}
	if s.Sessions != 6 || s.Days < 9 || s.Tokens != 1000 {
		t.Errorf("Workflow evidence = %+v, want 6 sessions over ~10 days", s)
	}
	if s.Basis == "" || !strings.Contains(s.Basis, "6 of your sessions since 2026-06-01") {
		t.Errorf("basis does not state what it rests on: %q", s.Basis)
	}
	if s.ProjectedUSD <= 0 {
		t.Error("a suggestion with no projected value is not worth showing")
	}
	for name, why := range map[string]string{
		"Bash":   "it was invoked",
		"Fresh":  "only 2 sessions observed",
		"Spread": "6 sessions but all in one day",
	} {
		if _, bad := got[name]; bad {
			t.Errorf("%s was suggested even though %s", name, why)
		}
	}
	// Withheld is reported so a short list cannot be mistaken for "nothing is wasted".
	if rep.Withheld < 2 {
		t.Errorf("withheld = %d, want at least the two never-used items below the bar", rep.Withheld)
	}
	// Coverage carries through: the session with request rows and no inventory is counted
	// as not-captured and contributes to nothing.
	if rep.Coverage.NotCaptured != 1 {
		t.Errorf("coverage = %+v, want exactly one not-captured session", rep.Coverage)
	}
	if rep.MinSessions != suggestMinSessions || rep.MinDays != 7 {
		t.Errorf("thresholds not reported: %d/%v", rep.MinSessions, rep.MinDays)
	}
}

// TestSuggestionsNeverCrossTenants: the removal report drives a change to what we SEND, so a
// scoping bug here is worse than one in a read-only chart.
func TestSuggestionsNeverCrossTenants(t *testing.T) {
	db := seedRemoval(t)
	rep, err := db.ToolFilterDocFor(Filter{Tenant: "t2"}, flatPrice, ToolFilterState{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Suggestions) != 0 || rep.Realized != nil {
		t.Errorf("another tenant's data leaked: %+v", rep)
	}
}

// TestRealizedSavingIsZeroUntilTheFilterActs is the point of the whole accounting: a
// permanently-zero saving column is the failure mode this feature's rules exist to prevent,
// so the column is written by the filter and by nothing else. With no filtered request in
// scope the realized figure must be zero even though the PROJECTION is large.
func TestRealizedSavingIsZeroUntilTheFilterActs(t *testing.T) {
	db := seedRemoval(t)
	rep, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// ABSENT, not zero: the page prints "nothing realized" for a missing figure and would
	// otherwise have to guess whether a zero means "no saving" or "no data".
	if rep.Realized != nil {
		t.Errorf("realized saving without a filtered request: %+v", rep.Realized)
	}
	if len(rep.Suggestions) == 0 {
		t.Fatal("nothing projected, so the contrast is untested")
	}
}

// TestRealizedSavingPricesTheTierActuallyBilled is where a sloppy number becomes a lie. The
// same 1,000 removed tokens are worth ten times more on the turn that CREATED the prefix
// than on a turn that read it from cache, and pricing every request at the fresh-input rate
// would inflate the whole figure tenfold.
func TestRealizedSavingPricesTheTierActuallyBilled(t *testing.T) {
	db := openTestDB(t)
	// Three requests of one session, one per tier, each having dropped 1,000 tokens.
	read := mkEvent(1000, "s", "claude", 100, 90)
	read.TenantID, read.Tools, read.FilteredDeclTokens = "t1", 4, 1000
	read.CacheRead, read.CacheWrite = 5000, 0
	write := mkEvent(1001, "s", "claude", 100, 90)
	write.TenantID, write.Tools, write.FilteredDeclTokens = "t1", 4, 1000
	write.CacheRead, write.CacheWrite = 0, 5000
	fresh := mkEvent(1002, "s", "claude", 100, 90)
	fresh.TenantID, fresh.Tools, fresh.FilteredDeclTokens = "t1", 4, 1000
	fresh.CacheRead, fresh.CacheWrite = 0, 0
	if err := db.insertBatch([]*Event{read, write, fresh}); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeclFilterSavings(Filter{Tenant: "t1"}, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 3 || got.Sessions != 1 || got.Reads != 3000 {
		t.Fatalf("totals = %+v, want 3 requests / 1 session / 3000 tokens", got)
	}
	if got.CacheReadTokens != 1000 || got.CacheWriteTokens != 1000 || got.FreshTokens != 1000 {
		t.Errorf("tier split = %+v, want 1000 in each", got)
	}
	// flatPrice: $1/MTok input, 0.1x read, 1.25x creation. 1000 tokens of each tier:
	// 1e-4 + 1.25e-3 + 1e-3 = 2.35e-3.
	if want := 1e-4 + 1.25e-3 + 1e-3; got.USD < want*0.999 || got.USD > want*1.001 {
		t.Errorf("USD = %g, want %g", got.USD, want)
	}
	if !got.Priced {
		t.Error("priced = false with a pricer that answers")
	}
}

// TestDeclFilterSavingsByTenantMatchesPerTenant is the equivalence check that makes the
// grouped query safe: it must return, for every tenant, exactly what calling
// DeclFilterSavings once per tenant would.
func TestDeclFilterSavingsByTenantMatchesPerTenant(t *testing.T) {
	db := openTestDB(t)
	t1 := mkEvent(1000, "s1", "claude", 100, 90)
	t1.TenantID, t1.Tools, t1.FilteredDeclTokens = "t1", 4, 1000
	t1.CacheRead, t1.CacheWrite = 5000, 0
	t2a := mkEvent(1001, "s2", "claude", 100, 90)
	t2a.TenantID, t2a.Tools, t2a.FilteredDeclTokens = "t2", 4, 2000
	t2a.CacheRead, t2a.CacheWrite = 0, 5000
	t2b := mkEvent(1002, "s2", "claude", 100, 90)
	t2b.TenantID, t2b.Tools, t2b.FilteredDeclTokens = "t2", 4, 500
	t2b.CacheRead, t2b.CacheWrite = 0, 0
	if err := db.insertBatch([]*Event{t1, t2a, t2b}); err != nil {
		t.Fatal(err)
	}
	grouped, err := db.DeclFilterSavingsByTenant(0, flatPrice)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"t1", "t2"} {
		want, err := db.DeclFilterSavings(Filter{Tenant: tenant}, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		got := grouped[tenant]
		if got == nil {
			t.Fatalf("tenant %s: missing from grouped result", tenant)
		}
		if *got != *want {
			t.Errorf("tenant %s: grouped = %+v, per-tenant call = %+v", tenant, *got, *want)
		}
	}
	if got := grouped["t1"].Requests; got != 1 {
		t.Errorf("t1 requests = %d, want 1", got)
	}
	if got := grouped["t2"].Requests; got != 2 {
		t.Errorf("t2 requests = %d, want 2", got)
	}
}

// TestRealizedSavingUnpricedIsNotZero: an unpriced model must not make a real saving read as
// "this cost nothing".
func TestRealizedSavingUnpricedIsNotZero(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(1000, "s", "mystery", 100, 90)
	e.TenantID, e.Tools, e.FilteredDeclTokens = "t1", 4, 1000
	e.CacheRead = 5000
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeclFilterSavings(Filter{Tenant: "t1"}, func(string) (modelinfo.Price, bool) { return modelinfo.Price{}, false })
	if err != nil {
		t.Fatal(err)
	}
	if got.Reads != 1000 {
		t.Errorf("tokens = %d, want 1000", got.Reads)
	}
	if got.Priced {
		t.Error("priced = true for a model with no rates")
	}
}

// TestSuggestNeverOffersWhatCannotBeRemoved: a provider-side tool is resolved by its `type`
// rather than by a schema we can drop; the skills LISTING is one indivisible block; and a
// built-in is Claude Code's own equipment. Offering any of the three would produce either a
// configuration the filter refuses or the worst advice this API can give.
//
// A SKILL is no longer on that list, and that is the point of the change rather than a
// relaxation of it: apply.filterSkillListing removes a skill's listing entry, so the offer now
// has a mechanism behind it. The RemoveAs assertion below is what makes the offer usable — the
// config needs `skill__<name>`, and a suggestion carrying the bare name would write a list entry
// that matches nothing forever.
func TestSuggestNeverOffersWhatCannotBeRemoved(t *testing.T) {
	w := declWindow{sessions: 50, first: day(0), last: day(60)}
	for _, kind := range []string{KindServerTool, KindSkillListing} {
		if _, ok := suggest(ToolStat{Kind: kind, Name: "x", Tokens: 5000}, w); ok {
			t.Errorf("%s was offered for removal", kind)
		}
	}
	if _, ok := suggest(ToolStat{Kind: KindTool, Name: "Read", Tokens: 5000}, w); ok {
		t.Error("a Claude Code built-in was offered for removal")
	}
	s, ok := suggest(ToolStat{Kind: KindSkill, Name: "dataviz", Tokens: 5000}, w)
	if !ok {
		t.Fatal("an unused skill with ample evidence was not offered")
	}
	if s.RemoveAs != "skill__dataviz" {
		t.Errorf("skill RemoveAs = %q, want skill__dataviz — the bare name matches no mechanism",
			s.RemoveAs)
	}
	if _, ok := suggest(ToolStat{Kind: KindTool, Name: "x", Tokens: 5000}, w); !ok {
		t.Error("a plain unused tool with ample evidence was not offered")
	}
	// A zero-weight declaration (defer_loading) saves nothing, so it is not a suggestion.
	if _, ok := suggest(ToolStat{Kind: KindTool, Name: "x", Tokens: 0}, w); ok {
		t.Error("a zero-weight declaration was offered")
	}
}

// TestRealizedIsOmittedNotZeroed is the wire-level half of the realized-vs-projected rule.
// The consumer prints "nothing realized" for an ABSENT field; a zero-valued object there
// would render as a measured $0.00, and a page that then borrowed the projection to fill the
// gap would be showing a forecast as a fact.
func TestRealizedIsOmittedNotZeroed(t *testing.T) {
	db := seedRemoval(t)
	doc, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"realized"`) {
		t.Errorf("realized is present with nothing measured: %s", b)
	}
	// Excluded is never nil, so a consumer can count it without a guard.
	if !strings.Contains(string(b), `"excluded":[]`) {
		t.Errorf("excluded is not an empty array: %s", b)
	}
	// And it appears the moment the filter has acted.
	e := mkEvent(day(50), "acted", "claude", 100, 90)
	e.TenantID, e.Tools, e.FilteredDeclTokens, e.CacheRead = "t1", 4, 1000, 5000
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	doc, err = db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice, ToolFilterState{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Realized == nil || doc.Realized.Reads != 1000 || doc.Realized.Since != day(50) {
		t.Errorf("realized = %+v, want 1000 reads since day 50", doc.Realized)
	}
}

// TestExcludedClassifiesTheConfiguredNames: the page matches an exclusion against an
// inventory row by kind AND name, so a wrong kind makes a switch look off when it is on.
func TestExcludedClassifiesTheConfiguredNames(t *testing.T) {
	got := excludedFrom([]string{"mcp__pw", "Workflow", "mcp__pw__click"}, 1234)
	want := []ExcludedDecl{
		{Kind: KindTool, Name: "Workflow", Since: 1234},
		{Kind: "mcp_server", Name: "mcp__pw", Server: "pw", Since: 1234},
		{Kind: KindMCPTool, Name: "mcp__pw__click", Server: "pw", Since: 1234},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAnExcludedItemIsNotAlsoSuggested: once removed it is a realized saving, and offering it
// again would invite a second write that changes nothing and re-anchors the prefix for free.
func TestAnExcludedItemIsNotAlsoSuggested(t *testing.T) {
	db := seedRemoval(t)
	doc, err := db.ToolFilterDocFor(Filter{Tenant: "t1"}, flatPrice,
		ToolFilterState{Enabled: true, Removed: []string{"Workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := suggestionsByName(doc)["Workflow"]; bad {
		t.Error("an already-excluded declaration was suggested again")
	}
}

// TestTheRemovalControlIsLiveForAManagerViewingOneAccount is the read-side twin of the bug
// writeToolFilterDoc's comment records, which shipped fixed on the write path and broken here.
//
// A manager's DEFAULT scope is the whole service, so f.Tenant is "". The state lookup was handed
// that empty id, the registry had no such account, and the document came back
// `enabled:false, reason:"could not read your account"` — so every opt-out switch on the
// Inventory tab rendered DISABLED for the only role permitted to use them, under a message
// claiming something was wrong with their account. Nothing was: a removal list belongs to one
// account and the request named none.
//
// Both directions are asserted. The service-wide view must refuse with a reason that says what to
// do, and the one-account view must be LIVE — a fix that disabled it everywhere would satisfy
// half a test.
func TestTheRemovalControlIsLiveForAManagerViewingOneAccount(t *testing.T) {
	api := &API{auth: func(*http.Request) (Principal, bool) {
		return Principal{TenantID: "boss", Manager: true}, true
	}}
	api.SetToolFilterState(func(id string) ToolFilterState {
		if id == "" {
			// What a real registry does with an empty id, which is the whole bug.
			return ToolFilterState{Reason: "could not read your account"}
		}
		return ToolFilterState{Enabled: true, Removed: []string{"Workflow"}}
	})

	// A manager viewing ONE account — their own, or somebody's — gets a live control.
	for _, f := range []Filter{{Tenant: "boss"}, {Tenant: "someone-else"}} {
		st := api.toolFilterStateForScope(f)
		if !st.Enabled {
			t.Errorf("scope %+v: the control is disabled for a manager viewing one account "+
				"(reason %q); the switches on the Inventory tab are dead for the only role "+
				"allowed to use them", f, st.Reason)
		}
	}

	// The service-wide view refuses, and the refusal has to be ACTIONABLE rather than a claim
	// that the account could not be read.
	st := api.toolFilterStateForScope(Filter{TenantAll: true})
	if st.Enabled {
		t.Error("the control is live while viewing every account: a switch there would edit one " +
			"account's configuration from a page describing all of them")
	}
	if strings.Contains(st.Reason, "could not read") {
		t.Errorf("the service-wide refusal blames the account: %q\n"+
			"Nothing failed — the request named no account. A reader told their account cannot "+
			"be read goes looking for a broken account.", st.Reason)
	}
	for _, want := range []string{"account"} {
		if !strings.Contains(st.Reason, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what to do: %q",
				want, st.Reason)
		}
	}
	if st.Reason == "" {
		t.Error("the refusal carries no reason at all, so the page can only guess")
	}
}

// Single-tenant is NOT the ambiguous case: TenantAll is the only scope there is, so the answer is
// the deployment's own ("this proxy has no per-account configuration"), not "pick an account"
// against a selector that does not exist.
func TestSingleTenantKeepsItsOwnReason(t *testing.T) {
	api := &API{} // auth nil == single-tenant
	st := api.toolFilterStateForScope(Filter{TenantAll: true})
	if st.Enabled {
		t.Error("enabled with no configuration hook at all")
	}
	if strings.Contains(st.Reason, "account selector") || strings.Contains(st.Reason, "Pick an") {
		t.Errorf("single-tenant was told to pick an account: %q", st.Reason)
	}
	if !strings.Contains(st.Reason, "per-account configuration") {
		t.Errorf("single-tenant reason = %q, want the deployment's own answer", st.Reason)
	}
}
