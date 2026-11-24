package tooldefer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	cgschema "github.com/rossoctl/context-guru/schema"
	"github.com/tidwall/gjson"
)

// The flow of tool deferral around one request (#485). The host decodes the request, builds a
// Flow for it and calls these methods; the order of the steps and every decision is here, so a
// second host (Cortex) behaves the same. The host keeps the transport: the HTTP retry, the
// credential and the dispatch of a probe.

// Parts are the pieces of the host's deferral state. The host may build them lazily: Flow.Parts is
// called only when a request asks for deferral, so a proxy no tenant of which asks pays nothing.
type Parts struct {
	Env    Env
	Hist   *Mem
	Ledger *Ledger
}

// Info carries what the host needs for the retry and the ledger.
type Info struct {
	Orig     []byte // the request before deferral (the client's own after the pipeline's rewrite)
	Outcome  Outcome
	Tenant   string
	Model    string
	Probe    ProbeKey
	Original []byte // the client's pristine bytes, for weights (search: their Anthropic-shaped view)
	// Search is set when our own search tool (#825) changed the request: the host holds the
	// response and answers a call to it in band. Session, Thread and Project name the thread's record.
	Search                   bool
	Session, Thread, Project string
}

// Flow is one request's view of tool deferral.
type Flow struct {
	Tenant   string
	Pipe     *components.Pipeline // the tenant's pipeline: it says whether deferral is on
	Provider bschemas.ModelProvider
	Wire     string
	// Upstream and Route name the endpoint the request goes to. They key the probe verdicts.
	Upstream, Route string
	Parts           func() Parts
	// Prices prices a model for the ledger. nil means no price table.
	Prices modelinfo.Pricer
	// ProjectFor reads the project directory off the client's pristine body: the host passes
	// apply.ProjectFor. A field, not an import, because apply's own test imports this package.
	ProjectFor func(provider bschemas.ModelProvider, body []byte) string
	// TakeProbeAsk reports, once, whether the tenant asked for a probe (POST /tool-defer/probe).
	// nil means never.
	TakeProbeAsk func() bool
	// StartProbe runs a probe in the background. The host sends it through its Dispatcher, with the
	// credential of the request that is in flight.
	StartProbe func(key ProbeKey)
}

func (f Flow) project(p bschemas.ModelProvider, body []byte) string {
	if f.ProjectFor == nil {
		return ""
	}
	return f.ProjectFor(p, body)
}

func nowMs() int64 { return time.Now().UnixMilli() }

