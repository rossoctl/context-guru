package dash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInsightsEndpointServesTheShapedDocument pins the route, not the shaping — the shaping has
// its own tests. What this covers is the wiring that makes it reachable, which is where a feature
// silently fails to exist: a handler that works and a route nobody mounted look identical from the
// outside, and the scoping test only walks the route TABLE.
func TestInsightsEndpointServesTheShapedDocument(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	db := rec.DB()
	seedSessionWithDecl(t, db, "s1", 1_000, 4, "DesignSync", 2057)

	_, body := get(t, a, "/api/insights", "")
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("no items array in the response; keys present: %v", body)
	}
	if len(items) == 0 {
		t.Fatal("the endpoint answered with an empty inventory for a store that has one")
	}
	row, _ := items[0].(map[string]any)
	for _, field := range []string{"name", "tokens", "scope", "verdict", "why"} {
		if _, present := row[field]; !present {
			t.Errorf("item row has no %q; the page cannot render a decision without it", field)
		}
	}
	// The totals and the basis, because an unlabelled dollar figure is the one thing every
	// number in this report is careful not to be.
	for _, field := range []string{"removable_usd", "removable_tokens", "basis"} {
		if _, present := body[field]; !present {
			t.Errorf("the document has no %q", field)
		}
	}
	if b, _ := body["basis"].(string); b != "measured over the window" {
		t.Errorf("basis = %q, want \"measured over the window\"", b)
	}
}

// TestInsightsEndpointIsInTheRouteTable is the one assertion that cannot be made from a response.
// Mount walks a.routes(); a handler registered anywhere else is a route whose scope class nothing
// checks, and the tenant-scoping test would never see it. That is how a tenant-scoped surface
// quietly becomes a public one.
func TestInsightsEndpointIsInTheRouteTable(t *testing.T) {
	a, _ := newTestAPI(t, Options{})
	var found *route
	for _, rt := range a.routes() {
		if rt.pattern == "GET /api/insights" {
			r := rt
			found = &r
		}
	}
	if found == nil {
		t.Fatal("GET /api/insights is not in a.routes(), so its scope class is unchecked by the " +
			"scoping tests even if it serves correctly")
	}
	if found.class != scopeTenant {
		t.Errorf("class = %v, want scopeTenant: the rows are the caller's own configuration, and "+
			"a manager reaches another tenant's view through ?tenant= like every other route",
			found.class)
	}
}

// TestInsightsEndpointHonoursTheSessionWindow proves the window reaches the shaping. A windowing
// parameter that is accepted and ignored is the worst failure here: the caller gets a
// correct-looking report of the wrong window and nothing says so.
func TestInsightsEndpointHonoursTheSessionWindow(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	db := rec.DB()
	seedSessionWithDecl(t, db, "oldest", 1_000, 2, "OldTool", 100)
	seedSessionWithDecl(t, db, "newest", 500_000, 3, "NewTool", 900)

	names := func(b map[string]any) map[string]bool {
		out := map[string]bool{}
		list, _ := b["items"].([]any)
		for _, r := range list {
			if m, ok := r.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					out[n] = true
				}
			}
		}
		return out
	}
	_, all := get(t, a, "/api/insights", "")
	if n := names(all); !n["OldTool"] || !n["NewTool"] {
		t.Fatalf("unwindowed document is missing a declaration: %v", n)
	}
	_, one := get(t, a, "/api/insights?sessions=1", "")
	n := names(one)
	if !n["NewTool"] {
		t.Error("sessions=1 dropped the most recent session's declaration")
	}
	if n["OldTool"] {
		t.Error("sessions=1 still carries the older session's declaration, so the parameter was " +
			"ignored and the caller got all history under a narrower label")
	}
}

