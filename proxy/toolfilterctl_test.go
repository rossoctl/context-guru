package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// The write half of declaration removal has to inherit three things from the control plane
// rather than reimplement them, and each of them is a way the switch could go wrong:
//
//	validation        — the stored document must still build, or the account's next turn
//	                    fails on a configuration a settings page accepted;
//	the audit trail   — a change to what we send must be attributable.
//
// It must NOT inherit the fourth: PUT /api/me's manager gate. One declaration off a user's own
// prompt is that user's own bill, so any signed-in account may do it — with the single
// exception of a built-in, which is not a saving but a broken agent.
//
// It also has to be a real round trip: excluding then re-including must leave a document that
// runs the same pipeline as before, because that is the recovery path.
func TestToolFilterExcludeRoundTrip(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	// Exclude one declaration.
	w, out := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("exclude = %d %s", w.Code, w.Body)
	}
	if !hasExcluded(out, "Workflow") {
		t.Fatalf("the answer does not list the exclusion: %v", out)
	}
	// The stored document must carry it, must build, and must run the component.
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	doc, _ := tn["effective_config_yaml"].(string)
	if !strings.Contains(doc, "Workflow") || !strings.Contains(doc, "toolfilter") {
		t.Fatalf("stored configuration does not carry the removal: %q", doc)
	}
	names, ok, reason := removedFrom(doc)
	if !ok || len(names) != 1 || names[0] != "Workflow" {
		t.Fatalf("removedFrom(%q) = %v ok=%v %q", doc, names, ok, reason)
	}

	// A whole MCP server is its own unit and is stored as the bare prefix form.
	if w, _ = f.do(t, "POST", "/api/toolfilter",
		`{"kind":"mcp_server","name":"","server":"playwright","action":"exclude"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("exclude server = %d %s", w.Code, w.Body)
	}
	_, me = f.do(t, "GET", "/api/me", "", jar)
	tn, _ = me["tenant"].(map[string]any)
	if doc, _ = tn["effective_config_yaml"].(string); !strings.Contains(doc, "mcp__playwright") {
		t.Fatalf("server exclusion not stored: %q", doc)
	}

	// RECOVERY: re-including both empties the LIST. The component stays in the pipeline, because
	// it now ships in the default one with an empty list — taking it out would write an explicit
	// pipeline diverging from the default to no effect, since a filter with nothing to remove
	// already removes nothing. What must not survive is the NAME.
	for _, name := range []string{`{"kind":"tool","name":"Workflow","action":"include"}`,
		`{"kind":"mcp_server","server":"playwright","action":"include"}`} {
		if w, _ = f.do(t, "POST", "/api/toolfilter", name, jar); w.Code != http.StatusOK {
			t.Fatalf("include = %d %s", w.Code, w.Body)
		}
	}
	_, me = f.do(t, "GET", "/api/me", "", jar)
	tn, _ = me["tenant"].(map[string]any)
	doc, _ = tn["effective_config_yaml"].(string)
	if strings.Contains(doc, "Workflow") || strings.Contains(doc, "mcp__playwright") {
		t.Errorf("re-including left a name in the removal list: %q", doc)
	}
	// And the component is STILL there, which is the half a revert to the old
	// `withComponent(..., len(names) > 0)` would break silently: the list would be right, the
	// account's pipeline would explicitly omit a component the default ships, and nothing would
	// notice until the default changed under it.
	if !strings.Contains(doc, "toolfilter") {
		t.Errorf("re-including took toolfilter out of the pipeline; it ships in the default one "+
			"and an empty list already removes nothing: %q", doc)
	}

	// The audit trail recorded the configuration changes.
	_, audit := f.do(t, "GET", "/api/me/audit", "", jar)
	entries, _ := audit["audit"].([]any)
	changes := 0
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["field"] == "config_yaml" {
			changes++
		}
	}
	if changes < 4 {
		t.Errorf("audit recorded %d configuration changes of 4; a change to what we send must "+
			"be attributable: %v", changes, entries)
	}
}

// TestToolFilterIsTheUsersOwnDecision: a plain account may stop carrying its own MCP tools and
// MCP servers. This is the whole point of the inventory page — it is shown to every account, so
// a route that answered 403 made the switch a lie.
//
// The audit row matters as much as the 200: the write goes through reg.Update as the USER, so
// "who changed what runs on my traffic" has to name the user and not a manager who never
// touched it.
func TestToolFilterIsTheUsersOwnDecision(t *testing.T) {
	f := ctlFixture(t)
	f.signUp(t, "boss@ibm.com", "l") // the bootstrap account is the manager
	w, _ := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	userID, _ := tn["id"].(string)
	if userID == "" {
		t.Fatalf("no tenant id for the plain account: %v", me)
	}
	if mgr, _ := tn["role"].(string); mgr == "manager" {
		t.Fatalf("the second account is a manager, so this test proves nothing: %v", tn)
	}

	for _, body := range []string{
		`{"kind":"mcp_tool","name":"mcp__playwright__click","server":"playwright","action":"exclude"}`,
		`{"kind":"mcp_server","server":"playwright","action":"exclude"}`,
	} {
		if w, _ := f.do(t, "POST", "/api/toolfilter", body, jar); w.Code != http.StatusOK {
			t.Fatalf("a plain account could not filter its own declaration: %s = %d %s",
				body, w.Code, w.Body)
		}
	}
	_, me = f.do(t, "GET", "/api/me", "", jar)
	tn, _ = me["tenant"].(map[string]any)
	doc, _ := tn["effective_config_yaml"].(string)
	if !strings.Contains(doc, "mcp__playwright__click") || !strings.Contains(doc, "toolfilter") {
		t.Fatalf("the user's own document does not carry the removal: %q", doc)
	}

	// Attributable TO THE USER. A manager id here would mean the write borrowed a privilege.
	_, audit := f.do(t, "GET", "/api/me/audit", "", jar)
	entries, _ := audit["audit"].([]any)
	found := 0
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok || m["field"] != "config_yaml" {
			continue
		}
		if m["actor"] != userID || m["target"] != userID {
			t.Errorf("config change attributed to actor=%v target=%v, want the user itself",
				m["actor"], m["target"])
		}
		found++
	}
	if found != 2 {
		t.Errorf("audit recorded %d config changes of 2: %v", found, entries)
	}
}

// TestToolFilterUserCannotDropABuiltin: the one thing a plain account may not switch off.
//
// Removing Claude Code's own tool does not trim fat, it takes away equipment the model is
// expected to have — and the page offers no switch for one to ANYBODY, so only a hand-crafted
// POST can get here. The check is on the resolved name rather than the caller's `kind`, because
// the kind is caller-supplied and a kind test is one lie away from being bypassed.
func TestToolFilterUserCannotDropABuiltin(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	mgrJar := w.Result().Cookies()
	w, _ = f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()

	for _, body := range []string{
		`{"kind":"tool","name":"Read","action":"exclude"}`,
		// The bypass: claim a kind whose branch does not classify built-ins.
		`{"kind":"mcp_tool","name":"Read","action":"exclude"}`,
		`{"kind":"","name":"Bash","action":"exclude"}`,
	} {
		w, _ := f.do(t, "POST", "/api/toolfilter", body, jar)
		if w.Code != http.StatusForbidden {
			t.Errorf("a plain account removed a built-in: %s = %d %s", body, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "compaction configuration") {
			t.Errorf("the refusal still blames the compaction-configuration gate, which is no "+
				"longer why this is refused: %s", w.Body)
		}
	}
	// A name that is not one of Claude Code's own is not a built-in, whatever it looks like.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, jar); w.Code != http.StatusOK {
		t.Errorf("another agent's client-side tool was refused as a built-in: %d %s", w.Code, w.Body)
	}
	// A manager keeps the escape hatch: refusing them here would be theatre, since they can
	// write the same line through PUT /api/me.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Read","action":"exclude"}`, mgrJar); w.Code != http.StatusOK {
		t.Errorf("a manager could not remove a built-in: %d %s", w.Code, w.Body)
	}
	// PUTTING ONE BACK is the repair, so it is never refused — otherwise a user whose manager
	// dropped Read for them cannot un-break their own agent.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Read","action":"include"}`, jar); w.Code != http.StatusOK {
		t.Errorf("a plain account could not re-include a built-in: %d %s", w.Code, w.Body)
	}
}