// Apply runs deferral on the final outgoing body. orig is the client's pristine body. Fail open:
// on any problem it returns body unchanged.
func (f Flow) Apply(session, threadID string, orig, body []byte) ([]byte, *Info) {
	if f.Provider == bschemas.OpenAI && ClientWire(f.Wire) {
		return f.applyClientSearch(session, threadID, orig, body)
	}
	if f.Wire != cgschema.WireAnthropic || f.Provider != bschemas.Anthropic {
		return body, nil
	}
	on, keep, always := PipeOn(f.Pipe)
	if f.TakeProbeAsk != nil && f.TakeProbeAsk() {
		// The preset picker asked for a probe. It runs now, in the background, with this request's
		// own upstream, model and credential: the proxy holds no credential between requests to
		// run it earlier.
		model := gjson.GetBytes(orig, "model").String()
		if AnthropicModel(model) {
			f.StartProbe(ProbeKey{Upstream: f.Upstream, Model: model, Route: f.Route})
		}
	}
	if !on {
		return body, nil
	}
	// A transcript that names a tool `tools` no longer declares (a search loaded it, then its MCP
	// server was removed before a resume) is a 400 at the provider, for the client's own body too.
	// A deferred stub keeps it valid without touching the cached prefix (#871, defect 1). Before
	// Apply, so the original a retry would send carries the stub as well.
	if fixed, added, ok := RepairRefs(body); ok {
		body = fixed
		slog.Debug("cg.tooldefer_stub", "session", session, "thread", threadID, "tools", len(added))
	}
	p := f.Parts()
	model := gjson.GetBytes(orig, "model").String()
	project := NormProject(f.project(f.Provider, orig))
	p.Hist.SeedTenant(f.Tenant) // this tenant only, once, in the background
	// Every request feeds the history, including the ones deferral then declines.
	p.Hist.Observe(f.Tenant, session, project, orig, nowMs())
	key := ProbeKey{Upstream: f.Upstream, Model: model, Route: f.Route}
	ckey := ProbeKey{Upstream: f.Upstream, Model: model, Route: f.Route + ClientProbeRoute}
	o := Apply(p.Env, Req{Tenant: f.Tenant, Session: session, Thread: threadID, Project: project,
		Model: model, Orig: orig, Body: body, Probe: key, ClientProbe: ckey, Keep: keep, AlwaysDefer: always})
	if o.Why == "no_durable_store" {
		noDurableOnce.Do(func() {
			slog.Warn("context-guru: ultrahigh defers no tools: no durable place for its per-thread record " +
				"(the session store is memory only and tool deferral's own file could not be opened). " +
				"A record lost at a restart would move tools inside live cached prefixes.")
		})
	}
	if o.NeedProbe {
		f.StartProbe(key)
	}
	if o.NeedClientProbe {
		f.StartProbe(ckey)
	}
	if o.ClientDisabled != "" {
		// No query text: the reason and a prefix of the description hash only.
		slog.Warn("cg.tooldefer_client_search_disabled", "reason", o.ClientDisabled, "desc_hash", o.DescHash[:min(8, len(o.DescHash))],
			"session", session, "thread", threadID)
	}
	if len(o.Loads) > 0 {
		// A ToolSearch answer written now loaded proxy-deferred tools (#898): each counts as a use.
		f.noteClientLoads(session, project, o.Loads)
	}
	if len(o.Direct) > 0 {
		// Called with no search: it worked, so it is a use. Counted per request that sees it.
		p.Hist.Observe(f.Tenant, session, project, LoadView(o.Direct), nowMs())
		p.Env.Gate.DirectCalls.Add(int64(len(o.Direct)))
	}
	if o.Decided || o.Changed {
		slog.Debug("cg.tooldefer", "session", session, "thread", threadID, "deferred", len(o.Deferred),
			"why", o.Why, "decided", o.Decided, "rebuilt", o.Rebuilt, "tokens", o.DeferredTokens)
	}
	if !o.Changed {
		return body, nil
	}
	return o.Body, &Info{Orig: body, Outcome: o, Tenant: f.Tenant, Model: model, Probe: key, Original: orig}
}

// noDurableOnce logs, once per process, why ultrahigh defers nothing.
var noDurableOnce sync.Once

// applyClientSearch runs our own tool search (#825) on a Chat Completions or Responses request, for
// a model that ignores defer_loading. Claude models are left to Apply's path. No probe: our search
// needs no provider feature, and a 400 to a changed request sends the original (Retry).
func (f Flow) applyClientSearch(session, threadID string, orig, body []byte) ([]byte, *Info) {
	on, keep, always := PipeOn(f.Pipe)
	if !on {
		return body, nil
	}
	p := f.Parts()
	model := gjson.GetBytes(orig, "model").String()
	project := NormProject(f.project(bschemas.OpenAI, orig))
	p.Hist.SeedTenant(f.Tenant)
	// The rule and the history read Anthropic-shaped bodies: both get the view.
	view := View(f.Wire, orig)
	if view != nil {
		p.Hist.Observe(f.Tenant, session, project, view, nowMs())
	}
	if !bytes.Contains(orig, []byte("tool_search_output")) || !NativeSearch(f.Wire, orig) {
		// not a client with its own search, or no load in the transcript
	} else if loads := NativeLoads(orig); len(loads) > 0 {
		// The client's own search loaded these: a load counts as a use, as for our search.
		p.Hist.Observe(f.Tenant, session, project, LoadView(loads), nowMs())
	}
	o, loaded := ApplySearch(p.Env, f.Wire, Req{Tenant: f.Tenant, Session: session,
		Thread: threadID, Project: project, Model: model, Orig: orig, Body: body, Keep: keep, AlwaysDefer: always})
	if len(loaded) > 0 {
		// Added back on the request side: a search the client received (with a client tool call),
		// or a tool tool_choice forces.
		f.noteSearchLoads(session, project, loaded)
	}
	if o.Decided || o.Changed {
		slog.Debug("cg.tooldefer_search", "session", session, "thread", threadID, "deferred", len(o.Deferred),
			"why", o.Why, "decided", o.Decided, "tokens", o.DeferredTokens, "added_back", len(loaded))
	}
	if !o.Changed {
		return body, nil
	}
	return o.Body, &Info{Orig: body, Outcome: o, Tenant: f.Tenant, Model: model, Original: view,
		Search: true, Session: session, Thread: threadID, Project: project}
}

