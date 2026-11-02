package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// The proxy's half of tool deferral (#485). The decision and the rewrite are package tooldefer's;
// this file only wires them: which requests ask for it (the tenant's toolfilter `defer: auto`, which
// only the ultrahigh preset sets), the probe through the host's Dispatcher, the retry of the original
// request after a 400, the ENABLE_TOOL_SEARCH flag from /lifecycle, and the /stats ledger.

// deferState is one handler's deferral state. Built lazily, so a proxy no tenant of which asks for
// deferral pays nothing.
type deferState struct {
	once   sync.Once
	hist   *tooldefer.Mem
	stop   func()
	gate   *tooldefer.Gate
	prober *tooldefer.Prober
	ledger tooldefer.Ledger
}

// toolDefer returns the handler's deferral state, building it on first use. The history reads and
// writes its own table in the dashboard database when there is one, else it is memory only.
func (h *Handler) toolDefer() *deferState {
	h.defer_.once.Do(func() {
		d := &h.defer_
		d.hist = tooldefer.NewMem(nil)
		if h.rec != nil {
			if db := h.rec.DB(); db != nil && db.Path() != "" && db.Path() != ":memory:" {
				d.hist = tooldefer.NewMem(db.SQL())
			}
		}
		d.gate = tooldefer.NewGate()
		d.prober = tooldefer.NewProber()
		// The verdicts survive a restart with the history, so a thread decided before it keeps its set.
		d.prober.Persist = d.hist.SaveVerdict
		d.stop = d.hist.Open(0, d.prober)
	})
	return &h.defer_
}

// deferOn reports whether this tenant's pipeline asks for deferral, and its keep list.
func deferOn(tn *Tenancy) (on bool, keep []string) {
	if tn == nil || tn.Pipe == nil {
		return false, nil
	}
	tf, ok := tn.Pipe.Find("toolfilter").(interface {
		Defers() bool
		Keep() []string
	})
	if !ok || !tf.Defers() {
		return false, nil
	}
	return true, tf.Keep()
}

// deferInfo carries what serve needs for the retry and the ledger.
type deferInfo struct {
	orig     []byte // the request before deferral (the client's own after the pipeline's rewrite)
	outcome  tooldefer.Outcome
	tenant   string
	model    string
	probe    tooldefer.ProbeKey
	original []byte // the client's pristine bytes, for weights
}

// applyToolDefer runs deferral on the final outgoing body. orig is the client's pristine body.
// Fail open: on any problem it returns body unchanged.
func (h *Handler) applyToolDefer(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, wire string,
	up upstream, session, threadID string, orig, body []byte) ([]byte, *deferInfo) {
	on, keep := deferOn(tn)
	if !on || wire != schema.WireAnthropic || provider != bschemas.Anthropic {
		return body, nil
	}
	ds := h.toolDefer()
	model := gjson.GetBytes(orig, "model").String()
	project := tooldefer.NormProject(apply.ProjectFor(provider, orig))
	// Every request feeds the history, including the ones deferral then declines.
	ds.hist.Observe(tn.ID, session, project, orig, time.Now().UnixMilli())
	key := tooldefer.ProbeKey{Upstream: up.base, Model: model, Route: up.path}
	o := tooldefer.Apply(tooldefer.Env{Store: tn.Store, History: ds.hist, Gate: ds.gate, Prober: ds.prober},
		tooldefer.Req{Tenant: tn.ID, Session: session, Thread: threadID, Project: project, Model: model,
			Orig: orig, Body: body, Probe: key, Keep: keep})
	if o.NeedProbe {
		h.startProbe(r, tn, provider, up, key)
	}
	if o.Decided || o.Changed {
		slog.Debug("cg.tooldefer", "session", session, "thread", threadID, "deferred", len(o.Deferred),
			"why", o.Why, "decided", o.Decided, "rebuilt", o.Rebuilt, "tokens", o.DeferredTokens)
	}
	if !o.Changed {
		return body, nil
	}
	return o.Body, &deferInfo{orig: body, outcome: o, tenant: tn.ID, model: model, probe: key, original: orig}
}

// startProbe runs a probe in the background, through the host's Dispatcher, with the caller's own
// endpoint and credential (the same resolution as the incoming model).
func (h *Handler) startProbe(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, up upstream, key tooldefer.ProbeKey) {
	m, ok := h.incomingModel(provider, up, []byte(`{"model":"x"}`), r).(cheapmodel.Anthropic)
	if !ok {
		return // no credential the proxy may spend: deferral stays off for this key
	}
	ref := m.Ref
	d := h.dispatch
	path := up.path
	h.toolDefer().prober.Ensure(key, func(ctx context.Context) (tooldefer.Verdict, error) {
		v, err := tooldefer.RunProbe(ctx, d, ref, path, key.Model)
		slog.Info("cg.tooldefer_probe", "model", key.Model, "route", path, "verdict", v.String(), "err", err)
		return v, err
	})
}

// deferRetry reports whether a response to a request we changed must be answered with the
// client's original request instead: a 400 or 422. It turns deferral off for this tenant and
// model and drops the probe verdict, so the next request probes again.
func (h *Handler) deferRetry(di *deferInfo, status int) bool {
	if di == nil || (status != http.StatusBadRequest && status != http.StatusUnprocessableEntity) {
		return false
	}
	ds := h.toolDefer()
	ds.gate.Break(di.tenant, di.model)
	ds.prober.Forget(di.probe)
	slog.Warn("cg.tooldefer_retry", "status", status, "model", di.model,
		"note", "the provider refused a request with deferred tools; sent the original and turned deferral off for this model")
	return true
}

// deferLedger books one finished request in the net-saving ledger. msg is the response message
// when the proxy parsed one, else nil (a stream-through response: its searches are not seen, and
// the request is booked with no search cost — the searches counter in /stats then reads low).
func (h *Handler) deferLedger(ctx context.Context, di *deferInfo, u Usage, usageOK bool, msg []byte) {
	if di == nil || !usageOK {
		return
	}
	r := tooldefer.Request{Model: di.model, DeferredTokens: di.outcome.DeferredTokens,
		ListTokens: di.outcome.ListTokens, Tier: tooldefer.TierOf(u.CacheRead, u.CacheWrite, u.CacheWrite1h)}
	n, loaded := u.Searches, u.SearchLoaded
	if msg != nil {
		n, loaded = tooldefer.SearchesIn(msg)
	}
	r.Searches = n
	r.LoadedTokens = tooldefer.LoadedWeight(di.original, loaded)
	var p modelinfo.Price
	if h.opts.Prices != nil {
		p, _ = h.opts.Prices.Price(ctx, di.model)
	}
	h.toolDefer().ledger.Add(r, p)
}

// noteToolSearch records the raw ENABLE_TOOL_SEARCH value a client's session-start hook sent.
func (h *Handler) noteToolSearch(scoped, raw string) {
	h.toolDefer().gate.NoteToolSearch(scoped, raw)
}

// deferStats is the /stats block. nil when deferral never ran in this process.
func (h *Handler) deferStats() any {
	ds := &h.defer_
	if ds.gate == nil {
		return nil
	}
	pass, fail := ds.prober.Runs()
	return map[string]any{
		"ledger":       ds.ledger.Snapshot(),
		"decisions":    ds.gate.Decisions.Load(),
		"rebuilds":     ds.gate.Rebuilds.Load(),
		"requests":     ds.gate.Deferred.Load(),
		"retries_400":  ds.gate.Retries.Load(),
		"probes_pass":  pass,
		"probes_fail":  fail,
		"history_live": ds.hist.Ready(),
	}
}