// TestToolFilterGrantsNothingElse: dropping the gate must widen this route and nothing around
// it. A user who can post one declaration name must still not be able to write a configuration
// document, and must not be able to reach anything the route does not model.
func TestToolFilterGrantsNothingElse(t *testing.T) {
	f := ctlFixture(t)
	f.signUp(t, "boss@ibm.com", "l")
	w, _ := f.signUp(t, "user@ibm.com", "u")
	jar := w.Result().Cookies()

	// A whole config document through PUT /api/me is still a manager's privilege — that gate is
	// the reason this one could be dropped, so it is the one that must not have moved.
	for _, body := range []string{
		`{"config_yaml":"pipeline: []\n"}`,
		`{"config":{"pipeline":["toolfilter"]}}`,
	} {
		if w, _ := f.do(t, "PUT", "/api/me", body, jar); w.Code != http.StatusForbidden {
			t.Errorf("a plain account wrote a configuration document: %s = %d %s",
				body, w.Code, w.Body)
		}
	}

	// The route models one component, so a save must leave the rest of the document alone —
	// including a pipeline entry it has no business knowing about.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"mcp_server","server":"playwright","action":"exclude"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("exclude = %d %s", w.Code, w.Body)
	}
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	if role, _ := tn["role"].(string); role != "user" {
		t.Errorf("the account's role changed through the toolfilter route: %q", role)
	}

	// And with no session at all it is nobody's own bill.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"mcp_server","server":"playwright","action":"exclude"}`, nil); w.Code == http.StatusOK {
		t.Error("an unauthenticated caller changed a configuration")
	}
}

