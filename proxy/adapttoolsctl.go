package proxy

import (
	"github.com/rossoctl/context-guru/config"
	"net/http"
	"regexp"
	"strings"

	"github.com/rossoctl/context-guru/internal/skills"
)

// ownDecl is Context Guru's own tools and skills, the same rule dash/adapttools.go lists by.
var ownDecl = regexp.MustCompile(`(?i)context[-_]?guru`)

// adaptToolsApply applies one option of GET /api/adapt/tools to the caller's removal list: the
// WRITE half of /context-guru:adapt tools. Who may, how it is written and how it is undone are
// the shared adapt helper's (adaptapply.go); this decides only what the list becomes. It writes
// only names an option of a report this account just fetched offered (dash.API.AdaptOffer), so
// it can add nothing the report would not list: never a built-in, never Context Guru's own.
func (h *Handler) adaptToolsApply(w http.ResponseWriter, r *http.Request) {
	who, err := h.adaptPrincipal(r)
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
	if in.Option == "revert" {
		if _, err := h.adaptRevert(who, "tools"); err != nil {
			code, msg := statusOf(err)
			ctlErr(w, code, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "revert", "previous": "current",
			"kind": "tools", "effective": "now",
			"note": "restored the configuration from before the last tools change"})
		return
	}
	if in.Basis == "" {
		ctlErr(w, http.StatusBadRequest, "basis is required: send the basis_id of the report you are acting on")
		return
	}
	v, ok := h.API().AdaptOffer("tools", who.TenantID, in.Basis, in.Option)
	names, _ := v.([]string)
	if !ok {
		ctlErr(w, http.StatusConflict, "that option is not in a current report; run the report again")
		return
	}
	// Defence in depth: whatever the report builder does, only MCP and skill entries, and never
	// Context Guru's own, are written from here.
	for _, n := range names {
		if !(strings.HasPrefix(n, "mcp__") || strings.HasPrefix(n, skills.RemovePrefix)) || ownDecl.MatchString(n) {
			ctlErr(w, http.StatusBadRequest, "refusing to write "+n)
			return
		}
	}
	restore := in.Option == "restore"
	note := "nothing changed"
	if len(names) > 0 {
		_, next, err := h.adaptWrite(who, "tools", in.Option, func(doc string) (string, error) {
			return editRemoved(doc, func(cur []string) []string {
				for _, n := range names {
					cur = withDecl(cur, n, !restore)
				}
				return cur
			})
		})
		if err != nil {
			code, msg := statusOf(err)
			ctlErr(w, code, msg)
			return
		}
		note = applyNote(names, next, restore)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": in.Option, "previous": "keep",
		// The filter acts on the next request, which re-writes the cached prefix once.
		"effective": "now", "note": note, "undo": map[string]string{"option": "revert"}})
}

// applyNote says what the list now does, read back from the configuration that was written
// (config.RemovalNote).
func applyNote(names []string, doc string, restore bool) string {
	return config.RemovalNote(names, doc, restore)
}
