package proxy

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/tooldefer"
)

// save-more tools (#485 D3, #869 item 4): the host half. The rows are dash's (dash/savemoretools.go);
// this file reads the account's tool deferral settings and history for them, and writes the actions
// through the shared adapt path (adaptapply.go), so a hosted account and a local proxy both work:
//
//   - keep / unkeep: add or take out a name on toolfilter's `keep` list (never defer it);
//   - defer / undefer: add or take out a name on toolfilter's `always_defer` list (#878);
//   - search_on / search_off: toolfilter `defer: auto` or `off` for the account;
//   - revert: undo the last save-more tools change.
//
// It writes only what a report this caller just fetched offered (dash.API.AdaptOffer).

const saveMoreToolsKind = "save-more-tools"

// deferConf is toolfilter's deferral settings in a configuration document: `defer`, `keep` and
// `always_defer`.
type deferConf struct {
	on           bool
	keep, always []string
}

// deferSettings reads toolfilter's deferral settings out of a configuration document.
func deferSettings(doc string) (c deferConf, ok bool, reason string) {
	f, err := config.ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return deferConf{}, false, "your stored configuration does not load, so it cannot be edited " +
			"as fields; a manager must repair it on the account page"
	}
	blk := f.Components[toolFilterComponent]
	return deferConf{on: docDefers(doc), keep: nameList(blk["keep"]), always: nameList(blk["always_defer"])}, true, ""
}

