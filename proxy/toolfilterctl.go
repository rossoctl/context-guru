package proxy

// The WRITE half of declaration removal, and the read hook the dashboard's control needs.
//
// It lives in the control plane rather than beside the read route in dash for one reason:
// the removal list is part of an account's compaction configuration, and the rules that
// govern writing that — strict validation of the whole document and an audit entry naming the
// field — already exist here on PUT /api/me. A second writer with its own copy of those rules
// is how one of the two ends up missing one; this route reuses the same Form round trip and the
// same registry Update, so the only thing it adds is the shape a single switch posts.
//
// What it does NOT inherit is that route's manager gate. A user may drop the declarations they
// added themselves — their own MCP tools and MCP servers — because what their prompt carries is
// their own bill. declConfigName says which kinds are removable at all; ctlToolFilter says the
// one removable thing a user may not drop, and why.
//
// It also inherits what the route table gives every control-plane write: the ctlTenant gate,
// cookie-only identity (a pasted proxy token cannot change a configuration), and the
// cross-site guard inside readJSON.

import (
	"errors"
	"net/http"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/tenant"
)

// toolFilterComponent is the pipeline name that carries the list.
const toolFilterComponent = config.ToolfilterComponent

// DashToolFilter is the dashboard's READ hook for an account's removal list: the inventory
// page needs to draw a switch per declaration, and dash holds no tenant configuration of its
// own. Wired in cmd/context-guru-proxy beside the capture hook.
func (h *Handler) DashToolFilter() func(string) dash.ToolFilterState {
	return func(id string) dash.ToolFilterState {
		reg := h.registry()
		if reg == nil {
			// No switch to offer (POST /api/toolfilter needs accounts), but the list the running
			// pipeline filters is known: its configuration's, including what /context-guru:adapt
			// applied (adaptapply.go keeps that as the effective document).
			doc, _ := h.adaptConfig(adaptWho{Local: true})
			names, _, _ := removedFrom(doc)
			return dash.ToolFilterState{Removed: names, Reason: "this proxy has no accounts: its removal " +
				"list is its configuration's, including what /context-guru:adapt tools applied"}
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
	return config.ToolfilterRemoved(doc)
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
	// NOT the config_yaml gate PUT /api/me applies, and deliberately so: what a user carries
	// in their own prompt is their own bill, and the inventory page exists to let them stop
	// paying it. This route is narrower than that gate by construction — ONE validated
	// declaration name, PipelineKnown pinned to this component so nothing else in the document
	// can move, and reg.Update confining the write to the caller's own account and auditing it.
	//
	// A built-in is the one exception, and only on the way OUT. Removing one does not degrade
	// the agent gracefully, it breaks it, and the page offers no switch for one to anybody — so
	// a hand-crafted POST gets a reason rather than a configuration whose next session fails.
	// Checked on the RESOLVED name and not the claimed kind, because the kind is caller-supplied:
	// {"kind":"mcp_tool","name":"Read"} resolves to `Read` and must not slip past a kind test.
	// A manager keeps it because refusing them here would be theatre — they can write the same
	// line through PUT /api/me, which is exactly the privilege a user does not have. And an
	// `include` is never refused: re-adding a built-in is the repair, not the damage.
	if exclude && !t.IsManager() && dash.IsBuiltinTool(dash.KindTool, name) {
		ctlErr(w, http.StatusForbidden, "removing one of Claude Code's own tools breaks the "+
			"agent rather than saving you anything, so a manager has to make that change")
		return
	}
	// A deferred MCP tool is a name in a message, not a `tools` element, so the filter cannot
	// remove it: storing the exclusion would save nothing and hide the item from the report.
	// Fail closed when the lookup itself fails, for the same reason. An include is never refused.
	if exclude && h.rec != nil {
		deferred, err := h.rec.DB().WithContext(r.Context()).IsDeferredDecl(t.ID, name)
		if err != nil {
			ctlErr(w, http.StatusServiceUnavailable, "could not check whether this tool is deferred; try again")
			return
		}
		if deferred {
			ctlErr(w, http.StatusConflict, "Claude Code sends this as a deferred tool, which the proxy "+
				"filter cannot remove; use the client command shown beside it instead")
			return
		}
	}
	if code, msg := h.updateRemoved(t, func(names []string) []string {
		return withDecl(names, name, exclude)
	}); code != 0 {
		ctlErr(w, code, msg)
		return
	}
	h.writeToolFilterDoc(w, r, t.ID)
}

// updateRemoved rewrites the caller's removal list through edit, with the same validation and
// audited registry Update for every writer. A non-zero code is the failure to report.
func (h *Handler) updateRemoved(t *tenant.Tenant, edit func([]string) []string) (int, string) {
	reg := h.registry()
	doc, err := editRemoved(reg.Config(t), edit)
	if err != nil {
		return statusOf(err)
	}
	if err := reg.Update(t, t.ID, tenant.Patch{ConfigYAML: &doc}); err != nil {
		if errors.Is(err, tenant.ErrForbidden) {
			return http.StatusForbidden, "not permitted"
		}
		return http.StatusBadRequest, err.Error()
	}
	return 0, ""
}

// editRemoved is a configuration document with its toolfilter removal list rewritten by edit:
// the one place that edit is made, for the Inventory switch and for /context-guru:adapt tools.
// The edit itself is config.EditToolfilterRemoved; this adds the HTTP status.
func editRemoved(current string, edit func([]string) []string) (string, error) {
	doc, err := config.EditToolfilterRemoved(current, edit)
	return doc, editStatus(err)
}

// editStatus gives a config editor's error the HTTP status the host answers with: 409 for a
// document that does not load, 400 for an invalid value.
func editStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, config.ErrUnloadable):
		return statusError{http.StatusConflict, err.Error()}
	}
	return statusError{http.StatusBadRequest, err.Error()}
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

// declConfigName maps an inventory row to the string the removal list holds (config.DeclConfigName).
func declConfigName(kind, name, server string) (string, error) {
	return config.DeclConfigName(kind, name, server)
}

// withDecl adds or removes one name, keeping the list sorted and duplicate-free.
func withDecl(names []string, name string, add bool) []string {
	return config.WithName(names, name, add)
}

// withComponent ensures a component is present (appended last) or absent from a pipeline.
func withComponent(pipeline []string, name string, want bool) []string {
	return config.WithComponent(pipeline, name, want)
}
