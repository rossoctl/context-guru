package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// The write half of declaration removal has to inherit three things from the control plane
// rather than reimplement them, and each of them is a way the switch could go wrong:
//
//	the manager gate  — the removal list decides what runs on the traffic, exactly like
//	                    config_yaml, so a plain user cannot set it for themselves;
//	validation        — the stored document must still build, or the account's next turn
//	                    fails on a configuration a settings page accepted;
//	the audit trail   — a change to what we send must be attributable.
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

	// RECOVERY: re-including both empties the list, which takes the component back out of
	// the pipeline rather than leaving a pass over every request that removes nothing.
	for _, name := range []string{`{"kind":"tool","name":"Workflow","action":"include"}`,
		`{"kind":"mcp_server","server":"playwright","action":"include"}`} {
		if w, _ = f.do(t, "POST", "/api/toolfilter", name, jar); w.Code != http.StatusOK {
			t.Fatalf("include = %d %s", w.Code, w.Body)
		}
	}
	_, me = f.do(t, "GET", "/api/me", "", jar)
	tn, _ = me["tenant"].(map[string]any)
	doc, _ = tn["effective_config_yaml"].(string)
	if strings.Contains(doc, "Workflow") || strings.Contains(doc, "toolfilter") {
		t.Errorf("re-including left the filter configured: %q", doc)
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

// TestToolFilterIsAManagersDecision: a hidden control is not a permission, and this route is
// one curl away. Same rule PUT /api/me applies to config_yaml.
func TestToolFilterIsAManagersDecision(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l") // the fixture's manager
	_ = w
	w, _ = f.signUp(t, "user@ibm.com", "u")
	userJar := w.Result().Cookies()
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, userJar); w.Code != http.StatusForbidden {
		t.Errorf("a plain user set the removal list: %d", w.Code)
	}
	// And with no session at all.
	if w, _ := f.do(t, "POST", "/api/toolfilter",
		`{"kind":"tool","name":"Workflow","action":"exclude"}`, nil); w.Code == http.StatusOK {
		t.Error("an unauthenticated caller changed a configuration")
	}
}

// TestToolFilterRefusesWhatCannotBeRemoved: a provider-side tool and a skill are not elements
// of `tools` we can drop, so the answer is a reason rather than a stored configuration the
// filter would silently decline to act on.
func TestToolFilterRefusesWhatCannotBeRemoved(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "boss@ibm.com", "l")
	jar := w.Result().Cookies()
	for _, body := range []string{
		`{"kind":"server_tool","name":"web_search","action":"exclude"}`,
		`{"kind":"skill","name":"dataviz","action":"exclude"}`,
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
