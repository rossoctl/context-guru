package dash

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// One session with a main set and a subagent set that carries none of the MCP servers, two
// models, every cache tier, an unpriced and an unmapped request, used and unused servers,
// tools and skills, and the plugin's own declarations.
func seedAdapt(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	type rq struct {
		model      string
		tools      int
		read, wr   int64
		accounting string
	}
	var evs []*Event
	for i, r := range []rq{
		{"opus", 6, 0, 5000, AccountingComplete},   // rewrote the prefix with a 1h write: 2x input
		{"haiku", 6, 5000, 10, AccountingComplete}, // read it, at haiku's rate
		{"opus", 1, 900, 0, AccountingComplete},    // subagent set: carries nothing listed
		{"opus", 6, 0, 0, AccountingComplete},      // uncached: input rate
		{"opus", 6, 5000, 0, "partial"},            // mapped, not priced
		{"opus", 9, 5000, 0, AccountingComplete},   // no set with 9 tools: unmapped
		{"mystery", 6, 5000, 0, AccountingComplete},
	} {
		e := mkEvent(day(0)+int64(i), "s1", r.model, 100, 100)
		e.TenantID, e.Tools, e.CacheRead, e.CacheWrite, e.TokenAccounting = "t1", r.tools, r.read, r.wr, r.accounting
		if i == 0 {
			e.CacheWrite1h = r.wr / 5 // any 1h write covers the head: still 2x, not a blend
		}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	mcp := func(server, tool string, tok int) Decl {
		return Decl{Kind: KindMCPTool, Name: "mcp__" + server + "__" + tool, Server: server, Tokens: tok}
	}
	main := []Decl{
		{Kind: KindTool, Name: "Read", Tokens: 100},
		mcp("srv", "a", 1000), mcp("srv", "b", 500),
		mcp("used", "x", 200), mcp("used", "y", 300),
		mcp("plugin_context-guru_cg", "t", 999),
		{Kind: KindSkill, Name: "p:sk", Tokens: 50},
		{Kind: KindSkill, Name: "used-sk", Tokens: 70},
		{Kind: KindSkill, Name: "context-guru:waste", Tokens: 40},
	}
	used := []Used{{Name: "mcp__used__x", Server: "used", Calls: 2}, {Name: "Skill", Skill: "used-sk", Calls: 1}}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{
		{tenant: "t1", session: "s1", ts: day(0), inv: &Inventory{Digest: "main", Decls: main, Used: used, UseFingerprint: 1}},
		{tenant: "t1", session: "s1", ts: day(0), inv: &Inventory{Digest: "sub", Decls: main[:1], UseFingerprint: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func adaptPrice(m string) (modelinfo.Price, bool) {
	switch m {
	case "opus":
		return modelinfo.Price{Input: 1, CacheRead: 0.1, CacheWrite: 1.25}, true
	case "haiku":
		return modelinfo.Price{Input: 0.1, CacheRead: 0.01, CacheWrite: 0.125}, true
	}
	return modelinfo.Price{}, false
}

func TestAdaptToolsPricesEachRequestAtTheSetItCarried(t *testing.T) {
	doc, err := seedAdapt(t).AdaptTools(Filter{Tenant: "t1"}, adaptPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}
	if c := doc.Coverage; c != (AdaptToolsCoverage{Sessions: 1, SessionsCaptured: 1, Requests: 7, RequestsMapped: 6, RequestsUnpriced: 1}) {
		t.Fatalf("coverage %+v", c)
	}
	// Carried on: r1 write at opus, r2 read at haiku, r4 input at opus (+ r7, unknown model).
	// Not on r3: the subagent set has no MCP server, and pricing it there was the old error.
	per := 2.0 + 0.01 + 1.0
	want := map[string]struct {
		kind string
		usd  float64
	}{
		"srv":          {KindMCPServer, 1500 * per},
		"mcp__used__y": {KindMCPTool, 300 * per},
		"p:sk":         {KindSkill, 50 * per},
	}
	if len(doc.Items) != len(want) {
		t.Fatalf("items %+v", doc.Items)
	}
	total := 0.0
	for _, it := range doc.Items {
		w, ok := want[it.Name]
		if !ok || it.Kind != w.kind || math.Abs(it.USD-w.usd) > 1e-9 {
			t.Fatalf("item %+v, want %+v", it, w)
		}
		if it.Requests != 4 || it.Priced || it.Sessions != 1 || it.Sufficient {
			t.Fatalf("item %+v: want 4 requests, unpriced (mystery model), 1 session, insufficient", it)
		}
		total += w.usd
	}
	if math.Abs(doc.TotalUSD-total) > 1e-9 {
		t.Fatalf("total %v, want %v", doc.TotalUSD, total)
	}
	// The one-off rewrite: the latest priced request (r4, 10 fresh tokens) at opus write minus read.
	if math.Abs(doc.RewriteUSD-10*(1.25-0.1)) > 1e-9 || !strings.Contains(doc.Items[0].Basis, "unused in the observed window") {
		t.Fatalf("rewrite %v, basis %q", doc.RewriteUSD, doc.Items[0].Basis)
	}
	srv := doc.Items[0]
	if srv.Name != "srv" || srv.Tools != 2 || srv.Tokens != 1500 || srv.RemoveAs != "mcp__srv" ||
		srv.Suggest != "claude mcp remove srv" || math.Abs(srv.USDCacheWrite-1500*2.0) > 1e-9 {
		t.Fatalf("server item %+v", srv)
	}
	if sk := doc.Items[2]; sk.RemoveAs != "skill__p:sk" || !strings.HasPrefix(sk.Suggest, "claude plugin disable p ") {
		t.Fatalf("skill item %+v", sk)
	}
}

func TestAdaptToolsSkipsWhatIsAlreadyRemoved(t *testing.T) {
	doc, err := seedAdapt(t).AdaptTools(Filter{Tenant: "t1"}, adaptPrice,
		ToolFilterState{Enabled: true, Removed: []string{"mcp__srv", "skill__p:sk"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 1 || doc.Items[0].Name != "mcp__used__y" || !doc.Filter.Enabled || len(doc.Filter.Removed) != 2 {
		t.Fatalf("doc %+v", doc)
	}
}

func TestSuggestServer(t *testing.T) {
	for in, want := range map[string]string{
		"context7":                 "claude mcp remove context7",
		"plugin_context7_context7": "claude plugin disable context7 (the plugin that provides this server)",
		"claude_ai_Gmail":          "disconnect the Gmail connector in claude.ai settings",
	} {
		if got := suggestServer(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// Two sets with the same tool count: a request that recorded its digest is priced on that set
// alone; one that did not gets the latest-before guess, bounded by both candidates.
func TestAdaptToolsUsesTheRecordedDigestAndBoundsTheGuess(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	for i, dg := range []string{"", "a"} {
		e := mkEvent(day(1)+int64(i), "s2", "opus", 100, 100)
		e.TenantID, e.Tools, e.CacheRead, e.CacheWrite, e.DeclDigest = "t1", 2, 1000, 0, dg
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	read := Decl{Kind: KindTool, Name: "Read", Tokens: 10}
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	if err := w.write([]invMsg{
		{tenant: "t1", session: "s2", ts: day(0), inv: &Inventory{Digest: "a", UseFingerprint: 1,
			Decls: []Decl{read, {Kind: KindMCPTool, Name: "mcp__srv2__q", Server: "srv2", Tokens: 100}}}},
		{tenant: "t1", session: "s2", ts: day(0) + 1, inv: &Inventory{Digest: "b", UseFingerprint: 1,
			Decls: []Decl{read, {Kind: KindTool, Name: "Write", Tokens: 10}}}},
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := db.AdaptTools(Filter{Tenant: "t1"}, adaptPrice, ToolFilterState{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Coverage.RequestsExact != 1 || doc.Coverage.RequestsMapped != 2 || len(doc.Items) != 1 {
		t.Fatalf("doc %+v", doc)
	}
	// Exact request: 100 x 0.1 everywhere. Guessed request: set b (latest), so 0, bounded [0, 10].
	if it := doc.Items[0]; math.Abs(it.USD-10) > 1e-9 || math.Abs(it.USDLow-10) > 1e-9 ||
		math.Abs(it.USDHigh-20) > 1e-9 || it.Requests != 1 {
		t.Fatalf("item %+v", it)
	}
}

// The envelope the shared adapt renderer reads: only items with enough evidence are recommended,
// and every option names exactly what it would filter.
func TestAdaptToolsEnvelope(t *testing.T) {
	doc := &AdaptToolsDoc{RewriteUSD: 1, Window: AdaptToolsWindow{Since: day(0), Until: day(30)}, Items: []AdaptToolsItem{
		{Name: "a", RemoveAs: "mcp__a", USD: 3, Sufficient: true},
		{Name: "b", RemoveAs: "skill__b", USD: 2},
	}}
	doc.finish(ToolFilterState{Removed: []string{"mcp__old", "Workflow"}})
	if doc.Best != "remove-sufficient" || doc.Current != "keep" || doc.Recommendation == nil ||
		doc.Recommendation.PerMonthUSD != 3 || doc.Recommendation.NetDeltaUSD != 2 ||
		doc.Options[1].SpendUSD != 1 || doc.Options[1].NetUSD != 2 || doc.Items[0].LastUsedMS != nil {
		t.Fatalf("doc %+v rec %+v", doc, doc.Recommendation)
	}
	remove := map[string]string{}
	for _, o := range doc.Options {
		remove[o.ID] = strings.Join(o.Remove, ",")
	}
	if want := map[string]string{"keep": "", "remove-sufficient": "mcp__a", "remove-unused": "mcp__a,skill__b",
		"restore": "mcp__old"}; fmt.Sprint(remove) != fmt.Sprint(want) {
		t.Errorf("options remove %v, want %v", remove, want)
	}
	dear := &AdaptToolsDoc{RewriteUSD: 5, Items: []AdaptToolsItem{{Name: "a", RemoveAs: "mcp__a", USD: 3, Sufficient: true}}}
	dear.finish(ToolFilterState{})
	if dear.Best != "keep" || dear.Recommendation != nil {
		t.Fatalf("recommended a removal that the rewrite outweighs: %+v", dear)
	}
	none := &AdaptToolsDoc{Items: []AdaptToolsItem{{Name: "b", RemoveAs: "skill__b", USD: 2}}}
	none.finish(ToolFilterState{})
	if none.Best != "keep" || none.Recommendation != nil {
		t.Fatalf("an item without enough evidence was recommended: %+v", none)
	}
}
