package dash

// A deferred MCP tool is a line of text in a message, not an element of `tools`, and the proxy
// filter removes only elements. So every place that offers the filter must not offer it for a
// deferred tool: a "filtered" item that saves nothing, then leaves the report, is wrong advice.
// The user's own change (claude mcp remove, permissions.deny) still works, and stays offered.

import (
	"strings"
	"testing"
)

// seedDeferred writes one session that carried server "dfr" as deferred rows and server "arr"
// as plain tools-array rows, neither used, over enough sessions and days to be sufficient.
func seedDeferred(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	var evs []*Event
	var msgs []invMsg
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	for i := 0; i < suggestMinSessions; i++ {
		s := "s" + string(rune('a'+i))
		ts := day(2 * i)
		e := mkEvent(ts, s, "opus", 100, 100)
		e.TenantID, e.Tools, e.DeclDigest, e.CacheRead = "t1", 2, "d"+s, 5000
		evs = append(evs, e)
		msgs = append(msgs, invMsg{tenant: "t1", session: s, ts: ts, inv: &Inventory{Digest: "d" + s, Decls: []Decl{
			{Kind: KindMCPTool, Name: "mcp__dfr__a", Server: "dfr", Tokens: 10, Deferred: true},
			{Kind: KindMCPTool, Name: "mcp__arr__b", Server: "arr", Tokens: 400},
		}}})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, invMsg{tenant: "t2", session: "other", ts: day(0), inv: &Inventory{Digest: "o", Decls: []Decl{
		{Kind: KindMCPTool, Name: "mcp__dfr__a", Server: "dfr", Tokens: 10},
	}}})
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestWriterStoresTheDeferredFlag(t *testing.T) {
	db := seedDeferred(t)

	got := map[string]int{}
	rows, err := db.sql.Query(`SELECT name, MAX(deferred) FROM tool_declarations GROUP BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var d int
		if err := rows.Scan(&n, &d); err != nil {
			t.Fatal(err)
		}
		got[n] = d
	}
	if got["mcp__dfr__a"] != 1 || got["mcp__arr__b"] != 0 {
		t.Errorf("deferred column = %v, want dfr 1 and arr 0", got)
	}
}

// The adapt report lists the deferred server with its client command, and no option writes it
// to the filter.
func TestAdaptToolsNeverOffersTheFilterForADeferredServer(t *testing.T) {
	doc, err := seedDeferred(t).AdaptTools(Filter{Tenant: "t1", TenantAll: false}, adaptPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}

	var dfr *AdaptToolsItem
	for i := range doc.Items {
		if doc.Items[i].Name == "dfr" {
			dfr = &doc.Items[i]
		}
	}
	if dfr == nil {
		t.Fatalf("the deferred server is missing from the report: %+v", doc.Items)
	}
	if dfr.RemoveAs != "" || !dfr.ClientOnly || dfr.Suggest != "claude mcp remove dfr" {
		t.Errorf("deferred item = %+v, want no remove_as, client_only, and the client command", *dfr)
	}
	for _, o := range doc.Options {
		for _, n := range o.Remove {
			if strings.Contains(n, "dfr") {
				t.Errorf("option %s writes %q to the filter", o.ID, n)
			}
		}
	}
	// The tools-array server is still offered as before.
	offered := false
	for _, o := range doc.Options {
		offered = offered || strings.Contains(strings.Join(o.Remove, ","), "mcp__arr")
	}
	if !offered {
		t.Errorf("the tools-array server lost its filter option: %+v", doc.Options)
	}
}

// The filter suggestions keep a deferred tool, because they are also the evidence behind the
// insights `remove` verdict, but name nothing for the filter to write.
func TestFilterSuggestionForADeferredToolNamesNoFilterEntry(t *testing.T) {
	doc, err := seedDeferred(t).ToolFilterDocFor(Filter{Tenant: "t1"}, adaptPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}

	by := map[string]Suggestion{}
	for _, s := range doc.Suggestions {
		by[s.Name] = s
	}
	d, ok := by["mcp__dfr__a"]
	if !ok {
		t.Fatalf("the deferred tool is not suggested at all: %+v", doc.Suggestions)
	}
	if d.RemoveAs != "" || !d.ClientOnly {
		t.Errorf("deferred suggestion = %+v, want no remove_as and client_only", d)
	}
	if a := by["mcp__arr__b"]; a.RemoveAs != "mcp__arr__b" || a.ClientOnly {
		t.Errorf("tools-array suggestion = %+v, want unchanged", a)
	}
}

// /api/tools rows carry the flag, so the Inventory tab can disable its filter switch.
func TestToolReportMarksDeferredRows(t *testing.T) {
	rep, err := seedDeferred(t).ToolReportFor(Filter{Tenant: "t1"}, adaptPrice)
	if err != nil {
		t.Fatal(err)
	}

	by := map[string]ToolStat{}
	for _, st := range rep.Tools {
		by[st.Name] = st
	}
	if !by["mcp__dfr__a"].Deferred || by["mcp__arr__b"].Deferred {
		t.Errorf("deferred flags: dfr=%v arr=%v, want true and false",
			by["mcp__dfr__a"].Deferred, by["mcp__arr__b"].Deferred)
	}
}

// The proxy refuses a filter write for a deferred tool or server, so a hand-made POST cannot
// do what the page no longer offers. The lookup is per tenant and needs no window.
func TestIsDeferredDecl(t *testing.T) {
	db := seedDeferred(t)

	for _, c := range []struct {
		tenant, name string
		want         bool
	}{
		{"t1", "mcp__dfr__a", true},
		{"t1", "mcp__dfr", true}, // the whole-server form
		{"t1", "mcp__arr__b", false},
		{"t1", "mcp__arr", false},
		{"t2", "mcp__dfr__a", false}, // another tenant's rows say nothing about this one
		{"t1", "Read", false},
	} {
		got, err := db.IsDeferredDecl(c.tenant, c.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("IsDeferredDecl(%s, %s) = %v, want %v", c.tenant, c.name, got, c.want)
		}
	}
}

// Every unused MCP tool on Claude Code 2.1.310 is deferred. Then no item is filterable, and the
// summary must not say that everything was used while it lists the unused items.
func TestAdaptToolsSummaryWhenOnlyDeferredItemsAreUnused(t *testing.T) {
	doc := &AdaptToolsDoc{Window: AdaptToolsWindow{Since: day(0), Until: day(30)}, Items: []AdaptToolsItem{
		{Kind: KindMCPServer, Name: "playwright", ClientOnly: true, USD: 4, Sufficient: true,
			Suggest: "claude mcp remove playwright"},
		{Kind: KindMCPServer, Name: "memory", ClientOnly: true, USD: 1, Suggest: "claude mcp remove memory"},
	}}

	doc.finish(ToolFilterState{})

	if strings.Contains(doc.Summary, "was used") {
		t.Errorf("summary says everything was used while 2 items are unused: %q", doc.Summary)
	}
	for _, want := range []string{"2 declared items", "deferred", "command on each item"} {
		if !strings.Contains(doc.Summary, want) {
			t.Errorf("summary %q does not say %q", doc.Summary, want)
		}
	}
	if doc.Best != "keep" || doc.Recommendation != nil || len(doc.Options) != 1 {
		t.Errorf("a client-only report recommends a filter: best=%s rec=%+v options=%+v", doc.Best, doc.Recommendation, doc.Options)
	}
}