// nameList is one of toolfilter's name lists as a form value, sorted.
func nameList(v any) []string {
	var out []string
	switch list := v.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, v := range list {
			if s, isStr := v.(string); isStr && s != "" {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// editDefer is doc with toolfilter's `defer`, `keep` and `always_defer` rewritten by edit. Like
// editRemoved, it adds toolfilter to the pipeline when it is absent and leaves every other key alone.
func editDefer(current string, edit func(deferConf) deferConf) (string, error) {
	f, perr := config.ParseForm(current)
	if perr != nil || f.ParseError != "" {
		return "", statusError{http.StatusConflict, "your stored configuration does not load, so it cannot " +
			"be edited as fields; a manager must repair it on the account page"}
	}
	c, _, _ := deferSettings(current)
	c = edit(c)
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	blk := f.Components[toolFilterComponent]
	if blk == nil {
		blk = map[string]any{}
		f.Components[toolFilterComponent] = blk
	}
	blk["defer"] = "off"
	if c.on {
		blk["defer"] = "auto"
	}
	blk["keep"] = c.keep
	if len(c.always) > 0 {
		blk["always_defer"] = c.always
	} else {
		delete(blk, "always_defer") // an empty list is the default: leave no key behind
	}
	f.Pipeline = withComponent(f.Pipeline, toolFilterComponent, true)
	f.PipelineKnown = []string{toolFilterComponent}
	doc, err := config.ApplyForm(current, f)
	if err != nil {
		return "", statusError{http.StatusBadRequest, err.Error()}
	}
	return doc, nil
}

// saveMoreToolsReport serves GET /api/save-more/tools.
func (h *Handler) saveMoreToolsReport(w http.ResponseWriter, r *http.Request) {
	tn, who, err := h.adaptCaller(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	if h.opts.Dashboard == nil || h.opts.Dashboard.DB() == nil || h.api == nil {
		ctlErr(w, http.StatusServiceUnavailable, "the report needs the dashboard database (--dashboard)")
		return
	}
	q := r.URL.Query()
	until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	if until <= 0 {
		until = time.Now().UnixMilli()
	}
	if since <= 0 || since >= until {
		since = until - 30*86400e3
	}
	if err := h.limiter.AllowAction("save-more-tools|"+tn.ID, "tools reports", adaptReportBurst); err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	doc, cerr := h.adaptConfig(who)
	host := dash.SaveMoreToolsHost{Writable: cerr == nil}
	if cerr == nil {
		c, ok, reason := deferSettings(doc)
		host.SearchOn, host.Keep, host.AlwaysDefer, host.Writable, host.Reason = c.on, c.keep, c.always, ok, reason
		host.Removed, _, _ = removedFrom(doc)
	} else {
		_, host.Reason = statusOf(cerr)
	}
	if ds, _ := h.deferPeek(); ds != nil {
		keep, always := host.Keep, host.AlwaysDefer
		host.Explain = func(name string) (bool, string) { return tooldefer.Explain(ds.hist, tn.ID, name, keep, always) }
	}
	var price func(string) (modelinfo.Price, bool)
	if h.opts.Prices != nil {
		ctx := r.Context()
		price = func(model string) (modelinfo.Price, bool) { return h.opts.Prices.Price(ctx, model) }
	}
	rep, err := h.opts.Dashboard.DB().SaveMoreTools(dash.Filter{Tenant: tn.ID, Since: since, Until: until}, price, host)
	if err != nil {
		ctlErr(w, http.StatusInternalServerError, "could not read the history")
		return
	}
	if rep.Writable {
		rep.BasisID = h.api.OfferAdapt(saveMoreToolsKind, who.TenantID, rep.SaveMoreToolsOffers())
	}
	writeJSON(w, http.StatusOK, rep)
}

// saveMoreToolsApply serves POST /api/save-more/tools/apply: one option of a report this caller just
// fetched, or "revert".
func (h *Handler) saveMoreToolsApply(w http.ResponseWriter, r *http.Request) {
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
		if _, err := h.adaptRevert(who, saveMoreToolsKind); err != nil {
			code, msg := statusOf(err)
			ctlErr(w, code, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "revert", "previous": "current",
			"kind": "tools", "effective": "next_session",
			"note": "restored the configuration from before the last save-more tools change"})
		return
	}
	if in.Basis == "" {
		ctlErr(w, http.StatusBadRequest, "basis is required: send the basis_id of the report you are acting on")
		return
	}
	v, ok := h.API().AdaptOffer(saveMoreToolsKind, who.TenantID, in.Basis, in.Option)
	opt, _ := v.(dash.SaveMoreToolsOption)
	if !ok || opt.ID == "" {
		ctlErr(w, http.StatusConflict, "that option is not in a current report; run the report again")
		return
	}
	// Defence in depth: only a tool or server name is written to a name list, never Context Guru's own.
	switch opt.Action {
	case "keep", "unkeep", "defer", "undefer":
		if opt.Name == "" || ownDecl.MatchString(opt.Name) {
			ctlErr(w, http.StatusBadRequest, "refusing to write "+opt.Name)
			return
		}
	}
	if opt.Action == "defer" && tooldefer.NeverDefer(opt.Name) {
		ctlErr(w, http.StatusBadRequest, "refusing to defer "+opt.Name+": it is on the never-defer list")
		return
	}
	var note string
	_, _, err = h.adaptWrite(who, saveMoreToolsKind, opt.ID, func(doc string) (string, error) {
		return editDefer(doc, func(c deferConf) deferConf {
			switch opt.Action {
			case "keep":
				c.keep, note = withDecl(c.keep, opt.Name, true), "never deferring "+opt.Name
			case "unkeep":
				c.keep, note = withDecl(c.keep, opt.Name, false), "took "+opt.Name+" off the keep list"
			case "defer":
				c.always, note = withDecl(c.always, opt.Name, true), "always deferring "+opt.Name
			case "undefer":
				c.always, note = withDecl(c.always, opt.Name, false), "took "+opt.Name+" off the always-defer list"
			case "search_on":
				c.on, note = true, "tool search on: tools your history shows unused are deferred"
			case "search_off":
				c.on, note = false, "tool search off: every tool is sent in full"
			}
			return c
		})
	})
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	// A thread keeps the set it decided at its first request (the cache rule), so a change reaches
	// new threads only.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": opt.ID, "previous": "current",
		"kind": "tools", "effective": "next_session", "note": note, "undo": map[string]string{"option": "revert"}})
}
