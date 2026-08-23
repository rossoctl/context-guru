package proxy

// The WRITE half of declaration removal, and the read hook the dashboard's control needs.
//
// It lives in the control plane rather than beside the read route in dash for one reason:
// the removal list is part of an account's compaction configuration, and the rules that
// govern writing that — strict validation of the whole document, an audit entry naming the
// field, and the manager gate — already exist here on PUT /api/me. A second writer with its
// own copy of those rules is how one of the two ends up missing one; this route reuses the
// same Form round trip and the same registry Update, so the only thing it adds is the shape
// a single switch posts.
//
// It also inherits what the route table gives every control-plane write: the ctlTenant gate,
// cookie-only identity (a pasted proxy token cannot change a configuration), and the
// cross-site guard inside readJSON.

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/rossoctl/context-guru/tenant"
)

// toolFilterComponent is the pipeline name that carries the list.
const toolFilterComponent = "toolfilter"

// DashToolFilter is the dashboard's READ hook for an account's removal list: the inventory
// page needs to draw a switch per declaration, and dash holds no tenant configuration of its
// own. Wired in cmd/context-guru-proxy beside the capture hook.
func (h *Handler) DashToolFilter() func(string) dash.ToolFilterState {
	return func(id string) dash.ToolFilterState {
		reg := h.registry()
		if reg == nil {
			return dash.ToolFilterState{Reason: "this proxy has no accounts, so the removal " +
				"list is whatever its own config file sets"}
		}
		t, err := reg.Get(id)
		if err != nil {
			return dash.ToolFilterState{Reason: "could not read your account"}
		}
		names, enabled, reason := removedFrom(reg.Config(t))
		// The control is offered even when the component is not in the pipeline yet — adding
		// it is exactly what the first exclusion does. What genuinely disables the control is
		// a document we could not read, because a save from a misread form would post whatever
		// the fallback happened to see (the same refusal PUT /api/me makes).
		if !enabled {
			return dash.ToolFilterState{Removed: names, Reason: reason}
		}
		return dash.ToolFilterState{Enabled: true, Removed: names}
	}
}

// removedFrom reads the removal list out of a stored configuration document.
func removedFrom(doc string) (names []string, ok bool, reason string) {
	f, err := config.ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return nil, false, "your stored configuration does not load, so it cannot be edited " +
			"as fields; a manager must repair it on the account page"
	}
	// The Form holds a stated key as `any`, and yaml.v3 decodes a sequence into []any — so
	// both shapes are read rather than only the one a Go caller would have set.
	switch list := f.Components[toolFilterComponent]["remove"].(type) {
	case []string:
		names = append(names, list...)
	case []any:
		for _, v := range list {
			if str, isStr := v.(string); isStr && str != "" {
				names = append(names, str)
			}
		}
	}
	sort.Strings(names)
	return names, true, ""
}

