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

// "NEVER USED" IS A MACHINE-WIDE CLAIM, EVEN WHEN A PROJECT IS SELECTED.
//
// THE BUG THIS FIXES, found by selecting a project on the live dashboard. The used list was
// scoped to the folder and the installed inventory was not, so every project reported "43 of your
// 46 skills are unused" — true of the machine, never of one folder. On the busiest project the
// page said nothing had been used at all while listing fifteen invoked capabilities beside it.
//
// The scope mismatch is the mechanical part; the reason for fixing it THIS way is the real one. A
// skill unused in one folder is not waste — a user runs `brainstorming` in their notes project and
// never in a Go repo, and that is correct behaviour rather than a finding. Scoping the claim to
// the folder would report deliberate, sensible usage as abandonment. So the never-used set is
// computed over the whole machine and the document says which scope each figure carries.
func TestNeverUsedStaysMachineWideUnderAProjectFilter(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	mkSkill(t, home, "superpowers", "brainstorming")
	mkSkill(t, home, "superpowers", "never-touched")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(20), SessionID: "s-notes", Model: "m", Source: SourceImported,
			Project: "/Users/v/notes", CacheWrite: 100},
		{TS: daysAgo(10), SessionID: "s-code", Model: "m", Source: SourceImported,
			Project: "/Users/v/code", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// brainstorming is used in `notes` and never in `code` — the normal, correct pattern.
	if err := db.insertImportedUses([]importedUse{
		{session: "s-notes", name: "superpowers:brainstorming", ts: daysAgo(20), isSkill: true,
			project: "/Users/v/notes"},
		{session: "s-code", name: "Bash", ts: daysAgo(10), project: "/Users/v/code"},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{Project: "/Users/v/code"}, home)
	if err != nil {
		t.Fatal(err)
	}
	// The used list is the FOLDER's: that is what the reader selected a project to see.
	var sawBash bool
	for _, u := range doc.Used {
		if u.Name == "Bash" {
			sawBash = true
		}
		if u.Name == "superpowers:brainstorming" {
			t.Error("the used list leaked an invocation from another project; selecting a project " +
				"has to scope what was used here")
		}
	}
	if !sawBash {
		t.Error("the selected project's own invocation is missing from the used list")
	}
	// But NEVER-USED is the machine's. brainstorming was used somewhere, so calling it unused
	// would report deliberate usage as waste.
	for _, n := range doc.NeverUsed {
		if n == "superpowers:brainstorming" {
			t.Error("superpowers:brainstorming is reported as never used because it was not used " +
				"in THIS project. It was used in another one — a skill you run in your notes and " +
				"not in a Go repo is not waste, and reporting it as such is a false positive")
		}
	}
	if len(doc.NeverUsed) != 1 || doc.NeverUsed[0] != "superpowers:never-touched" {
		t.Fatalf("NeverUsed = %v; want only the skill used in no project at all", doc.NeverUsed)
	}
	// The overlap is machine-wide too, so it still partitions the inventory.
	if doc.UsedInstalled != 1 {
		t.Errorf("UsedInstalled = %d; want 1 — machine-wide, matching NeverUsed's scope",
			doc.UsedInstalled)
	}
	if doc.UsedInstalled+len(doc.NeverUsed) != doc.InstalledSkills {
		t.Errorf("used %d + never %d != %d installed; the two have to share a scope or the "+
			"arithmetic on screen does not close",
			doc.UsedInstalled, len(doc.NeverUsed), doc.InstalledSkills)
	}
	// And the document has to SAY the scopes differ, or a reader reads both as the folder's.
	if !containsFold(doc.Basis, "every project") && !containsFold(doc.Basis, "any project") {
		t.Errorf("Basis = %q; under a project filter it has to say that the never-used figure "+
			"covers every project, not the selected one", doc.Basis)
	}
}

// SELECTING A PROJECT MUST SHOW THAT PROJECT'S USAGE.
//
// THE REPORTED BUG, in one test. `tool_uses` carried no folder, so the used list was built from
// whole sessions that touched the project — and one session works in several folders. Measured on
// the live store: selecting `/private/tmp/cgzip`, a folder with 6 requests, returned that
// session's entire 2,626-request tool list, so every folder reported nearly identical figures.
func TestTheHistoryScopesUsageToTheSelectedProjectNotTheWholeSession(t *testing.T) {
	db := openTestDB(t)
	// ONE SESSION, TWO FOLDERS — the shape that produced the bug.
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported,
			Project: "/Users/v/notes", CacheWrite: 100},
		{TS: daysAgo(9), SessionID: "s1", Model: "m", Source: SourceImported,
			Project: "/Users/v/code", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.insertImportedUses([]importedUse{
		{session: "s1", name: "superpowers:brainstorming", ts: daysAgo(10), isSkill: true,
			project: "/Users/v/notes"},
		{session: "s1", name: "Grep", ts: daysAgo(9), project: "/Users/v/code"},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{Project: "/Users/v/code"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range doc.Used {
		got[u.Name] = true
	}
	if !got["Grep"] {
		t.Error("the selected folder's own invocation is missing")
	}
	if got["superpowers:brainstorming"] {
		t.Error("an invocation from ANOTHER folder in the same session leaked into this " +
			"project's usage. That is the reported defect: every project showed the same " +
			"figures because the query selected whole sessions rather than folders")
	}
}

// THE PICKER OFFERS PROJECTS, NOT EVERY DIRECTORY EVER USED.
//
// THE DEFECT, reported against the running dashboard: 40 entries, of which about six were
// projects. The rest were subdirectories of real repositories and eighteen throwaway
// /private/tmp folders — every distinct cwd Claude Code had been launched in.
//
// Claude Code's own transcript grouping decides; see ProjectGroups. Selecting a project must also
// INCLUDE its subfolders' traffic, or folding the picker would hide the very rows the reader
// selected it to see.
func TestTheHistoryGroupsProjectsAndIncludesTheirSubfolders(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// One project with a subfolder, as Claude Code files it.
	writeJSONL(t, home, "-Users-v-notes", "s-notes",
		turnLine("s-notes", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLine("s-notes", "/Users/v/notes", daysAgo(9), 100, 10),
		turnLineCalling("s-notes", "/Users/v/notes/server", daysAgo(8),
			`{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"x"}}`))
	if _, err := db.ImportHistory(home, ImportOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	// ONE entry, not two: the subfolder is part of the project.
	var names []string
	for _, p := range doc.Projects {
		names = append(names, p.Project)
	}
	if len(names) != 1 || names[0] != "/Users/v/notes" {
		t.Fatalf("Projects = %v; want just [/Users/v/notes]. A picker listing every directory "+
			"ever used offered 40 entries for six projects on a real machine", names)
	}

	// AND SELECTING IT INCLUDES THE SUBFOLDER'S USAGE. Grep ran in notes/server; a reader who
	// selects notes is asking about that work too.
	scoped, err := db.HistoryDocFor(Filter{Project: "/Users/v/notes"}, home)
	if err != nil {
		t.Fatal(err)
	}
	var sawGrep bool
	for _, u := range scoped.Used {
		if u.Name == "Grep" {
			sawGrep = true
		}
	}
	if !sawGrep {
		t.Error("selecting the project hid an invocation made in one of its own subfolders. " +
			"Folding the picker without folding the query means the reader selects a project " +
			"and sees less than they did before")
	}
}

// THE PER-PROJECT VIEW NEEDS THE SPLIT, AND ONLY THE SERVER CAN COMPUTE IT.
//
// "43 skills not used in this project" is not a waste figure: some of those are used in other
// projects, which is correct behaviour rather than waste. The honest presentation splits the
// number — never used anywhere (the waste) versus used elsewhere (not waste) — and the view
// cannot derive that from a project-scoped document, because the other projects' usage is not in
// it. So the document carries both figures.
func TestTheHistorySplitsUnusedHereFromNeverUsedAnywhere(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	mkSkill(t, home, "superpowers", "used-here")
	mkSkill(t, home, "superpowers", "used-elsewhere")
	mkSkill(t, home, "superpowers", "used-nowhere")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(20), SessionID: "s-a", Model: "m", Source: SourceImported,
			Project: "/Users/v/a", CacheWrite: 100},
		{TS: daysAgo(19), SessionID: "s-b", Model: "m", Source: SourceImported,
			Project: "/Users/v/b", CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.insertImportedUses([]importedUse{
		{session: "s-a", name: "superpowers:used-here", ts: daysAgo(20), isSkill: true,
			project: "/Users/v/a"},
		{session: "s-b", name: "superpowers:used-elsewhere", ts: daysAgo(19), isSkill: true,
			project: "/Users/v/b"},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{Project: "/Users/v/a"}, home)
	if err != nil {
		t.Fatal(err)
	}
	// Used in this project: one.
	if doc.UsedHere != 1 {
		t.Errorf("UsedHere = %d; want 1", doc.UsedHere)
	}
	// Not used HERE: two — but only ONE of them is waste.
	if doc.UnusedHere != 2 {
		t.Errorf("UnusedHere = %d; want 2", doc.UnusedHere)
	}
	if doc.UnusedHereButUsedElsewhere != 1 {
		t.Errorf("UnusedHereButUsedElsewhere = %d; want 1 — used-elsewhere is being used "+
			"correctly in another project and is not waste. Without this figure the view reports "+
			"2 unused and a reader reads both as money burning",
			doc.UnusedHereButUsedElsewhere)
	}
	// And the waste figure stays machine-wide: exactly the one used nowhere.
	if len(doc.NeverUsed) != 1 || doc.NeverUsed[0] != "superpowers:used-nowhere" {
		t.Errorf("NeverUsed = %v; want only the skill used in no project at all", doc.NeverUsed)
	}
	// The three have to account for the inventory exactly, or the page shows arithmetic that
	// does not close.
	if doc.UsedHere+doc.UnusedHere != doc.InstalledSkills {
		t.Errorf("used-here %d + unused-here %d = %d, but %d skills are installed",
			doc.UsedHere, doc.UnusedHere, doc.UsedHere+doc.UnusedHere, doc.InstalledSkills)
	}
}

// THE WASTE LIST MUST NOT COUNT THIS TOOL'S OWN SKILLS.
//
// THE BUG, reported as "never used still 40 WTF". The figure was wrong, not merely confusingly
// scoped: nine of those forty were context-guru's own skills -- insights, install, update,
// statusline and the rest -- so the page was billing the user for the reporter's own inventory
// and calling it their waste.
//
// The self-exclusion filtered the USED list only. An unused self-owned skill fell straight
// through into NeverUsed, which is the one list that matters here, and inflated both the headline
// count and the denominator it is read against.
func TestTheWasteListExcludesThisToolsOwnSkills(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	// Two of this tool's own skills, under both names it has shipped under, and two genuine
	// third-party ones. None of them ever invoked.
	mkSkill(t, home, selfPlugin, "insights")
	mkSkill(t, home, "cg", "status")
	mkSkill(t, home, "superpowers", "never-touched")
	mkSkill(t, home, "superpowers", "also-untouched")
	if err := db.insertBatch([]*Event{
		{TS: daysAgo(10), SessionID: "s1", Model: "m", Source: SourceImported, CacheWrite: 100},
	}); err != nil {
		t.Fatal(err)
	}

	doc, err := db.HistoryDocFor(Filter{}, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range doc.NeverUsed {
		if isSelfOwned(n) {
			t.Errorf("%q is in the waste list. These are the reporter's own skills, so counting "+
				"them as the user's waste bills them for this tool's inventory -- and the "+
				"headline count is wrong by however many it has", n)
		}
	}
	if len(doc.NeverUsed) != 2 {
		t.Errorf("NeverUsed has %d entries (%v); want exactly the 2 third-party skills",
			len(doc.NeverUsed), doc.NeverUsed)
	}
	// AND THE DENOMINATOR DROPS WITH IT. "40 of 46" is two wrong numbers if nine of each belong
	// to the reporter: the ratio has to describe the user's own inventory.
	if doc.InstalledSkills != 2 {
		t.Errorf("InstalledSkills = %d; want 2. The count the waste figure is read against has "+
			"to exclude the same skills the waste figure does, or the ratio is nonsense",
			doc.InstalledSkills)
	}
	// Reported, not hidden: they do occupy the prompt.
	if doc.SelfExcluded.Items != 2 {
		t.Errorf("SelfExcluded.Items = %d; want 2 -- excluded from the figures and still "+
			"disclosed, since they are carried on every request like any other skill",
			doc.SelfExcluded.Items)
	}
}
