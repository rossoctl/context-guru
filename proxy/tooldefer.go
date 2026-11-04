package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/expand"
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

// deferState is one handler's deferral state. Built at start when a configuration turns deferral
// on (startToolDefer), else at the first request that asks for it, so a proxy no tenant of which
// asks for deferral pays nothing. Once built it never changes; every field is safe for concurrent use.
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
	// closed is set by closeToolDefer: a late background build (deferTenantsChanged) then stops.
	closed bool
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
	return h.toolDeferLocked()
}

// toolDeferLocked is toolDefer with h.defer_.mu held.
func (h *Handler) toolDeferLocked() *deferState {
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

// startWait bounds how long New waits for the stored deferral state to load: the probe verdicts,
// the history and the seeds together, 10 s in all. The load is a few small tables; the bound only
// keeps a broken disk from holding the start.
const startWait = 10 * time.Second

// startToolDefer builds the deferral state at start, when this proxy's configuration, or any
// tenant's, turns deferral on, and waits (bounded) for the stored probe verdicts and history to
// load. Without it the state is built at the first ultrahigh request and loads in the background,
// so that request reads "probe unknown" and its whole session defers nothing (#871, defect 3).
// Start-up only: the request path never waits.
func (h *Handler) startToolDefer() {
	tenants := h.deferTenantsAtStart()
	if tenants == nil {
		return
	}
	ds := h.toolDefer()
	h.deferGate() // so /stats reports the state from the start
	// One bound for the load and the seeds together. A context, not a timer channel: a fired timer
	// delivers once, and the next wait on it would block for ever.
	ctx, cancel := context.WithTimeout(context.Background(), startWait)
	defer cancel()
	ok := waitAll(ctx, ds.hist.ReadyC()) // the seeds start after the load: a loaded session wins
	seeds := make([]<-chan struct{}, 0, len(tenants))
	for _, id := range tenants {
		seeds = append(seeds, ds.hist.SeedTenant(id))
	}
	ok = waitAll(ctx, seeds...) && ok
	slog.Info("context-guru: tool deferral state loaded at start", "tenants", len(tenants), "ready", ok)
}

// waitAll waits until every channel is closed or ctx ends. It reports whether all closed.
func waitAll(ctx context.Context, chans ...<-chan struct{}) bool {
	for _, c := range chans {
		select {
		case <-c:
			continue // closed already: done, even when ctx is done too
		default:
		}
		select {
		case <-c:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// deferTenantsChanged builds the deferral state, in the background, when an account change (a
// new configuration, a new account) turns deferral on for a tenant, and seeds that tenant's
// history. So a tenant that picks ultrahigh after the start gets the same eager load as one that
// had it at the start: its first request finds the stored verdicts. It never waits.
func (h *Handler) deferTenantsChanged() {
	go func() {
		tenants := h.deferTenantsAtStart()
		if tenants == nil {
			return
		}
		h.defer_.mu.Lock()
		if h.defer_.closed {
			h.defer_.mu.Unlock()
			return // the handler is closed: never build a state nobody will stop
		}
		ds := h.toolDeferLocked()
		h.defer_.mu.Unlock()
		h.deferGate()
		seedAfterLoad(ds.hist, tenants) // the seeds run on; nothing here waits for them
	}()
}

// seedAfterLoad waits for the history load that Open started, then seeds each tenant. The seed and
// the load both add only the sessions memory does not hold yet: a seed that landed first would hide
// the table's row for the same session, with its used flags. So the load goes first, as at the
// start. For a background goroutine only: it waits without a bound.
func seedAfterLoad(hist *tooldefer.Mem, tenants []string) []<-chan struct{} {
	<-hist.ReadyC()
	seeds := make([]<-chan struct{}, 0, len(tenants))
	for _, id := range tenants {
		seeds = append(seeds, hist.SeedTenant(id)) // once per tenant: a no-op for one seeded before
	}
	return seeds
}

// deferTenantsAtStart is the tenants whose configuration turns deferral on ("" for a
// single-tenant proxy), or nil when none does.
func (h *Handler) deferTenantsAtStart() []string {
	reg := h.registry()
	if reg == nil {
		if on, _, _ := deferOn(h.static.Load()); on {
			return []string{""}
		}
		return nil
	}
	ts, err := reg.List()
	if err != nil {
		slog.Warn("context-guru: tool deferral not loaded at start: the tenant list is unreadable", "err", err)
		return nil
	}
	var out []string
	for _, t := range ts {
		if !t.Disabled && docDefers(reg.Config(t)) {
			out = append(out, t.ID)
		}
	}
	return out
}

// docDefers reports whether a configuration document turns tool deferral on: its pipeline holds
// toolfilter with `defer: auto` (the ultrahigh preset, or a document that sets it).
func docDefers(doc string) bool {
	c, err := config.LoadBytes([]byte(doc))
	if err != nil {
		return false
	}
	has := false
	for _, n := range c.Pipeline {
		has = has || n == "toolfilter"
	}
	node, ok := c.Components["toolfilter"]
	if !has || !ok {
		return false
	}
	var tf struct {
		Defer string `yaml:"defer"`
	}
	return node.Decode(&tf) == nil && tf.Defer == "auto"
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

// noDurableOnce and noPricesOnce log, once per process, why ultrahigh defers nothing.
var noDurableOnce, noPricesOnce sync.Once

// purgeToolDefer forgets tenantID's tool deferral state: in memory (history, opt-outs, turn-offs,
// and the record and flag keys held in the process session store) and in tool deferral's own file.
// The dashboard purge deletes the dashboard database's rows.
//
// The file is purged even when this process has not opened it (no ultrahigh request since the
// start, or the session state off): a file that exists at Options.ToolDeferPath is opened for the
// purge only, and closed. A path with no file is skipped, so a purge never creates one.
func (h *Handler) purgeToolDefer(ctx context.Context, tenantID string) (int64, error) {
	if ds, g := h.deferPeek(); ds != nil || g != nil {
		if ds != nil {
			ds.hist.DropTenant(tenantID)
		}
		g.DropTenant(tenantID)
	}
	if mem, ok := h.store.(*store.Memory); ok {
		mem.DropPrefix(store.ToolDeferPrefix + tenantID + ":")
		mem.DropPrefix(store.ToolSearchOffPrefix + tenantID + ":")
	}
	h.defer_.mu.Lock()
	f := h.defer_.file
	h.defer_.mu.Unlock()
	if f != nil {
		f.Store.DropPrefix(store.ToolDeferPrefix + tenantID + ":")
		f.Store.DropPrefix(store.ToolSearchOffPrefix + tenantID + ":")
		return dash.PurgeToolDefer(ctx, f.DB, tenantID)
	}
	path := h.opts.ToolDeferPath
	if path == "" {
		return 0, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no file: nothing to purge, and none is created
		}
		return 0, err
	}
	db, err := tooldefer.OpenForPurge(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return dash.PurgeToolDefer(ctx, db, tenantID)
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
	h.defer_.closed = true
	h.defer_.mu.Unlock()
	if d != nil {
		d.stop()
	}
	if f != nil {
		f.Close()
	}
}

// deferOn reports whether this tenant's pipeline asks for deferral, its keep list and its
// always-defer list (#878).
func deferOn(tn *Tenancy) (on bool, keep, always []string) {
	if tn == nil || tn.Pipe == nil {
		return false, nil, nil
	}
	tf, ok := tn.Pipe.Find("toolfilter").(interface {
		Defers() bool
		Keep() []string
		AlwaysDefer() []string
	})
	if !ok || !tf.Defers() {
		return false, nil, nil
	}
	return true, tf.Keep(), tf.AlwaysDefer()
}

// deferInfo carries what serve needs for the retry and the ledger.
type deferInfo struct {
	orig     []byte // the request before deferral (the client's own after the pipeline's rewrite)
	outcome  tooldefer.Outcome
	tenant   string
	model    string
	probe    tooldefer.ProbeKey
	original []byte // the client's pristine bytes, for weights (search: their Anthropic-shaped view)
	// search is set when our own search tool (#825) changed the request: serve holds the response
	// and answers a call to it in band. session, thread and project name the thread's record.
	search                   bool
	session, thread, project string
}

// applyToolDefer runs deferral on the final outgoing body. orig is the client's pristine body.
// Fail open: on any problem it returns body unchanged.
func (h *Handler) applyToolDefer(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, wire string,
	up upstream, session, threadID string, orig, body []byte) ([]byte, *deferInfo) {
	if provider == bschemas.OpenAI && tooldefer.ClientWire(wire) {
		return h.applyClientSearch(tn, wire, session, threadID, orig, body)
	}
	if wire != schema.WireAnthropic || provider != bschemas.Anthropic {
		return body, nil
	}
	on, keep, always := deferOn(tn)
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
	// A transcript that names a tool `tools` no longer declares (a search loaded it, then its MCP
	// server was removed before a resume) is a 400 at the provider, for the client's own body too.
	// A deferred stub keeps it valid without touching the cached prefix (#871, defect 1). Before
	// Apply, so the original a retry would send carries the stub as well.
	if fixed, added, ok := tooldefer.RepairRefs(body); ok {
		body = fixed
		slog.Debug("cg.tooldefer_stub", "session", session, "thread", threadID, "tools", len(added))
	}
	ds := h.toolDefer()
	model := gjson.GetBytes(orig, "model").String()
	project := tooldefer.NormProject(apply.ProjectFor(provider, orig))
	ds.hist.SeedTenant(tn.ID) // this tenant only, once, in the background
	// Every request feeds the history, including the ones deferral then declines.
	ds.hist.Observe(tn.ID, session, project, orig, time.Now().UnixMilli())
	key := tooldefer.ProbeKey{Upstream: up.base, Model: model, Route: up.path}
	env := h.deferEnv(tn)
	ckey := tooldefer.ProbeKey{Upstream: up.base, Model: model, Route: up.path + tooldefer.ClientProbeRoute}
	o := tooldefer.Apply(env, tooldefer.Req{Tenant: tn.ID, Session: session, Thread: threadID, Project: project,
		Model: model, Orig: orig, Body: body, Probe: key, ClientProbe: ckey, Keep: keep, AlwaysDefer: always})
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
	if o.NeedClientProbe {
		h.startProbe(r, tn, provider, up, ckey)
	}
	if o.ClientDisabled != "" {
		// No query text: the reason and a prefix of the description hash only.
		slog.Warn("cg.tooldefer_client_search_disabled", "reason", o.ClientDisabled, "desc_hash", o.DescHash[:min(8, len(o.DescHash))],
			"session", session, "thread", threadID)
	}
	if len(o.Loads) > 0 {
		// A ToolSearch answer written now loaded proxy-deferred tools (#898): each counts as a use.
		h.noteSearchLoads(tn.ID, session, project, o.Loads)
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

// applyClientSearch runs our own tool search (#825) on a Chat Completions or Responses request, for
// a model that ignores defer_loading. Claude models are left to Apply's path. No probe: our search
// needs no provider feature, and a 400 to a changed request sends the original (deferRetry).
func (h *Handler) applyClientSearch(tn *Tenancy, wire, session, threadID string, orig, body []byte) ([]byte, *deferInfo) {
	on, keep, always := deferOn(tn)
	if !on {
		return body, nil
	}
	ds := h.toolDefer()
	model := gjson.GetBytes(orig, "model").String()
	project := tooldefer.NormProject(apply.ProjectFor(bschemas.OpenAI, orig))
	ds.hist.SeedTenant(tn.ID)
	// The rule and the history read Anthropic-shaped bodies: both get the view.
	view := tooldefer.View(wire, orig)
	if view != nil {
		ds.hist.Observe(tn.ID, session, project, view, time.Now().UnixMilli())
	}
	o, loaded := tooldefer.ApplySearch(h.deferEnv(tn), wire, tooldefer.Req{Tenant: tn.ID, Session: session,
		Thread: threadID, Project: project, Model: model, Orig: orig, Body: body, Keep: keep, AlwaysDefer: always})
	if len(loaded) > 0 {
		// Added back on the request side: a search the client received (with a client tool call),
		// or a tool tool_choice forces.
		h.noteSearchLoads(tn.ID, session, project, loaded)
	}
	if o.Decided || o.Changed {
		slog.Debug("cg.tooldefer_search", "session", session, "thread", threadID, "deferred", len(o.Deferred),
			"why", o.Why, "decided", o.Decided, "tokens", o.DeferredTokens, "added_back", len(loaded))
	}
	if !o.Changed {
		return body, nil
	}
	return o.Body, &deferInfo{orig: body, outcome: o, tenant: tn.ID, model: model, original: view,
		search: true, session: session, thread: threadID, project: project}
}

// noteSearchLoads books tools our search added back: each counts as a use in the history, so the
// user's later sessions keep it loaded, and as an add-back on /stats.
func (h *Handler) noteSearchLoads(tenant, session, project string, names []string) {
	ds := h.toolDefer()
	ds.hist.Observe(tenant, session, project, tooldefer.LoadView(names), time.Now().UnixMilli())
	h.deferGate().AddBacks.Add(int64(len(names)))
}

// searchLoad adds the named tools back to di's thread. It returns the thread's deferred set, for
// the answer, and the names added now, which the follow-up must declare.
func (h *Handler) searchLoad(tn *Tenancy, di *deferInfo, names []string) (set map[string]bool, now []string) {
	rs := h.deferEnv(tn).RecordStore()
	rec, ok := tooldefer.LoadSearch(rs, di.session, di.thread)
	if !ok {
		return nil, nil
	}
	set = make(map[string]bool, len(rec.Deferred))
	for _, n := range rec.Deferred {
		set[n] = true
	}
	now = tooldefer.AddBack(rs, di.session, di.thread, names)
	if len(now) > 0 {
		h.noteSearchLoads(di.tenant, di.session, di.project, now)
	}
	return set, now
}

// noteSearchCalls keeps the searches the client received in di's thread record.
func (h *Handler) noteSearchCalls(tn *Tenancy, di *deferInfo, calls []tooldefer.SearchCall) {
	tooldefer.NoteCalls(h.deferEnv(tn).RecordStore(), di.session, di.thread, calls)
}

// noteLeakedSearches keeps the calls to our search in a response the client gets whole (the round
// cap), so a later request can answer them (tooldefer.NoteCalls).
func (h *Handler) noteLeakedSearches(tn *Tenancy, di *deferInfo, wire string, sse bool, resp []byte) {
	if !bytes.Contains(resp, []byte(tooldefer.ClientSearchName)) {
		return
	}
	msg := resp
	if sse {
		agg, ok := expand.AggregateSSE(wire, resp)
		if !ok {
			return
		}
		msg = agg
	}
	if calls, _ := tooldefer.SearchCalls(wire, msg); len(calls) > 0 {
		h.noteSearchCalls(tn, di, calls)
	}
}

// startProbe runs a probe in the background, through the host's Dispatcher, with the caller's own
// endpoint and credential (the same resolution as the incoming model).
func (h *Handler) startProbe(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, up upstream, key tooldefer.ProbeKey) {
	// Only a model the price table knows: the name comes from the client, and a probe spends a
	// credential. An unknown model is never probed, so it is never deferred.
	if h.opts.Prices == nil {
		noPricesOnce.Do(func() {
			slog.Warn("context-guru: ultrahigh defers no tools: the host has no price table, so no model is probed")
		})
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
	// The client's own auth betas: a subscription token on direct Anthropic needs the OAuth beta.
	beta := tooldefer.AuthBetas(r.Header.Values("Anthropic-Beta"))
	run := tooldefer.RunProbeBeta
	if strings.HasSuffix(key.Route, tooldefer.ClientProbeRoute) {
		// Client-search mode (#898): the probe sends to the real route, with its own verdict.
		run, path = tooldefer.RunClientProbeBeta, strings.TrimSuffix(key.Route, tooldefer.ClientProbeRoute)
	}
	h.toolDefer().prober.Ensure(key, func(ctx context.Context) (tooldefer.Verdict, error) {
		v, err := run(ctx, d, ref, path, key.Model, beta)
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

// repairOnMissingRef reads a refused response. When it is a 400 that names a tool_reference to a
// tool body does not declare, it closes resp and returns body with a deferred stub for that tool.
// Otherwise resp is left readable, with the same bytes, and ok is false.
func repairOnMissingRef(resp *http.Response, body []byte) (fixed []byte, ok bool) {
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		return nil, false
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	name := tooldefer.MissingRef(errBody)
	if name != "" && !declares(body, name) {
		fixed, _, ok = tooldefer.StubTools(body, []string{name})
	}
	if !ok {
		// Put back what was read: LimitReader stops at 16 KiB, so the bytes read and the unread
		// rest of resp.Body are the original stream, in order.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(errBody), resp.Body), resp.Body}
		return nil, false
	}
	resp.Body.Close()
	slog.Info("cg.tooldefer_stub", "on", "retry")
	return fixed, true
}

// declares reports whether body's `tools` declares name.
func declares(body []byte, name string) bool {
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		found = t.Get("name").String() == name
		return !found
	})
	return found
}

// deferLedger books one finished request in the net-saving ledger. The searches are the ones the
// stream-through path read off its window (u.Searches) plus the ones serve's loop read off each
// round it held (st): every path counts them, the SSE splice included (#871, defect 2). The same
// facts go to the dashboard row (cp), for the per-account, per-day net-saving panel (#869).
func (h *Handler) deferLedger(ctx context.Context, cp *capture, di *deferInfo, u Usage, usageOK bool, st searchTally) {
	if di == nil || !usageOK {
		return
	}
	r := tooldefer.Request{Tenant: di.tenant, Model: di.model, DeferredTokens: di.outcome.DeferredTokens,
		ListTokens: di.outcome.ListTokens, Tier: tooldefer.TierOf(u.CacheRead, u.CacheWrite, u.CacheWrite1h)}
	n := u.Searches + st.n
	loaded := append(append([]string(nil), u.SearchLoaded...), st.loaded...)
	r.Searches = n
	r.DroppedSearches = u.SearchDropped + st.dropped
	r.LoadedTokens = tooldefer.LoadedWeight(di.original, loaded)
	r.WrongGuesses = tooldefer.WrongGuesses(di.outcome.Deferred, di.original, loaded)
	var p modelinfo.Price
	if h.opts.Prices != nil {
		p, _ = h.opts.Prices.Price(ctx, di.model)
	}
	h.toolDefer().ledger.Add(r, p)
	// The dashboard row takes the same r the ledger booked, so /stats and the panel agree (#869).
	m, _ := tooldefer.Price(r, p)
	cp.noteDefer(r, m, distinctLoads(loaded))
}

// distinctLoads is how many different tools the searches of one response loaded.
func distinctLoads(loaded []string) int {
	seen := map[string]bool{}
	for _, n := range loaded {
		if n != "" {
			seen[n] = true
		}
	}
	return len(seen)
}

// searchTally adds up the server-side searches of the rounds serve's loop held.
type searchTally struct {
	n       int
	dropped int // searches with no result
	loaded  []string
}

// add reads one round's response: an event stream, or a message object.
func (st *searchTally) add(sse bool, resp []byte) {
	if !bytes.Contains(resp, []byte(tooldefer.SearchName)) {
		return
	}
	var sr tooldefer.SearchRead
	if sse {
		sr = tooldefer.ReadSearchesSSE(resp)
	} else {
		sr = tooldefer.ReadSearches(resp)
	}
	st.n += sr.Searches
	st.dropped += sr.Dropped()
	st.loaded = append(st.loaded, sr.Loaded...)
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
	total := ds.ledger.Snapshot()
	return map[string]any{
		"ledger": total,
		// Per account ("" is a single-tenant proxy's one account), the same fields as ledger.
		"accounts": ds.ledger.Accounts(),
		// A deferred tool that a search then loaded, and a search with no result (sent together
		// with another tool call, so the model searched again). The same totals as ledger's.
		"wrong_guesses":    total.WrongGuesses,
		"dropped_searches": total.DroppedSearches,
		"decisions":        g.Decisions.Load(),
		"rebuilds":         g.Rebuilds.Load(),
		// Store misses sent as the original: the rule's set left out a tool the transcript proves.
		"rebuilds_unproven": g.Unproven.Load(),
		"requests":          g.Deferred.Load(),
		// Every 400/422 to a changed request that the proxy answered with the original, whatever
		// the cause; "turned_off" counts only those whose original succeeded, which turned it off.
		"retries_400": g.Retries.Load(),
		"turned_off":  g.TurnedOff.Load(),
		// Our own search (#825), counters of its own: tools it added back (one cache miss on `tools`
		// each; those a search loaded count in wrong_guesses too, through the ledger), and searches
		// made together with a client tool call, answered on the next request.
		"add_backs":      g.AddBacks.Load(),
		"mixed_searches": g.MixedSearches.Load(),
		// Client-search mode (#898) disabled, by reason. Each counts once per live mark (24 h per
		// tenant and ToolSearch description hash). The same numbers are cg_tooldefer_client_disabled_total.
		"client_search_disabled": map[string]int64{
			tooldefer.ReasonUnknownDescription: g.DisabledUnknown.Load(),
			tooldefer.ReasonNonSelectQuery:     g.DisabledNonSel.Load(),
			tooldefer.ReasonBreak:              g.DisabledBreak.Load(),
		},
		"probes_pass":  pass,
		"probes_fail":  fail,
		"history_live": ds.hist.Ready(),
	}
}
