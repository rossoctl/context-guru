package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GET /api/history — what the user's own past says about their capabilities.
//
// A DIFFERENT CLAIM FROM THE REST OF THE DASHBOARD, and the whole reason it is a separate
// surface. Every other figure here is "this cost you X and you never used it", which needs the
// proxy to have watched the traffic. This one is "you used this / you never used this", which a
// transcript can answer on its own — so it works on the day the proxy is installed, over months
// of history the proxy never saw.
//
// It also has to be careful about what it does NOT say. Whether a capability was used is in the
// history; what carrying it cost is not. The surface names candidates and refuses to price them,
// because a price derived from a transcript would be invented and would look authoritative.

func historyFor(t *testing.T, mux *http.ServeMux, query string) *HistoryDoc {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/history"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/history%s = %d: %s", query, rr.Code, rr.Body.String())
	}
	var doc HistoryDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return &doc
}

// The headline: what was invoked, and over what span.
func TestTheHistoryReportsWhatWasInvokedAndWhen(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(30), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 100},
		{TS: daysAgo(2), SessionID: "s2", Model: "m", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	// Two capabilities used, at different times.
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(30), isSkill: true},
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(25), isSkill: true},
		{session: "s2", name: "Bash", ts: daysAgo(2)},
	}); err != nil {
		t.Fatal(err)
	}
	mux := projectAPI(t, db)
	doc := historyFor(t, mux, "")

	byName := map[string]HistoryUse{}
	for _, u := range doc.Used {
		byName[u.Name] = u
	}
	bs, ok := byName["superpowers:brainstorming"]
	if !ok {
		t.Fatalf("the invoked skill is missing from Used: %+v", doc.Used)
	}
	if bs.Calls != 2 {
		t.Errorf("calls = %d; want 2", bs.Calls)
	}
	// FIRST AND LAST, because "used once in March" and "used daily since March" are different
	// facts and a bare count cannot tell them apart.
	if bs.FirstTS == 0 || bs.LastTS == 0 {
		t.Errorf("first/last = %d/%d; a date is what makes 'used' checkable against the "+
			"reader's own memory", bs.FirstTS, bs.LastTS)
	}
	if bs.FirstTS > bs.LastTS {
		t.Errorf("first %d is after last %d", bs.FirstTS, bs.LastTS)
	}
	if !bs.Skill {
		t.Error("the skill is not flagged as one, so the view cannot separate skills from tools")
	}
	if byName["Bash"].Skill {
		t.Error("Bash is flagged as a skill")
	}
	// The span covered, so a reader knows how much history the claim rests on.
	if doc.FirstTS == 0 || doc.LastTS == 0 {
		t.Error("the document does not state its own span; 'never used' means nothing without it")
	}
}

// THE FINDING THE IMPORT EXISTS FOR: installed on this machine, never invoked in the history.
//
// This is the one answer available on install day, and it needs no inference: the inventory is on
// disk and the invocations are in the transcripts. "You have 46 skills and have used 10 of them"
// is actionable immediately, where a cost figure would have to wait for the proxy to watch.
func TestTheHistoryNamesWhatWasNeverUsed(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// Two skills installed on disk.
	mkSkill(t, home, "superpowers", "brainstorming")
	mkSkill(t, home, "superpowers", "never-touched")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// Only one of them was ever called.
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(10), isSkill: true},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.NeverUsed) != 1 || doc.NeverUsed[0] != "superpowers:never-touched" {
		t.Fatalf("NeverUsed = %v; want exactly the installed skill that was never called",
			doc.NeverUsed)
	}
	if doc.InstalledSkills != 2 {
		t.Errorf("InstalledSkills = %d; want 2 — the denominator is half the finding",
			doc.InstalledSkills)
	}
}

// IT MUST NOT PRICE WHAT IT CANNOT MEASURE. A transcript records what a request cost in total and
// not which tools it declared, so the cost of CARRYING an unused skill is not recoverable. A
// figure here would be invented, and would look more authoritative than the measured ones beside
// it because it would cover a longer span.
func TestTheHistoryRefusesToPriceWhatItCannotMeasure(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	mkSkill(t, home, "superpowers", "never-touched")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 999999},
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := db.HistoryDocFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Basis == "" {
		t.Fatal("the document states no basis, so a reader cannot tell what kind of claim it is")
	}
	// The basis has to say the cost is absent, not merely omit it.
	for _, want := range []string{"cost", "not"} {
		if !containsFold(doc.Basis, want) {
			t.Errorf("Basis = %q; it has to say that what carrying these COST is not in this "+
				"data, or a reader will assume the omission means zero", doc.Basis)
		}
	}
}

