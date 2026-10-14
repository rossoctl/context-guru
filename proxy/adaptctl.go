package proxy

// /context-guru:adapt compaction's write half: store an option the report (dash/
// adaptcompaction.go) offered as the caller's own configuration. Identity, the rate limit, the
// audit trail, the undo and the single-tenant hot reload are adaptapply.go's.
//
// What may change is fixed: which of the product's deterministic compaction components run,
// their order, and the two settings the report searched. Everything else in the document —
// every other component, its position, every other setting, the cache block — is kept. That
// is why a plain account, or its proxy token, may do this for itself while arbitrary YAML stays
// manager-only on PUT /api/me.

import (
	"net/http"
	"strings"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
)

// DashAdaptConfig is the dashboard's read hook for the adapt report: the caller's EFFECTIVE
// configuration (on a single-tenant proxy, including an applied override) and the service
// default.
func (h *Handler) DashAdaptConfig() dash.AdaptCompactionConfigFunc {
	return func(id string) (string, string, bool, string) {
		who := adaptWho{TenantID: id, Local: h.registry() == nil}
		doc, err := h.adaptConfig(who)
		if err != nil {
			return "", "", false, "could not read your configuration"
		}
		def := h.opts.BaseConfig
		if reg := h.registry(); reg != nil {
			def = reg.DefaultConfig()
		}
		return doc, def, true, ""
	}
}

// adaptCompactionApply serves POST /api/adapt/compaction/apply {"option", "basis"}. "revert"
// (no basis) undoes the last change.
func (h *Handler) adaptCompactionApply(w http.ResponseWriter, r *http.Request) {
	h.adaptConfigApply(w, r, "compaction")
}

// saveMoreConfigApply serves POST /api/save-more/config/apply: the same, for /context-guru:save-more
// config's options, which include switching to a shipped preset.
func (h *Handler) saveMoreConfigApply(w http.ResponseWriter, r *http.Request) {
	h.adaptConfigApply(w, r, "compaction") // one report, one set of offers, under either name
}

func (h *Handler) adaptConfigApply(w http.ResponseWriter, r *http.Request, kind string) {
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
		if _, err := h.adaptRevert(who, kind); err != nil {
			code, msg := statusOf(err)
			ctlErr(w, code, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": "revert", "previous": "current",
			"kind": kind, "effective": "now",
			"note": "restored the configuration from before the last change to it, whichever setting that change was"})
		return
	}
	if in.Basis == "" {
		ctlErr(w, http.StatusBadRequest, "basis is required: the basis_id of the report the option is from")
		return
	}
	v, ok := h.API().AdaptOffer(kind, who.TenantID, in.Basis, in.Option)
	choice, _ := v.(dash.AdaptCompactionChoice)
	if !ok {
		ctlErr(w, http.StatusConflict, "that option is not in a current report; run the report again")
		return
	}
	var pipeline []string
	_, _, err = h.adaptWrite(who, kind, in.Option, func(doc string) (string, error) {
		if dash.AdaptConfigHash(doc) != choice.ConfigHash {
			return "", statusError{http.StatusConflict, "your configuration changed since the report was made; run it again"}
		}
		if choice.Preset != "" {
			next, err := dash.AdaptPresetDoc(doc, choice.Preset)
			if err != nil {
				return "", statusError{http.StatusBadRequest, err.Error()}
			}
			cfg, err := config.LoadBytes([]byte(next))
			if err != nil {
				return "", statusError{http.StatusBadRequest, err.Error()}
			}
			pipeline = cfg.Pipeline
			return next, nil
		}
		next, pipe, err := adaptCompactionDoc(doc, choice)
		pipeline = pipe
		return next, err
	})
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": in.Option, "previous": "current",
		"effective": "now", "note": "compaction pipeline: " + strings.Join(pipeline, " > "),
		"undo": map[string]string{"option": "revert"}})
}

// adaptCompactionDoc is doc with choice applied: the candidates replaced in place, the searched
// settings set, nothing else touched.
func adaptCompactionDoc(doc string, choice dash.AdaptCompactionChoice) (string, []string, error) {
	f, err := config.ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return "", nil, statusError{http.StatusConflict, "your stored configuration does not load, so it " +
			"cannot be edited as fields; a manager must repair it on the account page"}
	}
	for c, knobs := range choice.Settings {
		for k, v := range knobs {
			// The grid the report searched, or the value already stored.
			if stored, ok := f.Components[c][k].(int); !dash.AdaptCompactionKnobAllowed(c, k, v) && (!ok || stored != v) {
				return "", nil, statusError{http.StatusBadRequest, "not an adaptable setting: " + c + "." + k}
			}
		}
	}
	base := f.Pipeline
	f.Pipeline = dash.AdaptCompactionMerge(f.Pipeline, choice.Candidates)
	// The candidates are the only names this may take out; ApplyForm keeps anything else.
	f.PipelineKnown, f.PipelineBase = dash.AdaptCompactionCandidates, base
	// Nor does it state the cache block: a parsed form carries one, and ApplyForm would write
	// `keepalive: false` into a document that never said it, pinning the account against a
	// default that later changes.
	f.Cache = nil
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	for c, knobs := range choice.Settings {
		if f.Components[c] == nil {
			f.Components[c] = map[string]any{}
		}
		for k, v := range knobs {
			f.Components[c][k] = v
		}
	}
	next, err := config.ApplyForm(doc, f)
	if err != nil {
		return "", nil, statusError{http.StatusBadRequest, err.Error()}
	}
	return next, f.Pipeline, nil
}
