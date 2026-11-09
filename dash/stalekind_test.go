package dash

import (
	"encoding/json"
	"testing"
)

// seedStale builds sessions whose declaration sets were stored before and after the upgrade of
// #817: an older proxy filed OpenAI tools as server_tool, and the Responses tools under one name,
// "function". Same digest, so the primary key (which has the kind) keeps both rows.
//   - s1 (t1): old rows, then new rows. The old Fetch and the phantom must not be counted.
//   - s2 (t1): old rows only (never re-sent). They stay, as no newer row replaces them.
//   - s3 (t2): old and new rows, another tenant.
func seedStale(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for _, c := range []struct{ tenant, session string }{{"t1", "s1"}, {"t1", "s2"}, {"t2", "s3"}} {
		for i := 0; i < 4; i++ {
			e := mkEvent(int64(1000+i), c.session, "gpt", 100, 90)
			e.TenantID, e.Tools, e.DeclDigest = c.tenant, 3, "d1"
			evs = append(evs, e)
		}
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	old := []Decl{
		{Kind: KindServerTool, Name: "Fetch", Tokens: 100},
		{Kind: KindServerTool, Name: "function", Tokens: 100},
		{Kind: KindServerTool, Name: "web_search", Tokens: 50},
	}
	fresh := []Decl{
		{Kind: KindTool, Name: "Fetch", Tokens: 100},
		{Kind: KindTool, Name: "Other", Tokens: 100},
		{Kind: KindServerTool, Name: "web_search", Tokens: 50},
	}
	write := func(tenant, session string, ts int64, decls []Decl) {
		w := &invWriter{db: db, seen: map[string]*invSession{}} // a fresh writer, as after a restart
		if err := w.write([]invMsg{{tenant: tenant, session: session, ts: ts,
			inv: &Inventory{Digest: "d1", Decls: decls}}}); err != nil {
			t.Fatal(err)
		}
	}
	write("t1", "s1", 1000, old)
	write("t1", "s1", 2000, fresh)
	write("t1", "s2", 1000, old)
	write("t2", "s3", 1000, old)
	write("t2", "s3", 2000, fresh)
	return db
}

// bothPaths runs fn on the per-session queries and on the whole-table pass.
func bothPaths(t *testing.T, fn func(t *testing.T, name string)) {
	old := declScanMinSessions
	defer func() { declScanMinSessions = old }()
	for _, p := range []struct {
		name string
		min  int
	}{{"narrow", 1 << 30}, {"broad", 1}} {
		declScanMinSessions = p.min
		fn(t, p.name)
	}
}

// Rows an older proxy stored as server_tool for OpenAI tools must not be counted next to the rows
// the same tools get now (#817). The newest kind wins; nothing is deleted. On every read path.
func TestToolReportHidesTheStaleServerToolRows(t *testing.T) {
	db := seedStale(t)
	bothPaths(t, func(t *testing.T, path string) {
		rep, err := db.ToolReportFor(Filter{TenantAll: true}, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		// kind:name -> sessions that declare it
		got := map[string]int{}
		for _, s := range append(append([]ToolStat{}, rep.Tools...), rep.Aside...) {
			got[s.Kind+":"+s.Name] = s.SessionsDeclared
		}
		want := map[string]int{
			"tool:Fetch":             2, // s1, s3
			"tool:Other":             2,
			"server_tool:Fetch":      1, // s2 never re-sent its tools: nothing replaces its row
			"server_tool:function":   1, // the same, for the phantom
			"server_tool:web_search": 3, // a genuine server tool stays everywhere
		}
		for k, n := range want {
			if got[k] != n {
				t.Errorf("%s: %s declared in %d sessions, want %d (all: %v)", path, k, got[k], n, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: rows = %v, want %v", path, got, want)
		}
	})
}

// The windows the removal suggestions read must follow the same rule on both paths.
func TestDeclWindowsHideTheStaleServerToolRows(t *testing.T) {
	db := seedStale(t)
	bothPaths(t, func(t *testing.T, path string) {
		w, err := db.declWindows(Filter{TenantAll: true}, &declScanOnce{d: db, f: Filter{TenantAll: true}})
		if err != nil {
			t.Fatal(err)
		}
		if n := w[statKey{KindServerTool, "Fetch"}].sessions; n != 1 {
			t.Errorf("%s: server_tool Fetch seen in %d sessions, want 1 (s2 only)", path, n)
		}
		if n := w[statKey{KindTool, "Fetch"}].sessions; n != 2 {
			t.Errorf("%s: tool Fetch seen in %d sessions, want 2", path, n)
		}
		if n := w[statKey{KindServerTool, "function"}].sessions; n != 1 {
			t.Errorf("%s: the phantom is seen in %d sessions, want 1 (s2 only)", path, n)
		}
	})
}

// The prompt view lists the regions of one digest. A transition session holds both rows in it.
func TestPromptViewHidesTheStaleServerToolRows(t *testing.T) {
	db := seedStale(t)
	bothPaths(t, func(t *testing.T, path string) {
		v, err := db.PromptViewFor(Filter{Tenant: "t1", Session: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, r := range v.Regions {
			got[r.Kind+":"+r.Name]++
		}
		if got["server_tool:Fetch"] != 0 || got["server_tool:function"] != 0 || got["tool:Fetch"] != 1 {
			t.Errorf("%s: regions = %v, want the stale server_tool rows hidden", path, got)
		}
		if got["server_tool:web_search"] != 1 {
			t.Errorf("%s: the genuine server tool is gone: %v", path, got)
		}
		// Coverage counts the same facts: Fetch, Other, web_search.
		if v.Rows != 3 {
			t.Errorf("%s: coverage rows = %d, want 3", path, v.Rows)
		}
	})
}

// The carried-cost read bills what a session carried; a stale row must not be billed next to the
// row that replaced it.
func TestCarriedSetsHideTheStaleServerToolRows(t *testing.T) {
	db := seedStale(t)
	bothPaths(t, func(t *testing.T, path string) {
		scan, err := db.carriedCost(Filter{Tenant: "t1"}, flatPrice,
			func(_, kind, name, _ string) string { return kind + "\x00" + name }, carriedOpts{})
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range scan.Items {
			keys = append(keys, k)
		}
		has := func(k string) bool { _, ok := scan.Items[k]; return ok }
		// s1 carries tool Fetch; s2 carries only the old rows, so server_tool Fetch remains from s2.
		if !has("tool\x00Fetch") || !has("server_tool\x00Fetch") {
			t.Fatalf("%s: carried keys = %q", path, keys)
		}
		if c := scan.Items["server_tool\x00Fetch"]; c.Sessions != 1 {
			t.Errorf("%s: server_tool Fetch carried by %d sessions, want 1 (s2 only)", path, c.Sessions)
		}
		if c := scan.Items["server_tool\x00function"]; c == nil || c.Sessions != 1 {
			t.Errorf("%s: the phantom carried by %+v, want 1 session (s2 only)", path, c)
		}
	})
}

// The whole-table pass must equal the per-session probe on data that holds stale rows: the case
// the golden fixture in perf_equivalence_test.go does not have.
func TestInventoryTablePassEqualsPerSessionProbeOnStaleRows(t *testing.T) {
	db := seedStale(t)
	f := Filter{TenantAll: true}
	read := func() map[string][]byte {
		rep, err := db.ToolReportFor(f, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := db.ToolFilterDocFor(f, flatPrice, ToolFilterState{})
		if err != nil {
			t.Fatal(err)
		}
		pv, err := db.PromptViewFor(f)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]byte{}
		out["report"], _ = json.Marshal(rep)
		out["filter doc"], _ = json.Marshal(doc)
		out["prompt view"], _ = json.Marshal(pv)
		return out
	}
	old := declScanMinSessions
	defer func() { declScanMinSessions = old }()
	declScanMinSessions = 1 << 30
	a := read()
	declScanMinSessions = 1
	b := read()
	for name := range a {
		var x, y any
		_ = json.Unmarshal(a[name], &x)
		_ = json.Unmarshal(b[name], &y)
		if d := jsonDiff("", x, y, nil); len(d) > 0 {
			t.Errorf("%s: table pass differs from per-session probe on stale rows: %v", name, d)
		}
	}
}

// The deferred marker is a note: not a prompt row, not a listing size.
func TestDeferredMarkerIsNotCounted(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(1000, "s1", "gpt", 100, 90)
	e.TenantID, e.Tools, e.DeclDigest = "t1", 1, "d1"
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "t1", session: "s1", ts: 1000, inv: &Inventory{Digest: "d1",
		Decls: []Decl{{Kind: KindTool, Name: "Bash", Tokens: 10},
			{Kind: KindDeferredListing, Server: SkillsUnknown}}}}}); err != nil {
		t.Fatal(err)
	}
	bothPaths(t, func(t *testing.T, path string) {
		rep, err := db.ToolReportFor(Filter{Tenant: "t1"}, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Prompt.Rows != 1 {
			t.Errorf("%s: report prompt rows = %d, want 1 (Bash)", path, rep.Prompt.Rows)
		}
		v, err := db.PromptViewFor(Filter{Tenant: "t1"})
		if err != nil {
			t.Fatal(err)
		}
		if v.Rows != 1 {
			t.Errorf("%s: prompt view coverage = %d, want 1", path, v.Rows)
		}
	})
}

// A listing that was not read has no size, so it must not pull the average of the real ones down.
func TestUnknownListingDoesNotDiluteTheListingAverage(t *testing.T) {
	sessions := map[string]*sessionCost{"a": {}, "b": {}}
	decls := []declRow{
		{session: "a", kind: KindSkillListing, server: SkillsOK, tokens: 1000},
		{session: "b", kind: KindSkillListing, server: SkillsUnknown, tokens: 0},
	}
	rep := buildToolReport(sessions, decls, nil, false)
	if rep.Skills.ListingTokens != 1000 {
		t.Errorf("listing tokens = %d, want 1000", rep.Skills.ListingTokens)
	}
	if rep.Skills.UnknownSessions != 1 {
		t.Errorf("unknown sessions = %d, want 1", rep.Skills.UnknownSessions)
	}
}

// A session stored before the Responses skills listing was read holds an `unknown` marker. Stored
// again after the change, it holds the real skill rows too, under the same digest. The newest fact
// wins: the marker is stale (#817). A session with no newer skill keeps its marker.
func seedStaleMarker(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	for _, s := range []string{"s1", "s2"} {
		for i := 0; i < 4; i++ {
			e := mkEvent(int64(1000+i), s, "gpt", 100, 90)
			e.TenantID, e.Tools, e.DeclDigest = "t1", 1, "d1"
			evs = append(evs, e)
		}
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	write := func(session string, ts int64, decls []Decl) {
		w := &invWriter{db: db, seen: map[string]*invSession{}}
		if err := w.write([]invMsg{{tenant: "t1", session: session, ts: ts,
			inv: &Inventory{Digest: "d1", Decls: decls}}}); err != nil {
			t.Fatal(err)
		}
	}
	unknown := Decl{Kind: KindSkillListing, Server: SkillsUnknown, Tokens: 0}
	write("s1", 1000, []Decl{{Kind: KindTool, Name: "Bash", Tokens: 10}, unknown})
	write("s1", 2000, []Decl{{Kind: KindTool, Name: "Bash", Tokens: 10},
		{Kind: KindSkillListing, Server: SkillsOK, Tokens: 200}, {Kind: KindSkill, Name: "dataviz", Tokens: 100}})
	write("s2", 1000, []Decl{{Kind: KindTool, Name: "Bash", Tokens: 10}, unknown}) // never re-sent
	return db
}

func TestStaleUnknownSkillsMarkerIsHidden(t *testing.T) {
	db := seedStaleMarker(t)
	bothPaths(t, func(t *testing.T, path string) {
		rep, err := db.ToolReportFor(Filter{Tenant: "t1"}, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		// s1 read the listing; s2 still holds its marker. So exactly one session is unknown.
		if rep.Skills.UnknownSessions != 1 {
			t.Errorf("%s: unknown sessions = %d, want 1 (s2 only)", path, rep.Skills.UnknownSessions)
		}
		found := false
		for _, s := range rep.Skills.Skills {
			found = found || s.Name == "dataviz"
		}
		if !found {
			t.Errorf("%s: dataviz is missing from the skills", path)
		}
		carried, err := db.carriedCost(Filter{Tenant: "t1"}, flatPrice,
			func(_, kind, name, server string) string { return kind + "\x00" + name + "\x00" + server }, carriedOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if c := carried.Items[KindSkillListing+"\x00\x00"+SkillsUnknown]; c == nil || c.Sessions != 1 {
			t.Errorf("%s: the unknown marker is carried by %+v, want 1 session (s2 only)", path, c)
		}
	})
}

// A scope of only transition sessions: the session was stored with an `unknown` marker, then again
// with real skills (same digest, so the OK marker may be lost). The state must be "ok" with the
// skills, not "absent" (#817). A scope with no listing stays "absent"; an unread listing stays
// "unknown".
func TestSkillsStateOfTransitionSessions(t *testing.T) {
	db := seedStaleMarker(t)
	// A third session: no listing at all.
	e := mkEvent(1000, "s3", "gpt", 100, 90)
	e.TenantID, e.Tools, e.DeclDigest = "t1", 1, "d3"
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{{tenant: "t1", session: "s3", ts: 1000,
		inv: &Inventory{Digest: "d3", Decls: []Decl{{Kind: KindTool, Name: "Bash", Tokens: 10}}}}}); err != nil {
		t.Fatal(err)
	}
	bothPaths(t, func(t *testing.T, path string) {
		for _, c := range []struct {
			session, state string
			skills         int
		}{
			{"s1", SkillsOK, 1},      // transition: marker + real skill
			{"s2", SkillsUnknown, 0}, // never re-sent: still an unread listing
			{"s3", "absent", 0},      // no listing at all
		} {
			rep, err := db.ToolReportFor(Filter{Tenant: "t1", Session: c.session}, flatPrice)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Skills.State != c.state || len(rep.Skills.Skills) != c.skills {
				t.Errorf("%s: %s state = %q with %d skills, want %q with %d",
					path, c.session, rep.Skills.State, len(rep.Skills.Skills), c.state, c.skills)
			}
		}
	})
}