// TestToolFilterRefusesWhatCannotBeRemoved: a provider-side tool is resolved by its `type` and
// the skills LISTING is one indivisible block of prose, so the answer for those is a reason
// rather than a stored configuration the filter would silently decline to act on.
//
// A SKILL is now accepted and is asserted in TestToolFilterStoresASkillUnderItsOwnPrefix.
func TestToolFilterRefusesWhatCannotBeRemoved(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()
	for _, body := range []string{
		`{"kind":"server_tool","name":"web_search","action":"exclude"}`,
		`{"kind":"skill_listing","name":"","action":"exclude"}`,
		`{"kind":"skill","name":"not a skill name","action":"exclude"}`,
		`{"kind":"tool","name":"","action":"exclude"}`,
		`{"kind":"tool","name":"Workflow","action":"maybe"}`,
		`{"kind":"mcp_server","server":"","action":"exclude"}`,
	} {
		if w, _ := f.do(t, "POST", "/api/toolfilter", body, jar); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, w.Code)
		}
	}
	// A junk name reaches the component's own validation and is refused there, so the
	// document never stores a pattern that could match more tomorrow than it does today.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Cron*","action":"exclude"}`, jar); w.Code != http.StatusBadRequest {
		t.Errorf("a wildcard was accepted: %d", w.Code)
	}
}