// noteSearchLoads books tools our search added back: each counts as a use in the history, so the
// user's later sessions keep it loaded, and as an add-back on /stats.
func (f Flow) noteSearchLoads(session, project string, names []string) {
	p := f.Parts()
	p.Hist.Observe(f.Tenant, session, project, LoadView(names), nowMs())
	p.Env.Gate.AddBacks.Add(int64(len(names)))
}

// noteClientLoads books deferred tools that a ToolSearch answer loaded in client-search mode (#898):
// each counts as a use in the history, and as a client load on /stats (not an add-back).
func (f Flow) noteClientLoads(session, project string, names []string) {
	p := f.Parts()
	p.Hist.Observe(f.Tenant, session, project, LoadView(names), nowMs())
	p.Env.Gate.ClientLoads.Add(int64(len(names)))
}

// SearchLoad adds the named tools back to di's thread. It returns the thread's deferred set, for
// the answer, and the names added now, which the follow-up must declare.
func (f Flow) SearchLoad(di *Info, names []string) (set map[string]bool, now []string) {
	rs := f.Parts().Env.RecordStore()
	rec, ok := LoadSearch(rs, di.Session, di.Thread)
	if !ok {
		return nil, nil
	}
	set = make(map[string]bool, len(rec.Deferred))
	for _, n := range rec.Deferred {
		set[n] = true
	}
	now = AddBack(rs, di.Session, di.Thread, names)
	if len(now) > 0 {
		f.noteSearchLoads(di.Session, di.Project, now)
	}
	return set, now
}

// NoteSearchCalls keeps the searches the client received in di's thread record.
func (f Flow) NoteSearchCalls(di *Info, calls []SearchCall) {
	NoteCalls(f.Parts().Env.RecordStore(), di.Session, di.Thread, calls)
}

