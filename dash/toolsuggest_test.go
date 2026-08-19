package dash

import (
	"encoding/json"
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
// rather than by a schema we can drop, and a skill is prose in a message rather than an
// element of tools[] — offering either would produce a configuration the filter refuses.
func TestSuggestNeverOffersWhatCannotBeRemoved(t *testing.T) {
	w := declWindow{sessions: 50, first: day(0), last: day(60)}
	for _, kind := range []string{KindServerTool, KindSkill, KindSkillListing} {
		if _, ok := suggest(ToolStat{Kind: kind, Name: "x", Tokens: 5000}, w); ok {
			t.Errorf("%s was offered for removal", kind)
		}
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