// hasExcluded reads the answer's excluded list.
func hasExcluded(out map[string]any, name string) bool {
	list, _ := out["excluded"].([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	// The no-dashboard fallback answers with the state document, whose field is Removed.
	for _, e := range asStrings(out["Removed"]) {
		if e == name {
			return true
		}
	}
	return false
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// WITH A DASHBOARD WIRED, the answer to a save comes from dash rather than from this
// package's fallback — and dash defaults a MANAGER to the whole service, whose removal list
// is tenant ""'s and does not exist. So the document has to be forced to the caller's own
// scope, or a save that succeeded and was audited answers "the control is unavailable and
// nothing is excluded" and the switch repaints as though it had failed.
//
// ctlFixture cannot see this: it has no dashboard, so h.api is nil and the fallback runs.
func TestToolFilterAnswersInTheCallersOwnScope(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetToolFilterState(f.h.DashToolFilter())
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	w, out := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("exclude = %d %s", w.Code, w.Body)
	}
	if en, _ := out["enabled"].(bool); !en {
		t.Errorf("a successful save answered enabled=false, so the control reports itself "+
			"unavailable right after it worked: %v", out)
	}
	if !hasExcluded(out, "Workflow") {
		t.Errorf("the answer to a successful save does not list the exclusion: %v", out)
	}
}

// A SKILL goes into the stored list under its own prefix, and comes back out described the way the
// inventory describes it.
//
// Two different strings, deliberately, and the mismatch is the bug this test exists to hold shut.
// The CONFIG needs `skill__<name>`, because a skill is removed from prose in a transcript message
// while a tool is removed from the `tools` array and a bare name cannot say which mechanism is
// meant. The PAGE matches an exclusion against an inventory row by (kind, name), and a row's name
// is the skill's own — so answering with the prefixed form would leave every skill's switch
// looking untouched after a write that had in fact landed.
func TestToolFilterStoresASkillUnderItsOwnPrefix(t *testing.T) {
	// The MANAGER fixture with the dashboard read hook wired, because the mapping under test is
	// the one the page consumes: ToolFilterDoc.Excluded. The no-dashboard fallback answers with
	// the raw config list by design and would not exercise it.
	f := newMgrFixture(t)
	f.h.API().SetToolFilterState(f.h.DashToolFilter())
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	w, out := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"skill","name":"dataviz","action":"exclude"}`, jar)
	if w.Code != http.StatusOK {
		t.Fatalf("exclude a skill = %d %s", w.Code, w.Body)
	}
	// The wire answer names the skill, not the config string.
	if !hasExcluded(out, "dataviz") {
		t.Fatalf("the answer does not list the skill as `dataviz`: %v", out)
	}
	if hasExcluded(out, "skill__dataviz") {
		t.Error("the answer leaked the config form; a page matching on it will never match a row")
	}
	// The stored document carries the PREFIXED form, builds, and runs the component.
	_, me := f.do(t, "GET", "/api/me", "", jar)
	tn, _ := me["tenant"].(map[string]any)
	doc, _ := tn["effective_config_yaml"].(string)
	if !strings.Contains(doc, "skill__dataviz") {
		t.Fatalf("the stored configuration does not carry `skill__dataviz`: %q", doc)
	}
	names, ok, reason := removedFrom(doc)
	if !ok || len(names) != 1 || names[0] != "skill__dataviz" {
		t.Fatalf("removedFrom(%q) = %v ok=%v %q", doc, names, ok, reason)
	}
	if !strings.Contains(doc, "toolfilter") {
		t.Error("the component is not in the pipeline, so the removal would never run")
	}

	// A plugin skill's name carries a colon, which the config charset allows and a naive
	// validator would not.
	if w, _ = f.do(t, "POST", "/api/toolfilter",
		`{"kind":"skill","name":"ponytail:ponytail","action":"exclude"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("exclude a plugin skill = %d %s", w.Code, w.Body)
	}

	// RECOVERY: re-including empties the list. The component stays (see the round-trip test).
	for _, body := range []string{
		`{"kind":"skill","name":"dataviz","action":"include"}`,
		`{"kind":"skill","name":"ponytail:ponytail","action":"include"}`,
	} {
		if w, _ = f.do(t, "POST", "/api/toolfilter", body, jar); w.Code != http.StatusOK {
			t.Fatalf("include = %d %s", w.Code, w.Body)
		}
	}
	_, me = f.do(t, "GET", "/api/me", "", jar)
	tn, _ = me["tenant"].(map[string]any)
	doc, _ = tn["effective_config_yaml"].(string)
	if strings.Contains(doc, "skill__") {
		t.Errorf("re-including left a skill in the removal list: %q", doc)
	}
}

// A removal switch has to show on the very next load. GET /api/toolfilter is served from the
// dashboard's response cache, which may answer from a body up to ten minutes old; the write must
// therefore drop that cached body, or a reload would show the switch as it was before the click.
func TestToolFilterWriteShowsOnTheNextRead(t *testing.T) {
	f := newMgrFixture(t)
	f.h.API().SetToolFilterState(f.h.DashToolFilter())
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()

	// Warm the cache with the state before the click.
	before, doc := f.do(t, "GET", "/api/toolfilter?tenant=me", "", jar)
	if before.Code != http.StatusOK {
		t.Fatalf("GET /api/toolfilter = %d %s", before.Code, before.Body)
	}
	if hasExcluded(doc, "Workflow") {
		t.Fatalf("precondition: Workflow is already excluded: %v", doc)
	}
	if again, _ := f.do(t, "GET", "/api/toolfilter?tenant=me", "", jar); again.Header().Get("X-Cache") != "hit" {
		t.Fatalf("second read X-Cache = %q; the test needs a warm cache to prove anything",
			again.Header().Get("X-Cache"))
	}

	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, jar); w.Code != http.StatusOK {
		t.Fatalf("exclude = %d %s", w.Code, w.Body)
	}
	after, doc := f.do(t, "GET", "/api/toolfilter?tenant=me", "", jar)
	if !hasExcluded(doc, "Workflow") {
		t.Errorf("the read right after the write still shows the old switch (X-Cache %q): %v",
			after.Header().Get("X-Cache"), doc)
	}
	if got := after.Header().Get("X-Cache"); got == "stale" {
		t.Errorf("served a stale body after a write that changed it")
	}
}