// NoteLeakedSearches keeps the calls to our search in a response the client gets whole (the round
// cap), so a later request can answer them (NoteCalls).
func (f Flow) NoteLeakedSearches(di *Info, wire string, sse bool, resp []byte) {
	if !bytes.Contains(resp, []byte(ClientSearchName)) {
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
	if calls, _ := SearchCalls(wire, msg); len(calls) > 0 {
		f.NoteSearchCalls(di, calls)
	}
}

// Retry books a 400 or 422 to a request we changed, after the client's original was sent in
// its place (so the client never sees our 400). originalOK is that original's outcome.
//
// The original succeeding where the deferred body was refused is the proof that deferral caused
// the refusal, whatever the provider's text: deferral is then turned off for this tenant and model,
// and the probe verdict dropped. Without that, an unrecognised message would send every later
// request twice. When the original fails too, the refusal is not ours: deferral stays on.
// DeferralCause only labels the log line.
func (f Flow) Retry(di *Info, status int, errBody []byte, originalOK bool) {
	cause := DeferralCause(errBody)
	p := f.Parts()
	if !originalOK {
		p.Env.Gate.Retries.Add(1)
		slog.Info("cg.tooldefer_retry", "status", status, "model", di.Model, "deferral_cause", cause, "original_ok", false)
		return
	}
	p.Env.Gate.Break(di.Tenant, di.Model)
	p.Env.Prober.Forget(di.Probe)
	slog.Warn("cg.tooldefer_retry", "status", status, "model", di.Model, "deferral_cause", cause, "original_ok", true,
		"note", "the provider refused a request with deferred tools and accepted the original; turned deferral off for this model")
}

// RepairFromError reads a refused response's error body. When it is a 400 that names a
// tool_reference to a tool body does not declare, it returns body with a deferred stub for that
// tool. The host checks the status and closes the response.
func RepairFromError(errBody, body []byte) (fixed []byte, ok bool) {
	name := MissingRef(errBody)
	if name != "" && !declares(body, name) {
		fixed, _, ok = StubTools(body, []string{name})
	}
	if !ok {
		return nil, false
	}
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

// Response is the part of one finished response that deferral's ledger reads.
type Response struct {
	CacheRead, CacheWrite, CacheWrite1h int64
	// Searches, SearchLoaded and SearchDropped are the server-side searches the stream-through path
	// read off its window.
	Searches      int
	SearchLoaded  []string
	SearchDropped int
}

// Booked is what the ledger booked for one request: the request facts, its price and how many
// different tools its searches loaded. The host puts the same numbers on its dashboard row (#869).
type Booked struct {
	Request  Request
	Money    Money
	Distinct int
}

// Book books one finished request in the net-saving ledger. The searches are the ones the
// stream-through path read off its window (u.Searches) plus the ones the host's loop read off each
// round it held (st): every path counts them, the SSE splice included (#871, defect 2). ok is false
// when nothing was booked.
func (f Flow) Book(ctx context.Context, di *Info, u Response, usageOK bool, st SearchTally) (b Booked, ok bool) {
	if di == nil || !usageOK {
		return Booked{}, false
	}
	r := Request{Tenant: di.Tenant, Model: di.Model, DeferredTokens: di.Outcome.DeferredTokens,
		ListTokens: di.Outcome.ListTokens, Tier: TierOf(u.CacheRead, u.CacheWrite, u.CacheWrite1h)}
	n := u.Searches + st.N
	loaded := append(append([]string(nil), u.SearchLoaded...), st.Loaded...)
	loaded = append(loaded, di.Outcome.Loads...) // client-search mode (#898): answers written on this request
	r.Searches = n
	r.DroppedSearches = u.SearchDropped + st.Dropped
	r.LoadedTokens = LoadedWeight(di.Original, loaded)
	r.WrongGuesses = WrongGuesses(di.Outcome.Deferred, di.Original, loaded)
	var p modelinfo.Price
	if f.Prices != nil {
		p, _ = f.Prices.Price(ctx, di.Model)
	}
	f.Parts().Ledger.Add(r, p)
	// The dashboard row takes the same r the ledger booked, so /stats and the panel agree (#869).
	m, _ := Price(r, p)
	return Booked{Request: r, Money: m, Distinct: distinctLoads(loaded)}, true
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

// SearchTally adds up the server-side searches of the rounds the host's loop held.
type SearchTally struct {
	N       int
	Dropped int // searches with no result
	Loaded  []string
}

// Add reads one round's response: an event stream, or a message object.
func (st *SearchTally) Add(sse bool, resp []byte) {
	if !bytes.Contains(resp, []byte(SearchName)) {
		return
	}
	var sr SearchRead
	if sse {
		sr = ReadSearchesSSE(resp)
	} else {
		sr = ReadSearches(resp)
	}
	st.N += sr.Searches
	st.Dropped += sr.Dropped()
	st.Loaded = append(st.Loaded, sr.Loaded...)
}

// noPricesOnce logs, once per process, why ultrahigh defers nothing.
var noPricesOnce sync.Once

// ProbeAllowed reports whether a probe may run for model. Only a model the price table knows: the
// name comes from the client, and a probe spends a credential. An unknown model is never probed,
// so it is never deferred.
func ProbeAllowed(ctx context.Context, prices modelinfo.Pricer, model string) bool {
	if prices == nil {
		noPricesOnce.Do(func() {
			slog.Warn("context-guru: ultrahigh defers no tools: the host has no price table, so no model is probed")
		})
		return false
	}
	p, ok := prices.Price(ctx, model)
	return ok && !p.Zero()
}

// ProbeKind names a probe key's kind for the log: the server-side search probe, or the client-search one.
func ProbeKind(k ProbeKey) string {
	if strings.HasSuffix(k.Route, ClientProbeRoute) {
		return "client"
	}
	return "server"
}

// EnsureProbe runs the probe for key in the background, once, through the host's Dispatcher, with
// the endpoint and credential the host resolved (ref) and the client's own Anthropic-Beta headers.
// path is the upstream path of the request.
func EnsureProbe(p *Prober, d dispatch.Dispatcher, ref dispatch.Ref, path string, key ProbeKey, betaHeaders []string) {
	// The client's own auth betas: a subscription token on direct Anthropic needs the OAuth beta.
	beta := AuthBetas(betaHeaders)
	run := RunProbeBeta
	if strings.HasSuffix(key.Route, ClientProbeRoute) {
		// Client-search mode (#898): the probe sends to the real route, with its own verdict.
		run, path = RunClientProbeBeta, strings.TrimSuffix(key.Route, ClientProbeRoute)
	}
	p.Ensure(key, func(ctx context.Context) (Verdict, error) {
		v, err := run(ctx, d, ref, path, key.Model, beta)
		slog.Info("cg.tooldefer_probe", "probe", ProbeKind(key), "model", key.Model, "route", path, "verdict", v.String(), "err", err)
		return v, err
	})
}