// ctlToolFilter excludes or re-includes ONE declaration and answers with the same document
// GET /api/toolfilter serves, so a switch repaints from the server's state rather than from
// the DOM it just changed.
func (h *Handler) ctlToolFilter(w http.ResponseWriter, r *http.Request) {
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Server string `json:"server"`
		Action string `json:"action"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// The same gate PUT /api/me applies to config_yaml, and for the same reason: this decides
	// what runs on the traffic. Enforced here and not only by hiding the switch — a hidden
	// control is not a permission, and this route is one curl away.
	if !t.IsManager() {
		ctlErr(w, http.StatusForbidden, "a manager sets the compaction configuration")
		return
	}
	name, err := declConfigName(in.Kind, in.Name, in.Server)
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	exclude := in.Action == "exclude"
	if !exclude && in.Action != "include" {
		ctlErr(w, http.StatusBadRequest, `action must be "exclude" or "include"`)
		return
	}
	reg := h.registry()
	current := reg.Config(t)
	f, perr := config.ParseForm(current)
	if perr != nil || f.ParseError != "" {
		ctlErr(w, http.StatusConflict, "your stored configuration does not load, so it cannot "+
			"be edited as fields; a manager must repair it on the account page")
		return
	}
	names, _, _ := removedFrom(current)
	names = withDecl(names, name, exclude)
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	if f.Components[toolFilterComponent] == nil {
		f.Components[toolFilterComponent] = map[string]any{}
	}
	f.Components[toolFilterComponent]["remove"] = names
	// ADDED when absent, and never REMOVED. `toolfilter` now ships in the default pipeline with
	// an empty removal list, so naming a tool is a settings change and not a pipeline change —
	// and an emptied list that took the component back out would write an explicit pipeline
	// DIVERGING from the default, for no gain: a component with nothing to remove already does
	// nothing. The add stays because an account whose stored pipeline predates that default does
	// not have it, and without the add that account's first exclusion would save nothing and
	// report success.
	//
	// Position is meaningless here either way: the rewrite lives in apply, not in the component,
	// because `tools` is a top-level field the message pipeline never sees.
	f.Pipeline = withComponent(f.Pipeline, toolFilterComponent, true)
	// This route models exactly one component, so it claims exactly one. Without the claim
	// ApplyForm preserves anything the stored pipeline runs and this omission is not sent —
	// which is the rule that stops a stale settings page dropping a component it cannot see,
	// and would here stop an emptied removal list from taking toolfilter back out.
	f.PipelineKnown = []string{toolFilterComponent}
	doc, err := config.ApplyForm(current, f)
	if err != nil {
		// The message names the offending value; showing it beats "invalid".
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := reg.Update(t, t.ID, tenant.Patch{ConfigYAML: &doc}); err != nil {
		if errors.Is(err, tenant.ErrForbidden) {
			ctlErr(w, http.StatusForbidden, "not permitted")
			return
		}
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h.writeToolFilterDoc(w, r, t.ID)
}

// writeToolFilterDoc answers with the read route's document, so one shape serves both.
//
// The document is built for the caller's OWN account, and that has to be forced: the
// dashboard's scope helper defaults a MANAGER to the whole service, so the document it would
// otherwise return describes every tenant's traffic and reads the removal list of tenant ""
// — which does not exist, so a successful, audited save would answer `enabled:false` with an
// empty exclusion list and the switch would repaint as though nothing had happened. `tenant=me`
// is that helper's own way back to own-only, so this reuses the one parameter that moves the
// scope rather than adding a second entry point beside it.
func (h *Handler) writeToolFilterDoc(w http.ResponseWriter, r *http.Request, id string) {
	if h.api != nil {
		own := r.Clone(r.Context())
		own.URL.RawQuery = "tenant=me"
		if doc, err := h.api.ToolFilterDocument(own); err == nil {
			writeJSON(w, http.StatusOK, doc)
			return
		}
	}
	// No dashboard (or its read failed): the write succeeded, so answer with the part we
	// know rather than an error the caller would read as "the change did not happen".
	st := h.DashToolFilter()(id)
	writeJSON(w, http.StatusOK, dash.ToolFilterState{
		Enabled: st.Enabled, Removed: st.Removed, Reason: st.Reason,
	})
}

// declConfigName maps an inventory row to the string the removal list holds, and refuses the
// kinds that are not removable — a suggestion never offers them, and a hand-crafted POST
// should get a reason rather than a configuration the filter will silently decline.
func declConfigName(kind, name, server string) (string, error) {
	switch kind {
	case "mcp_server":
		if server == "" {
			return "", errors.New("an MCP server needs a server name")
		}
		return "mcp__" + server, nil
	case dash.KindServerTool:
		return "", errors.New("a provider-side tool is resolved by the provider from its " +
			"type, not by a schema we send, so it cannot be removed here")
	case dash.KindSkill:
		if name == "" {
			return "", errors.New("no skill name")
		}
		if !skills.ValidName(name) {
			// The listing is the authority for what may be cut out of a real request, so a name
			// that could not have come from one is refused rather than written into a config that
			// would match nothing forever.
			return "", errors.New("that is not a skill name")
		}
		return skills.RemovePrefix + name, nil
	case dash.KindSkillListing:
		return "", errors.New("the skills listing is one indivisible block of prose — remove the " +
			"individual skills instead, and the listing shrinks with them")
	case dash.KindTool, dash.KindMCPTool, "":
		if name == "" {
			return "", errors.New("no declaration name")
		}
		return name, nil
	}
	return "", errors.New("unknown declaration kind " + kind)
}

// withDecl adds or removes one name, keeping the list sorted and duplicate-free so the stored
// document does not churn on a no-op change.
func withDecl(names []string, name string, add bool) []string {
	out := make([]string, 0, len(names)+1)
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	if add {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// withComponent ensures a component is present (appended last) or absent from a pipeline.
func withComponent(pipeline []string, name string, want bool) []string {
	out := make([]string, 0, len(pipeline)+1)
	for _, n := range pipeline {
		if strings.TrimSpace(n) != name {
			out = append(out, n)
		}
	}
	if want {
		out = append(out, name)
	}
	return out
}