// TestADescriptionIsReadFromDiskOrReportedAbsent covers the one capability a browser cannot have,
// and the reason the shaping lives in the proxy rather than in page JavaScript.
//
// It also pins the limit. These files are on the machine running the proxy, which is the user's own
// machine for a local install and someone else's for a hosted one. Where there is nothing to read
// the item must say so rather than be given a description this code invented — a fabricated one
// would be indistinguishable from a real one, and the whole surface's claim is that every line
// traces to a measurement or a file.
func TestADescriptionIsReadFromDiskOrReportedAbsent(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "superpowers", "6.4.1",
		"skills", "brainstorming")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: brainstorming\ndescription: Explore intent before implementation.\n---\n\n# x\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := skillDescriptions(home)
	if d := got["superpowers:brainstorming"]; d != "Explore intent before implementation." {
		t.Errorf("description = %q, want the one in its SKILL.md keyed as the inventory names "+
			"it (<plugin>:<skill>)", d)
	}
	if _, present := got["dataviz"]; present {
		t.Error("a description appeared for a skill with no file on disk; absent must stay absent")
	}
	// A missing plugin cache is the normal state of a machine with no plugins, not an error worth
	// failing a cost report over.
	if m := skillDescriptions(filepath.Join(home, "nope")); len(m) != 0 {
		t.Errorf("a missing cache returned %d description(s), want none and no error", len(m))
	}
	// A folded description is normal YAML and must not be truncated at its first newline, nor
	// split across lines in a way a reader sees as two facts.
	folded := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "p2", "1", "skills", "f")
	if err := os.MkdirAll(folded, 0o755); err != nil {
		t.Fatal(err)
	}
	fb := "---\nname: f\ndescription: >\n  First part here.\n  Second part continues.\n---\n"
	if err := os.WriteFile(filepath.Join(folded, "SKILL.md"), []byte(fb), 0o644); err != nil {
		t.Fatal(err)
	}
	d := skillDescriptions(home)["p2:f"]
	for _, want := range []string{"First part here.", "Second part continues."} {
		if !strings.Contains(d, want) {
			t.Errorf("folded description %q is missing %q", d, want)
		}
	}
	if strings.Contains(d, "\n") {
		t.Errorf("description %q contains a newline; flattened is the only safe shape for a "+
			"value that reaches a line-oriented reader", d)
	}
}

// TestTheEndpointActuallyAttachesDescriptions closes a gap the sibling test left open: it exercised
// skillDescriptions directly, so replacing the handler's `Descriptions:` argument with nil still
// passed. A reader can only trust a function the handler is proven to CALL.
//
// Keyed off $HOME, which os.UserHomeDir honours, so the handler reads a sandbox rather than the
// developer's own plugin cache — the same trap the plugin's own tests document about HOME.
func TestTheEndpointActuallyAttachesDescriptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "plugins", "cache", "mkt", "superpowers", "6.4.1",
		"skills", "brainstorming")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: brainstorming\ndescription: Explore intent before implementation.\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a, rec := newTestAPI(t, Options{})
	db := rec.DB()
	// A plugin skill by the name the inventory uses, so the description can be joined to it.
	seedSkillDecl(t, db, "s1", 1_000, 3, "superpowers:brainstorming", 332)

	_, resp := get(t, a, "/api/insights", "")
	items, _ := resp["items"].([]any)
	var found bool
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		if n, _ := m["name"].(string); n != "superpowers:brainstorming" {
			continue
		}
		found = true
		if d, _ := m["desc"].(string); d != "Explore intent before implementation." {
			t.Errorf("desc = %q, want the description from its SKILL.md. The endpoint is not "+
				"joining what it reads from disk onto the item", d)
		}
		if known, _ := m["desc_known"].(bool); !known {
			t.Error("desc_known is false for an item whose description was read")
		}
	}
	if !found {
		t.Fatalf("superpowers:brainstorming is absent from the document: %v", items)
	}
}

// seedSkillDecl inserts a SKILL declaration, which the tool declaration helper cannot do: the kind
// is what decides whether a row reaches the skills block or the tools list.
func seedSkillDecl(t *testing.T, db *DB, session string, ts int64, requests int,
	name string, tokens int) {
	t.Helper()
	for i := 0; i < requests; i++ {
		insertReq(t, db, &Event{
			TS: ts + int64(i), SessionID: session, Model: "m1", TenantID: "t1",
			TokensBefore: 100, TokensAfter: 100, Meta: Meta{Tools: 1},
			CacheRead: 100, TokenAccounting: AccountingComplete,
		})
	}
	if _, err := db.sql.Exec(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts)
		VALUES('t1',?,'d1',?,?,'',?,?)`, session, KindSkill, name, tokens, ts); err != nil {
		t.Fatal(err)
	}
}
