package proxy

import (
	"sync"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
)

// autoEligTTL bounds how stale a tenant's eligibility list may be. The list only matters at the
// two moments a session's removed set is decided, so five minutes of staleness changes nothing.
const autoEligTTL = 5 * time.Minute

type autoEligEntry struct {
	at    time.Time
	names []string
}

var autoElig sync.Map // tenant id -> autoEligEntry

// autoEligible is the tenant's history-derived removal candidates, cached.
func (h *Handler) autoEligible(tenant string) []string {
	if h.opts.AutoToolEligible != nil {
		return h.opts.AutoToolEligible(tenant)
	}
	if h.rec == nil {
		return nil
	}
	if v, ok := autoElig.Load(tenant); ok && time.Since(v.(autoEligEntry).at) < autoEligTTL {
		return v.(autoEligEntry).names
	}
	names, err := h.rec.DB().AutoEligibleDecls(tenant, dash.AutoMinSessions, dash.AutoMinSpan)
	if err != nil {
		return nil
	}
	autoElig.Store(tenant, autoEligEntry{time.Now(), names})
	return names
}

// applyAutoTools runs toolfilter auto mode on the post-pipeline body when the tenant's toolfilter
// has `auto: true`. Anthropic dialect only. Books the net saving on the trace.
func (h *Handler) applyAutoTools(tn *Tenancy, wire string, body []byte, tr *apply.Trace) []byte {
	if wire != "anthropic" || tr.Session == "" {
		return body
	}
	tf, ok := tn.Pipe.Find("toolfilter").(interface{ Auto() bool })
	if !ok || !tf.Auto() {
		return body
	}
	out, tok, n := apply.AutoFilterTools(tn.Store, tr.Session, body, h.autoEligible(tn.ID), tr.ColdCache)
	if n > 0 {
		tr.FilteredDeclTokens += tok
		tr.FilteredDecls += n
	}
	return out
}
