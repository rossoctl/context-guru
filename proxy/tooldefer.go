package proxy

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
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

// docDefers reports whether a configuration document turns tool deferral on (tooldefer.DocOn).
func docDefers(doc string) bool { return tooldefer.DocOn(doc) }

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
func (h *Handler) deferEnv(tn *Tenancy) tooldefer.Env { return h.deferParts(tn).Env }

// deferParts is the deferral state for tn, for the library's flow. tn may be nil for a call that
// reads no record (the retry, the ledger). It builds the state, so it runs only for a request
// that asked for deferral.
func (h *Handler) deferParts(tn *Tenancy) tooldefer.Parts {
	ds := h.toolDefer()
	env := tooldefer.Env{Fallback: h.deferFallback(), History: ds.hist, Gate: h.deferGate(), Prober: ds.prober}
	if tn != nil {
		env.Store = tn.Store
	}
	return tooldefer.Parts{Env: env, Hist: ds.hist, Ledger: &ds.ledger}
}

// deferFlow is the library's flow of tool deferral for one request of tn.
func (h *Handler) deferFlow(tn *Tenancy) tooldefer.Flow {
	f := tooldefer.Flow{Parts: func() tooldefer.Parts { return h.deferParts(tn) }, ProjectFor: apply.ProjectFor}
	if tn != nil {
		f.Tenant, f.Pipe = tn.ID, tn.Pipe
	}
	return f
}

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
	if tn == nil {
		return false, nil, nil
	}
	return tooldefer.PipeOn(tn.Pipe)
}

// deferInfo carries what serve needs for the retry and the ledger.
type deferInfo = tooldefer.Info

// applyToolDefer runs deferral on the final outgoing body. orig is the client's pristine body.
// The flow is the library's (tooldefer.Flow.Apply); the host gives it the probe it sends.
func (h *Handler) applyToolDefer(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, wire string,
	up upstream, session, threadID string, orig, body []byte) ([]byte, *deferInfo) {
	f := h.deferFlow(tn)
	f.Provider, f.Wire, f.Upstream, f.Route = provider, wire, up.base, up.path
	f.TakeProbeAsk = func() bool { return h.takeProbeAsk(tn) }
	f.StartProbe = func(key tooldefer.ProbeKey) { h.startProbe(r, tn, provider, up, key) }
	return f.Apply(session, threadID, orig, body)
}

// searchLoad adds the named tools back to di's thread (tooldefer.Flow.SearchLoad).
func (h *Handler) searchLoad(tn *Tenancy, di *deferInfo, names []string) (set map[string]bool, now []string) {
	return h.deferFlow(tn).SearchLoad(di, names)
}

// noteSearchCalls keeps the searches the client received in di's thread record.
func (h *Handler) noteSearchCalls(tn *Tenancy, di *deferInfo, calls []tooldefer.SearchCall) {
	h.deferFlow(tn).NoteSearchCalls(di, calls)
}

// noteLeakedSearches keeps the calls to our search in a response the client gets whole.
func (h *Handler) noteLeakedSearches(tn *Tenancy, di *deferInfo, wire string, sse bool, resp []byte) {
	h.deferFlow(tn).NoteLeakedSearches(di, wire, sse, resp)
}

// startProbe runs a probe in the background, through the host's Dispatcher, with the caller's own
// endpoint and credential (the same resolution as the incoming model).
func (h *Handler) startProbe(r *http.Request, tn *Tenancy, provider bschemas.ModelProvider, up upstream, key tooldefer.ProbeKey) {
	if !tooldefer.ProbeAllowed(r.Context(), h.opts.Prices, key.Model) {
		return
	}
	m, ok := h.incomingModel(provider, up, []byte(`{"model":"x"}`), r).(cheapmodel.Anthropic)
	if !ok {
		return // no credential the proxy may spend: deferral stays off for this key
	}
	tooldefer.EnsureProbe(h.toolDefer().prober, h.dispatch, m.Ref, up.path, key, r.Header.Values("Anthropic-Beta"))
}

// deferRetry books a 400 or 422 to a request we changed (tooldefer.Flow.Retry).
func (h *Handler) deferRetry(di *deferInfo, status int, errBody []byte, originalOK bool) {
	h.deferFlow(nil).Retry(di, status, errBody, originalOK)
}

// deferLedger books one finished request in the net-saving ledger (tooldefer.Flow.Book), and the
// same facts on the dashboard row (cp).
func (h *Handler) deferLedger(ctx context.Context, cp *capture, di *deferInfo, u Usage, usageOK bool, st searchTally) {
	if di == nil || !usageOK {
		return
	}
	b, ok := h.deferFlow(nil).Book(ctx, di, tooldefer.Response{FreshInput: u.FreshInput, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		CacheWrite1h: u.CacheWrite1h, Searches: u.Searches, SearchLoaded: u.SearchLoaded, SearchDropped: u.SearchDropped}, usageOK, st)
	if !ok {
		return
	}
	// cp is the capture the accountant runs on: the dashboard's, or a lean one with the dashboard off (#1451). The
	// dollars are the accountant's one figure for tooldefer, on both paths (#1441). The facts go first and stay
	// synchronous: the search turns are spend operations that the accountant reads when it prices the request. The
	// dollars wait for the accountant, so they are booked off the handler path (capture.runDeferBook).
	cp.noteDefer(b.Request, b.Distinct)
	ledger := h.deferFlow(nil).Parts().Ledger
	tenant := b.Request.Tenant
	if cp == nil {
		ledger.AddMoney(tenant, tooldefer.Money{}, false)
		return
	}
	cp.deferBook = func() {
		// Fail open: finish runs this in the handler's defer on the dashboard path, and accCompute is outside
		// accFigure's own recover. A panic costs the figure only: the request books as unpriced.
		defer func() {
			if p := recover(); p != nil {
				slog.Error("tool deferral: booking the ledger panicked; the request books no figure", "panic", p)
				ledger.AddMoney(tenant, tooldefer.Money{}, false)
			}
		}()
		fig, priced := cp.accFigure(context.Background(), u, usageOK)
		if res, have := cp.accCompute(u, usageOK); !priced || !have || accrec.Held(res) {
			ledger.AddMoney(tenant, tooldefer.Money{}, false)
			return
		}
		ledger.AddMoney(tenant, accrec.DeferMoney(fig), true)
	}
}

// searchTally adds up the server-side searches of the rounds serve's loop held.
type searchTally = tooldefer.SearchTally

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
		"add_backs": g.AddBacks.Load(),
		// Client-search mode (#898): deferred tools a ToolSearch answer loaded, and deferred tools
		// the model called with no search (the API accepts it; a use, not a break).
		"client_loads":   g.ClientLoads.Load(),
		"direct_calls":   g.DirectCalls.Load(),
		"mixed_searches": g.MixedSearches.Load(),
		// Client-search mode (#898) disabled, by reason. unknown_description counts
		// once per tenant and hash per 24 h; break counts every time. The same numbers are cg_tooldefer_client_disabled_total.
		"client_search_disabled": map[string]int64{
			tooldefer.ReasonUnknownDescription: g.DisabledUnknown.Load(),
			tooldefer.ReasonBreak:              g.DisabledBreak.Load(),
		},
		"probes_pass":  pass,
		"probes_fail":  fail,
		"history_live": ds.hist.Ready(),
	}
}
