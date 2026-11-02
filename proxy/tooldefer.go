package proxy

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/tidwall/gjson"
)

// The proxy's half of tool deferral (#485). The decision and the rewrite are package tooldefer's;
// this file only wires them: which requests ask for it (the tenant's toolfilter `defer: auto`, which
// only the ultrahigh preset sets), the probe through the host's Dispatcher, the retry of the original
// request after a 400, the ENABLE_TOOL_SEARCH flag from /tool-search, and the /stats ledger.

// deferState is one handler's deferral state. Built lazily, so a proxy no tenant of which asks for
// deferral pays nothing. Once built it never changes; every field is safe for concurrent use.
type deferState struct {
	hist   *tooldefer.Mem
	stop   func()
	prober *tooldefer.Prober
	ledger tooldefer.Ledger
}

// deferHost holds the state behind a lock, so the lazy build, /stats and Close never see it half
// built. The gate is not lazy: /signal writes it on any proxy, and building the whole state (the
// database open, the history load) for a flag no tenant may ever read would be waste.
type deferHost struct {
	mu    sync.Mutex
	state *deferState
	gate  *tooldefer.Gate
	// file is tool deferral's own file, opened with the state when needed; nil when it is not.
	file *tooldefer.File
	// asks holds the tenants whose preset picker asked for a probe, until their next request.
	asks map[string]bool
}

// maxProbeAsks bounds asks: one entry per tenant that asked and has not sent a request since.
const maxProbeAsks = 10000

// askProbe records that tenant wants a probe at its next request. It returns at once.
func (h *Handler) askProbe(tenant string) {
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	if h.defer_.asks == nil {
		h.defer_.asks = map[string]bool{}
	}
	if len(h.defer_.asks) < maxProbeAsks {
		h.defer_.asks[tenant] = true
	}
}

// takeProbeAsk reports, once, whether tenant asked for a probe.
func (h *Handler) takeProbeAsk(tn *Tenancy) bool {
	if tn == nil {
		return false
	}
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	if !h.defer_.asks[tn.ID] {
		return false
	}
	delete(h.defer_.asks, tn.ID)
	return true
}

// deferGate is the off-switch gate, built on first use under the same lock.
func (h *Handler) deferGate() *tooldefer.Gate {
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	if h.defer_.gate == nil {
		h.defer_.gate = tooldefer.NewGate()
	}
	return h.defer_.gate
}

// toolDefer returns the handler's deferral state, building it on first use.
//
// The history and the probe verdicts live in the dashboard database when there is one, else in
// tool deferral's own file (Options.ToolDeferFile), else in memory only. The seed reads the
// dashboard database, per tenant (SeedTenant).
func (h *Handler) toolDefer() *deferState {
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	if d := h.defer_.state; d != nil {
		return d
	}
	var dashDB *sql.DB
	if h.rec != nil {
		if db := h.rec.DB(); db != nil && db.Path() != "" && db.Path() != ":memory:" {
			dashDB = db.SQL()
		}
	}
	// The file is needed when the session store is not durable, or there is no dashboard database
	// for the history. Without a path, or when it cannot be opened, it stays nil.
	if (!store.Durable(h.store) || dashDB == nil) && h.opts.ToolDeferPath != "" {
		ret := h.opts.ToolDeferRetention
		if ret <= 0 {
			ret = store.DefaultDiskRetention
		}
		if f, err := tooldefer.OpenFile(h.opts.ToolDeferPath, ret); err != nil {
			slog.Warn("context-guru: tool deferral's file could not be opened", "path", h.opts.ToolDeferPath, "err", err)
		} else {
			h.defer_.file = f
		}
	}
	histDB := dashDB
	if histDB == nil && h.defer_.file != nil {
		histDB = h.defer_.file.DB
	}
	d := &deferState{hist: tooldefer.NewMem(histDB)}
	d.hist.SeedDB = dashDB
	d.hist.Retention = h.opts.ToolDeferRetention
	if d.hist.Retention <= 0 {
		d.hist.Retention = store.DefaultDiskRetention
	}
	d.prober = tooldefer.NewProber()
	// The verdicts survive a restart with the history, so a thread decided before it keeps its set.
	d.prober.Persist = d.hist.SaveVerdict
	d.stop = d.hist.Open(0, d.prober)
	h.defer_.state = d
	return d
}

// deferFallback is the record store for a tenant whose session store is not durable.
func (h *Handler) deferFallback() store.Store {
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	if f := h.defer_.file; f != nil {
		return f.Store
	}
	return nil
}

// deferEnv is the deferral environment for tn.
func (h *Handler) deferEnv(tn *Tenancy) tooldefer.Env {
	ds := h.toolDefer()
	return tooldefer.Env{Store: tn.Store, Fallback: h.deferFallback(), History: ds.hist, Gate: h.deferGate(), Prober: ds.prober}
}

// noDurableOnce logs, once per process, that ultrahigh defers nothing for want of a durable place.
var noDurableOnce sync.Once

// purgeToolDefer forgets tenantID's tool deferral state: in memory (history, opt-outs, turn-offs)
// and in tool deferral's own file. The dashboard purge deletes the dashboard database's rows.
func (h *Handler) purgeToolDefer(ctx context.Context, tenantID string) (int64, error) {
	if ds, g := h.deferPeek(); ds != nil || g != nil {
		if ds != nil {
			ds.hist.DropTenant(tenantID)
		}
		g.DropTenant(tenantID)
	}
	h.defer_.mu.Lock()
	f := h.defer_.file
	h.defer_.mu.Unlock()
	if f != nil {
		f.Store.DropPrefix(store.ToolDeferPrefix + tenantID + ":")
		f.Store.DropPrefix(store.ToolSearchOffPrefix + tenantID + ":")
		return dash.PurgeToolDefer(ctx, f.DB, tenantID)
	}
	return 0, nil
}