// The spend it CAN report: per project, from imported and measured rows alike, since usage is the
// one thing both sources agree on.
func TestTheHistoryReportsSpendPerProject(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(20), SessionID: "s1", Model: "m", Source: SourceImported,
			Project: "/Users/v/a", CacheWrite: 1000, FreshInput: 100},
		{TS: daysAgo(19), SessionID: "s2", Model: "m", Source: SourceImported,
			Project: "/Users/v/a", CacheWrite: 1000, FreshInput: 100},
		{TS: daysAgo(2), SessionID: "s3", Model: "m", Project: "/Users/v/b", TokensBefore: 10},
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := db.HistoryDocFor(Filter{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	byProj := map[string]int64{}
	for _, p := range doc.Projects {
		byProj[p.Project] = p.Requests
	}
	if byProj["/Users/v/a"] != 2 {
		t.Errorf("project a has %d requests; want 2", byProj["/Users/v/a"])
	}
	if byProj["/Users/v/b"] != 1 {
		t.Errorf("project b has %d requests; want 1", byProj["/Users/v/b"])
	}
}

// mkSkill creates an installed plugin skill on disk, as the plugin cache lays them out.
func mkSkill(t *testing.T, home, plugin, name string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "plugins", "cache", "mkt", plugin, "1.0.0",
		"skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: "+name+"\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// containsFold is a case-insensitive substring check, for asserting on prose.
func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// THE REPORTER STAYS OUT OF ITS OWN REPORT, under every name it has ever had.
//
// insightsshape.go already keeps `context-guru:*` skills out of the insights tables. The history
// surface reaches further back than that rule was written for: it reads transcripts that predate
// the current plugin name, so a prefix match on today's name alone lets the old one through. Found
// on real history — `cg:insights` and `cg:status` appeared in the used list beside
// `context-guru:status`, all four of them this tool measuring itself.
func TestTheHistoryKeepsContextGuruOutOfItsOwnReport(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(30), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.insertImportedUses([]importedUse{
		// The current name, the old one, and a genuine third-party skill that must survive.
		{session: "s1", name: "context-guru:status", ts: daysAgo(30), isSkill: true},
		{session: "s1", name: "cg:insights", ts: daysAgo(30), isSkill: true},
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(30), isSkill: true},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range doc.Used {
		if u.Name == "context-guru:status" || u.Name == "cg:insights" {
			t.Errorf("%q is in the used list; this tool must not appear in its own report, and "+
				"that holds for every name it has shipped under", u.Name)
		}
	}
	var kept bool
	for _, u := range doc.Used {
		if u.Name == "superpowers:brainstorming" {
			kept = true
		}
	}
	if !kept {
		t.Error("a third-party skill was dropped along with the self-owned ones; the exclusion is " +
			"too wide")
	}

	// EXCLUDED, BUT SAID OUT LOUD. Held out of the figures and still reported, because this tool's
	// own skills do occupy the prompt: a reader who counts more skills in `/plugin` than appear
	// here has to be able to find out why, and "the tool hid its own cost" is a worse look than
	// the cost. The insights page already reports its own weight this way.
	if doc.SelfExcluded.Items != 2 {
		t.Errorf("SelfExcluded.Items = %d; want 2 — both self-owned names, counted rather than "+
			"silently dropped. Excluding this tool from its own figures is right; hiding that it "+
			"costs tokens at all is not", doc.SelfExcluded.Items)
	}
	if doc.SelfExcluded.Plugin == "" {
		t.Error("SelfExcluded names no plugin, so a reader cannot tell whose cost was held out")
	}
}

// THE COUNTS MUST PARTITION. "10 used" beside "40 never used, 46 installed" does not add up, and a
// reader who tries the arithmetic loses trust in every other figure on the page.
//
// The gap is real rather than cosmetic: a used capability that is not in the on-disk inventory — a
// built-in skill, or one installed under a name since changed — belongs in neither set. So the
// document states the overlap itself instead of leaving it to be inferred.
func TestTheHistoryCountsPartitionTheInstalledInventory(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	mkSkill(t, home, "superpowers", "brainstorming")
	mkSkill(t, home, "superpowers", "never-touched")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(10), isSkill: true},
		// Used, a skill, and NOT installed on disk — the case that breaks naive arithmetic.
		{session: "s1", name: "some-removed-plugin:gone", ts: daysAgo(10), isSkill: true},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	if doc.UsedInstalled != 1 {
		t.Errorf("UsedInstalled = %d; want 1 — the one installed skill that was invoked",
			doc.UsedInstalled)
	}
	// The whole point: these two must account for the inventory exactly.
	if doc.UsedInstalled+len(doc.NeverUsed) != doc.InstalledSkills {
		t.Errorf("used-installed %d + never-used %d = %d, but %d skills are installed; the "+
			"figures on the page have to add up or none of them is believable",
			doc.UsedInstalled, len(doc.NeverUsed),
			doc.UsedInstalled+len(doc.NeverUsed), doc.InstalledSkills)
	}
}
