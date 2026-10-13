package proxy

import (
	"net/http"
	"strings"
)

// ctlAdaptToolsApply applies one option of GET /api/adapt/tools to the caller's removal list:
// the WRITE half of /context-guru:adapt tools. It only ever writes names the caller's own current
// report offers, so it can add nothing the report would not list (never a built-in, never
// Context Guru's own tools or skills), and it writes through the same validated, audited update
// as the Inventory page's switch.
func (h *Handler) ctlAdaptToolsApply(w http.ResponseWriter, r *http.Request) {
	if h.registry() == nil || h.api == nil {
		ctlErr(w, http.StatusNotImplemented, "this proxy has no account configuration to write to: "+
			"run each item's remove command, or add its remove_as name to components.toolfilter.remove "+
			"in the proxy's config and restart it")
		return
	}
	t, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	var in struct {
		Option string `json:"option"`
		Basis  string `json:"basis"`
	}
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	// The caller's OWN report, whatever scope a manager's default view would be.
	own := r.Clone(r.Context())
	own.URL.RawQuery = "tenant=me"
	doc, err := h.api.AdaptToolsDocument(own)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not build your report: "+err.Error())
		return
	}
	if in.Basis != "" && in.Basis != doc.BasisID {
		ctlErr(w, http.StatusConflict, "the report changed since you saw it; read it again and decide on the current one")
		return
	}
	names, ok := doc.OptionRemove(in.Option)
	if !ok {
		ctlErr(w, http.StatusBadRequest, "that option is not in your current report")
		return
	}
	restore := in.Option == "restore"
	undo, note := "restore", "filtering "+strings.Join(names, ", ")
	switch {
	case len(names) == 0:
		undo, note = "keep", "nothing changed"
	case restore:
		undo, note = "keep", "stopped filtering "+strings.Join(names, ", ")
	}
	if len(names) > 0 {
		if code, msg := h.updateRemoved(t, func(cur []string) []string {
			for _, n := range names {
				cur = withDecl(cur, n, !restore)
			}
			return cur
		}); code != 0 {
			ctlErr(w, code, msg)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": in.Option, "previous": doc.Current,
		// The filter acts on the next request, which re-writes the cached prefix once.
		"effective": "now", "note": note, "undo": map[string]string{"option": undo},
	})
}