// deferPeek returns the state and the gate when they exist, without building anything.
func (h *Handler) deferPeek() (*deferState, *tooldefer.Gate) {
	h.defer_.mu.Lock()
	defer h.defer_.mu.Unlock()
	return h.defer_.state, h.defer_.gate
}

// closeToolDefer flushes the history, then closes tool deferral's file, once.
func (h *Handler) closeToolDefer() {
	h.defer_.mu.Lock()
	d, f := h.defer_.state, h.defer_.file
	h.defer_.file = nil
	h.defer_.mu.Unlock()
	if d != nil {
		d.stop()
	}
	if f != nil {
		f.Close()
	}
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
	if wire != schema.WireAnthropic || provider != bschemas.Anthropic {
		return body, nil
	}
	on, keep := deferOn(tn)
	if h.takeProbeAsk(tn) {
		// The preset picker asked for a probe (POST /tool-defer/probe). It runs now, in the
		// background, with this request's own upstream, model and credential: the proxy holds no
		// credential between requests to run it earlier.
		model := gjson.GetBytes(orig, "model").String()
		if tooldefer.AnthropicModel(model) {
			h.startProbe(r, tn, provider, up, tooldefer.ProbeKey{Upstream: up.base, Model: model, Route: up.path})
		}
	}
	if !on {
		return body, nil
	}
	ds := h.toolDefer()
	model := gjson.GetBytes(orig, "model").String()
	project := tooldefer.NormProject(apply.ProjectFor(provider, orig))
	ds.hist.SeedTenant(tn.ID) // this tenant only, once, in the background
	// Every request feeds the history, including the ones deferral then declines.
	ds.hist.Observe(tn.ID, session, project, orig, time.Now().UnixMilli())
	key := tooldefer.ProbeKey{Upstream: up.base, Model: model, Route: up.path}
	env := h.deferEnv(tn)
	o := tooldefer.Apply(env, tooldefer.Req{Tenant: tn.ID, Session: session, Thread: threadID, Project: project,
		Model: model, Orig: orig, Body: body, Probe: key, Keep: keep})
	if o.Why == "no_durable_store" {
		noDurableOnce.Do(func() {
			slog.Warn("context-guru: ultrahigh defers no tools: no durable place for its per-thread record " +
				"(the session store is memory only and tool deferral's own file could not be opened). " +
				"A record lost at a restart would move tools inside live cached prefixes.")
		})
	}
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
	// Only a model the price table knows: the name comes from the client, and a probe spends a
	// credential. An unknown model is never probed, so it is never deferred.
	if h.opts.Prices == nil {
		return
	}
	if p, ok := h.opts.Prices.Price(r.Context(), key.Model); !ok || p.Zero() {
		return
	}
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

// deferRetry books a 400 or 422 to a request we changed, after the client's original was sent in
// its place (so the client never sees our 400). originalOK is that original's outcome.
//
// The original succeeding where the deferred body was refused is the proof that deferral caused
// the refusal, whatever the provider's text: deferral is then turned off for this tenant and model,
// and the probe verdict dropped. Without that, an unrecognised message would send every later
// request twice. When the original fails too, the refusal is not ours: deferral stays on.
// tooldefer.DeferralCause only labels the log line.
func (h *Handler) deferRetry(di *deferInfo, status int, errBody []byte, originalOK bool) {
	cause := tooldefer.DeferralCause(errBody)
	if !originalOK {
		h.deferGate().Retries.Add(1)
		slog.Info("cg.tooldefer_retry", "status", status, "model", di.model, "deferral_cause", cause, "original_ok", false)
		return
	}
	h.deferGate().Break(di.tenant, di.model)
	h.toolDefer().prober.Forget(di.probe)
	slog.Warn("cg.tooldefer_retry", "status", status, "model", di.model, "deferral_cause", cause, "original_ok", true,
		"note", "the provider refused a request with deferred tools and accepted the original; turned deferral off for this model")
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

// noteToolSearch records the raw ENABLE_TOOL_SEARCH value a client's session-start hook sent, for
// tn's session scoped. A session the proxy has seen (its history knows it) gets the flag in its
// record store at once; any other waits, per tenant, for the session's first request. It never
// builds the deferral state.
func (h *Handler) noteToolSearch(tn *Tenancy, scoped, raw string) {
	ds, _ := h.deferPeek()
	seen := ds != nil && ds.hist.Has(tn.ID, scoped)
	var rs store.Store
	if seen {
		rs = h.deferEnv(tn).RecordStore()
	}
	h.deferGate().NoteToolSearch(tn.ID, scoped, raw, rs, seen && rs != nil)
}

// deferStats is the /stats block. nil when deferral never ran in this process.
func (h *Handler) deferStats() any {
	ds, g := h.deferPeek()
	if ds == nil || g == nil {
		return nil
	}
	pass, fail := ds.prober.Runs()
	return map[string]any{
		"ledger":    ds.ledger.Snapshot(),
		"decisions": g.Decisions.Load(),
		"rebuilds":  g.Rebuilds.Load(),
		"requests":  g.Deferred.Load(),
		// Every 400/422 to a changed request that the proxy answered with the original, whatever
		// the cause; "turned_off" counts only those whose original succeeded, which turned it off.
		"retries_400":  g.Retries.Load(),
		"turned_off":   g.TurnedOff.Load(),
		"probes_pass":  pass,
		"probes_fail":  fail,
		"history_live": ds.hist.Ready(),
	}
}
