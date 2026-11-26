package proxy

import (
	"context"
	"github.com/rossoctl/context-guru/agent"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/compactionpoint"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/rossoctl/context-guru/reqfacts"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/thread"
	"github.com/rossoctl/context-guru/tooldefer"
	"github.com/rossoctl/context-guru/waitcause"
)

// The dashboard's capture point. It is a plain struct built from values the
// request path has already computed, handed to a channel with a `default:` branch.
// No I/O, no lock held across a call, no token re-counting: the entire cost on the
// request goroutine is a few field copies and one non-blocking send, which is why
// enabling the dashboard does not show up in request latency (see docs/dashboard.md
// for the measurement).
//
// Everything expensive — redaction of captured content, gzip, the insert, the SSE
// fan-out — happens on the writer goroutine, after the response has been sent.

// spendHook is a TEST-ONLY hook. It is nil in production, and then each timed call costs one atomic
// load and a deferred no-op. A test sets it to learn how long the dashboard's own calls ran on the
// request goroutine, which is the dashboard's cost with the runner's speed taken out
// (TestDashboardAddsNoRequestLatencyWithContentCapture). Same idea as the nil test hook in extract_llm.go.
var spendHook atomic.Pointer[func(call string, d time.Duration)]

func noopSpan() {}

// timedSpan starts timing one dashboard call. Defer the func it returns.
func timedSpan(call string) func() {
	hook := spendHook.Load()
	if hook == nil {
		return noopSpan
	}
	start := time.Now()
	return func() { (*hook)(call, time.Since(start)) }
}

// timed is timedSpan for a capture method. A nil capture (dashboard off) is never timed.
func (c *capture) timed(call string) func() {
	if c == nil {
		return noopSpan
	}
	return timedSpan(call)
}

// capture is the per-request scratchpad the chat handler fills as it goes.
type capture struct {
	rec       *dash.Recorder
	pricer    modelinfo.Pricer
	preset    string
	route     string
	provider  string
	model     string
	agent     agent.Client
	project   string
	start     time.Time
	cgMs      float64
	upstream  float64
	status    int
	expands   int
	expandTok int
	// The expands that restored a sweep marker, and what they returned. A subset of the two above.
	sweepExpands   int
	sweepExpandTok int
	// sweepFalseExp counts first expands of a sweep drop that no earlier signal had already counted.
	sweepFalseExp int
	// sweepExpandRules counts first expands of a sweep v3 drop by what chose it: a rule name, or "judge"
	// (#1191). finish adds them to the sweep's component row as sweep_v3_expanded_<rule> events.
	sweepExpandRules map[string]int
	// deferFacts is tool deferral's ledger entry for this request (noteDefer), nil when deferral
	// did not change it.
	deferFacts *deferFacts
	// sweepFalseExpWarm is the part of sweepFalseExp whose drop was applied on a warm request.
	sweepFalseExpWarm int
	// The response-side streaming facts, set by observeSSE from proxy.go's terminal defer.
	sseTTFBMs   float64
	sseBuffered bool
	trace       apply.Trace
	// llm is THIS request's own cheap-model usage, accumulated by the compaction
	// clients running under the request's context (see llmCtx). It replaces a delta of
	// the process-global counters, which charged a row for whatever any OTHER tenant
	// spent while the row's request happened to be in flight.
	llm    *cheapmodel.Sink
	unique map[string]int
	// tenant owns this row. It has to be here rather than read at finish time
	// because the unique-savings attribution during noteTrace already needs it, and
	// that runs on the request path.
	tenant string
	// meta is the request's own metadata, read once off the pristine inbound body.
	meta dash.Meta
	// predArrival is set by the handler when a cache campaign tracks this tenant (see LiveFacts.Arrival).
	predArrival *predictive.Arrival
	// thread is the session thread this request belongs to (see package thread), set by
	// noteThread once the session id is known. "" is the primary thread. inherits/hasInherit
	// name the thread a NEW thread continues by content (thread.Result.Inherits).
	thread    string
	agentComp bool
	// compVerdict is the client compaction verdict of this request (#947), set by noteThread;
	// compKnown says the tracker judged the request, so "" in compVerdict means none rather than unknown.
	compVerdict string
	compKnown   bool
	// cpoint and cpointSrc are C for this request and where it came from (#948).
	cpoint     int
	cpointSrc  string
	inherits   string
	hasInherit bool
	// limits are the subscription limit headers of the upstream answer (#984; noteLimits).
	limits *dash.Limits
	// authSub is true when the client sent a Claude subscription (OAuth) token (#984). Only the
	// kind is kept, never the token.
	authSub bool
	// Issue #424's RECORD half. waits is the handler's shared tracker (nil-checked like
	// everything else here); waitBody is the PRISTINE request body, stashed by noteWaitBody —
	// a plain slice-header copy, no scan. finish() builds a CLOSURE around both of these
	// (plus the response's own facts) and hands it to the dash.Event as Event.WaitCause,
	// which the dashboard's WRITER goroutine invokes — never finish() itself, and never
	// anything on the request path: the actual rescan (waitcause.Tracker.Observe, ~11ms on
	// a 783 KB body) running inside finish() was measured adding exactly that much latency
	// to the NEXT request on a keep-alive connection, because finish() runs on the
	// handler's own defer, before the handler returns (same reason dash.Event.Redact runs
	// on the writer goroutine, not here). See dash/capture.go's write().
	waits    *waitcause.Tracker
	waitBody []byte
	// early is this request's early wait-cause scan (earlyscan.go); nil: none was started. scanStat is the
	// handler's counters.
	early    *earlyScan
	scanStat *scanStats
	// terminal is Options.TerminalTools (#857), read by the wait-cause closure.
	terminal []string
	// The alias derivation's other inputs (#766): the pipeline that will transform this
	// body, the wire dialect, and the bypass flag — everything apply.EnforcedSessionIDFor
	// needs to derive the id the pipeline's own turn will ask under. Stashed here because
	// the derivation runs on the WRITER goroutine, in finish()'s AliasFn closure. Set by
	// noteAliasSource; a capture it was never called on derives off the raw body (nil pipe
	// transforms nothing), the pre-round-4 behavior.
	aliasPipe   *components.Pipeline
	aliasAPI    string
	aliasBypass bool
	// kaPings / kaRefreshed / kaStrategy are what the idle keep-alive did during the span
	// this request just ended: how many pings it sent, how many tokens the last of them
	// read from cache, and which manager-controlled strategy (if any) resolved the policy
	// that sent them. Recorded on the request path because the keeper's per-session state
	// is cleared the moment a real request arrives, so this is the only point at which all
	// three facts are still in hand.
	kaPings     int
	kaRefreshed int64
	kaStrategy  string
	// kaPlan is the ping plan the keeper built for the wait this response starts (#1198); nil when none
	// was built. hosted is true on a multi-tenant proxy, whose host clock says nothing about the user.
	kaPlan *keepalive.PlanInForce
	hosted bool
	// kaRefreshedAt is when the provider's entry was last known refreshed (see kaArrival).
	kaRefreshedAt time.Time
	// kaRefreshedOneHour: the entry's last touch was a live free 1-hour ping (kaArrival).
	kaRefreshedOneHour bool
	// inv is the request's DECLARED inventory (tool / MCP / skill names and their token
	// weights) plus the tool calls of its last tool-using turn — nil when the request
	// declares no tools, and nil when the dashboard is off. Read on the request path with
	// the pristine body, like meta; emitted at finish, where the session id is known.
	inv *dash.Inventory
	// savings is the handler's accountant tracker (nil only on a handler built without one); accReq is this
	// request's record (nil when it was skipped); accDone guards a double finish.
	savings *savingsTracker
	// metrics receives each priced request for the push metrics (Options.MetricsSink; nil = off).
	metrics acmetrics.Observer
	// inference is the common inference set (Options.InferenceMetrics; nil = off).
	inference *InferenceMetrics
	// observe is true when the tenancy runs in observe mode: the trace is then empty by design
	// and the accountant figure must not be booked.
	observe bool
	// pings holds the keep-alive ping facts the next request consumes (Options.Pings; nil = none).
	pings *PingStore
	// endpoint and readSets tell the accountant whether a read sets the TTL on this route
	// (Provider.ReadSetsTTL). readSets is the keeper's readSetsTTL; nil = never.
	endpoint string
	readSets func(provider bschemas.ModelProvider, endpoint, route, model string, pinged bool) bool
	accReq   *accountant.Request
	// accRes is the accountant's Result for this request, set once by accCompute (nil when the request books no figure).
	accRes *accountant.Result
	// accMu guards accCompute on a lean capture, where the ledger's hook and the finish goroutine can both call it. nil otherwise.
	accMu *sync.Mutex
	// accServerHeldHistory is set for an OpenAI Responses request with server-held history (previous_response_id, conversation, item_reference): the provider
	// holds its history, so the accountant has no record of it.
	accServerHeldHistory bool
	// accNoResult says why no Result was attached when Compute was reached and gave none.
	accNoResult string
	// accClient is the body the agent sent, kept for accEnd's envelope diff (envelopeOps).
	accClient []byte
	// accMeta and accOpen are what accBegin noted for accEnd to hand to the accountant's job.
	accMeta accountant.RequestMeta
	accOpen bool
	// accJob is the accountant's work in flight (nil before accEnd and after accWait).
	accJob *accJob
	// accPreDefer is the body tool deferral read, set only on a request it changed (#908). It splits
	// the envelope diff in two, so deferral's edits to `tools` are reported under its own operator.
	accPreDefer []byte
	// accThread is the thread id accEnd gave the library, the second half of the ping store's key.
	accThread string
	// accFacts is the entry facts accEnd gave the library, kept for accBilledTTL: the library keeps its record private.
	accFacts accountant.EntryFacts
	// accPingWindow is the window of the ping facts accEnd read, so finish drops those and not a
	// newer ping that arrived while the request was in flight.
	accPingWindow string
	accDone       bool
	// rounds is the number of upstream rounds of the expand loop (noteRounds). The usage is the
	// LAST round's, billed on a body the expand loop grew, while the savings record holds the
	// first round's body: with rounds > 1 the library's bytes-to-tokens ratio would be wrong, so
	// the record is not computed.
	rounds int
}

// newCapture starts a capture for one request, or returns nil when the dashboard
// is off — every call site is nil-safe, so the disabled path costs one nil check.
func (h *Handler) newCapture(r *http.Request, cl agent.Client, provider, route string, tn *Tenancy) *capture {
	defer timedSpan("newCapture")()
	if h.rec == nil {
		return nil
	}
	// The preset comes from the TENANCY, not from Options: in a hosted deployment
	// the configuration in effect is the tenant's, and labelling every row with the
	// server default would make the dashboard's preset comparison a lie. That is the
	// DEFAULT, not the last word: /compact lets one request swap the pipeline, and a
	// row labelled with the tenant default while another preset ran is the same lie a
	// level down — so the handler overrides this label via notePreset once the
	// override has resolved.
	return &capture{
		rec: h.rec, pricer: h.opts.Prices, preset: tn.Preset, tenant: tn.ID,
		route: route, provider: provider,
		agent: cl, start: time.Now(),
		llm:      &cheapmodel.Sink{},
		unique:   map[string]int{},
		waits:    h.waits,
		terminal: h.opts.TerminalTools,
		savings:  h.savings, pings: h.opts.Pings, metrics: h.opts.MetricsSink, inference: h.opts.InferenceMetrics,
		observe: tn.Mode == components.ModeObserve,
		authSub: provider == string(bschemas.Anthropic) && callerIsSubscription(r),
		hosted:  h.opts.Tenants != nil,
	}
}

// noteKeepAlivePlan records the ping plan the keeper built for the wait this response starts (#1198).
func (c *capture) noteKeepAlivePlan(p *keepalive.PlanInForce) {
	defer c.timed("noteKeepAlivePlan")()
	if c != nil {
		c.kaPlan = p
	}
}

// notePreset relabels the row with the pipeline this ONE request ran, for /compact's
// per-request override (?preset=<name>, or x-context-guru-pipeline with an explicit
// component list, which is labelled "custom" because it has no preset name). The
// dashboard's preset facet is the whole point of an eval sweep across presets, and it can
// only compare configurations if each row names the one that actually ran.
//
// An empty name means no override took effect, so the tenancy's default — the label
// newCapture set — is the honest one and stays.
func (c *capture) notePreset(name string) {
	defer c.timed("notePreset")()
	if c == nil || name == "" {
		return
	}
	c.preset = name
}

// llmCtx scopes context-guru's own cheap-model accounting to THIS request. Every
// compaction call made under the returned context — the pipeline's, and internal/extract's
// underneath it — lands on this row and on no other. Wrap the context once, where it
// enters the pipeline; the process totals /stats reports are unaffected.
//
// Nil-safe: with the dashboard off there is nothing to attribute and the context is
// returned unchanged.
func (c *capture) llmCtx(ctx context.Context) context.Context {
	defer c.timed("llmCtx")()
	if c == nil {
		return ctx
	}
	return cheapmodel.WithSink(ctx, c.llm)
}

// noteTrace records the pipeline's outcome and computes each component's
// unique-savings share. The dedup map lives in the recorder (process-wide), which
// is what makes "unique" mean the same thing here and in /stats.
func (c *capture) noteTrace(tr apply.Trace) {
	defer c.timed("noteTrace")()
	if c == nil {
		return
	}
	c.trace = tr
	if tr.Run == nil || c.rec == nil { // no dashboard: nothing marks a saving unique
		return
	}
	for _, rep := range tr.Run.Components {
		if saved := rep.Saved(); saved > 0 && !rep.Reverted && !rep.Skipped {
			c.unique[rep.Component] = c.rec.MarkUnique(c.tenant, rep.Component, rep.CacheKeys, saved)
		}
	}
}

// noteAgentCompaction records that this request is the agent's own compaction request.
func (c *capture) noteAgentCompaction(v bool) {
	defer c.timed("noteAgentCompaction")()
	if c != nil {
		c.agentComp = v
	}
}

// noteReadSets gives the capture the upstream and the keeper's proof, for Provider.ReadSetsTTL. Nil-safe.
func (c *capture) noteReadSets(endpoint string, f func(provider bschemas.ModelProvider, endpoint, route, model string, pinged bool) bool) {
	if c != nil {
		c.endpoint, c.readSets = endpoint, f
	}
}

// prefixRewritten says the client rewrote the prefix of this request: the agent's own compaction came just
// before it, or the tracker found the thread forks or rewinds another one. Nil-safe.
func (c *capture) prefixRewritten() bool {
	return c != nil && ((c.compKnown && c.compVerdict != "") || c.hasInherit)
}

// noteThread records which thread of the session this request belongs to.
func (c *capture) noteThread(res thread.Result) {
	defer c.timed("noteThread")()
	if c != nil {
		c.thread = res.ID
		c.inherits, c.hasInherit = res.InheritsFrom()
		// A "session" result is the tracker's fallback: no session id, no message list, or a recovered
		// panic. It judged nothing, so the row keeps no verdict (NULL) and the old rule decides (#947).
		c.compVerdict, c.compKnown = string(compactionVerdict(res)), res.Source != thread.SourceSession
	}
}

// noteWaitBody stashes the PRISTINE request body for issue #424's RECORD half, so finish()
// can scan it (waitcause.Tracker.Observe) after the request has already gone upstream. This
// is a slice-header copy — the request path pays nothing beyond this one field write, however
// large the body: see waitcause's own benchmark for what the scan itself costs, and
// why that cost belongs on finish()'s side of the line, not here. Call site: proxy.go, right
// after noteThread, with `orig` — the body as the agent sent it, before any pipeline rewrite,
// the same body threadFor already matches on.
func (c *capture) noteWaitBody(body []byte) {
	defer c.timed("noteWaitBody")()
	if c != nil {
		c.waitBody = body
	}
}

// noteAliasSource stashes the inputs of this request's alias derivation: the pipeline that
// transforms the body, the wire dialect, and the bypass flag. The restart grace's record
// side (#766) must touch the grace clock under the id the pipeline's own turn will ASK
// for, and apply derives that id from the body AFTER transformEnvelope — toolfilter's
// skills half rewrites the head message the session hash reads — so hashing the pristine
// body the capture already holds would produce a key no request ever derives. See
// apply.EnforcedSessionIDFor, the same door the expand repair and the observe job use.
func (c *capture) noteAliasSource(pipe *components.Pipeline, api string, bypass bool) {
	defer c.timed("noteAliasSource")()
	if c == nil {
		return
	}
	c.aliasPipe, c.aliasAPI, c.aliasBypass = pipe, api, bypass
}

// noteMeta records the request's own metadata. Called once, from the chat handler, with
// the PRISTINE inbound body — what the client asked for, before the pipeline rewrote
// anything. That is the honest subject: `temperature` and `reasoning_effort` are the
// caller's choices, and reporting a value we injected as theirs would make the dashboard's
// per-effort cost breakdown a statement about ourselves.
func (c *capture) noteMeta(m dash.Meta) {
	defer c.timed("noteMeta")()
	if c != nil {
		c.meta = m
	}
}

// noteProject records the directory the agent was started in. Called once, from the chat handler,
// with the PRISTINE inbound body — for the same reason noteMeta is: the pipeline may compact the
// environment block out of the transcript it forwards, and reading the project from the rewritten
// body would record "unattributed" on exactly the requests the pipeline acted on.
func (c *capture) noteProject(p string) {
	defer c.timed("noteProject")()
	if c != nil {
		c.project = p
	}
}

// noteInventory records WHICH tools, MCP servers and skills the request declared, and
// which of them its last turn actually called. The count in Meta.Tools answers "how
// many"; this answers "which", which is the only way to say how much of a declared
// inventory is dead weight.
//
// Cheap by construction, because it runs on every request: the whole per-request cost is
// one structural scan of the tools array, two byte searches for the skills listing, one
// hash of that set and one map lookup. Parsing and tokenizing happen once per distinct
// declaration set (dash.ScanInventory memoizes by digest), so a session pays for its
// inventory on its first request and nothing on the other 64. Measured in
// dash.BenchmarkScanInventory.
func (c *capture) noteInventory(provider string, body []byte) {
	defer c.timed("noteInventory")()
	if c == nil {
		return
	}
	c.inv = dash.ScanInventory(provider, body)
}

func (c *capture) noteCG(ms float64) {
	defer c.timed("noteCG")()
	if c != nil {
		c.cgMs = ms
	}
}
func (c *capture) noteUpstream(ms float64, status int) {
	defer c.timed("noteUpstream")()
	if c != nil {
		c.upstream, c.status = ms, status
	}
}

// noteKeepAlive records what the idle keep-alive did during the span this request ended.
func (c *capture) noteKeepAlive(ka kaArrival) {
	defer c.timed("noteKeepAlive")()
	if c != nil {
		c.kaPings, c.kaRefreshed, c.kaStrategy = ka.pings, ka.refreshed, ka.strategyID
		c.kaRefreshedAt, c.kaRefreshedOneHour = ka.refreshedAt, ka.refreshedOneHour
	}
}

// noteCompactionPoint records C (billed tokens) and its source, so the replay cap and the
// episode panel read the figure this request ran under and not a recomputed one (#948).
func (c *capture) noteCompactionPoint(tokens int, source string) {
	defer c.timed("noteCompactionPoint")()
	if c != nil {
		c.cpoint, c.cpointSrc = tokens, source
	}
}

func (c *capture) noteModel(model string) {
	defer c.timed("noteModel")()
	if c != nil {
		c.model = model
	}
}
func (c *capture) noteExpand(tokens int) {
	defer c.timed("noteExpand")()
	if c != nil {
		c.expands++
		c.expandTok += tokens
	}
}

// noteSweepExpand records that an expand call restored a sweep marker, so the dashboard can charge
// it to the sweep (#404 point 7). It is in addition to noteExpand, which still counts the expand in
// the totals. note.First is true only for the first expand of a drop, so a stale tool error repaired
// on every later turn is one false drop and not many.
func (c *capture) noteSweepExpand(tokens int, note offload.SweepExpandNote) {
	defer c.timed("noteSweepExpand")()
	if c == nil || !note.Sweep {
		return
	}
	c.sweepExpands++
	c.sweepExpandTok += tokens
	if note.First && !note.Overlap {
		c.sweepFalseExp++
		if note.Warm {
			c.sweepFalseExpWarm++
		}
	}
	for rule, n := range note.Rules {
		if c.sweepExpandRules == nil {
			c.sweepExpandRules = map[string]int{}
		}
		c.sweepExpandRules[rule] += n
	}
}

// addSweepExpandEvents puts the first expands of sweep v3 drops, by rule, on the sweep's component row
// of this request, beside the drops it raised per rule (sweep_v3_dropped_<rule>). The dashboard keeps
// the row with the request's route and sums its events, so the expand rate per rule reads per route,
// across a restart (#1191). A request with no sweep row (the sweep was not in its pipeline) keeps only
// the request-level counts.
func addSweepExpandEvents(e *dash.Event, rules map[string]int) {
	for i := range e.Components {
		row := &e.Components[i]
		if row.Component != "extract_llm_sweep" {
			continue
		}
		// A copy: the row shares the component report's own map (dash.Event.FromTrace).
		events := maps.Clone(row.Events)
		if events == nil {
			events = map[string]int{}
		}
		row.Events = events
		for rule, n := range rules {
			row.Events["sweep_v3_expanded_"+rule] += n
		}
		return
	}
}

// deferFacts is what tool deferral did on one request, for the dashboard row (#869).
type deferFacts struct {
	tokens, list, searches, dropped, loaded, wrong int
	gross, net, search                             float64
}

// noteDefer records tool deferral's ledger entry for this request: the request facts as the ledger
// booked them (r.WrongGuesses and r.DroppedSearches included), its price (zero when the model has
// none) and how many different tools its searches loaded.
func (c *capture) noteDefer(r tooldefer.Request, m tooldefer.Money, loaded int) {
	defer c.timed("noteDefer")()
	if c == nil {
		return
	}
	c.deferFacts = &deferFacts{tokens: r.DeferredTokens, list: r.ListTokens, searches: r.Searches,
		dropped: r.DroppedSearches, loaded: loaded, wrong: r.WrongGuesses, gross: m.Gross, net: m.Net, search: m.SearchCost}
}

// finish builds the event and hands it off. Called once, after the response has
// been written, so nothing here is on the client's critical path. usage may be
// zero-valued with ok=false, in which case the row is marked partial and left
// unpriced rather than priced as free.
// observeSSE records the response-side streaming facts, which arrive in proxy.go's terminal
// defer alongside the aggregator's own RecordSSE. They are set rather than passed to finish()
// because finish() already carries five arguments and these are about the RESPONSE, not the
// capture's own bookkeeping.
//
// ttfbMs is 0 on a buffered response by construction: the client's first byte IS the whole
// write, so there is no earlier moment to measure. That is why the flag is stored beside the
// duration instead of the two being averaged into one number — an average over both would
// report a healthy TTFB for exactly the requests that had the worst one.
func (c *capture) observeSSE(ttfbMs float64, buffered bool) {
	defer c.timed("observeSSE")()
	if c == nil {
		return
	}
	c.sseTTFBMs, c.sseBuffered = ttfbMs, buffered
}

func (c *capture) finish(usage Usage, usageOK bool, captureContent bool, contentCap, contentMax int) {
	defer c.timed("finish")()
	if c == nil {
		return
	}
	if c.rec == nil {
		// No dashboard: the accountant still runs (#1451), after the handler returns. finish runs in the handler's
		// defer, so a synchronous wait would hold the next request on a keep-alive connection. No row waits for it.
		go c.finishAccountant(usage, usageOK)
		return
	}
	e := &dash.Event{
		TS:       c.start.UnixMilli(),
		TenantID: c.tenant,
		Model:    c.model,
		Provider: c.provider,
		Route:    c.route,
		Preset:   c.preset,
		Status:   c.status,
	}
	e.Agent = c.agent.Label
	e.Project = c.project
	e.Meta = c.meta
	e.FromTrace(c.trace, c.unique)
	// A request the provider rejected saved nothing: book no tokens beside its zero dollars (#914).
	e.DropUnbilledSavings()
	e.ThreadID = c.thread
	e.AgentCompaction = c.agentComp
	e.ClientCompaction, e.ClientCompactionKnown = c.compVerdict, c.compKnown
	// Only a client-set C (the user's setting or the client's own default, #963) is stored: the table value scales with the window and is read back
	// from the table, so an old row and a new table row read alike (compactionpoint.Recorded).
	if compactionpoint.IsClientSource(c.cpointSrc) && c.cpoint > 0 {
		e.CompactionPoint, e.CompactionPointSource = c.cpoint, c.cpointSrc
	}
	e.Limits, e.AuthSubscription = c.limits, c.authSub
	// The provider's terminal reason, off the same response bytes the token tiers came
	// from. Present even when `usage` was not (see Usage.StopReason).
	e.StopReason = usage.StopReason
	// Issue #424's RECORD half: a CLOSURE, not a call. The body scan (waitcause.Tracker.
	// Observe, ~11ms on a 783 KB body) must NOT run here — finish() runs on the handler's
	// own defer, before the handler returns, so anything synchronous here delays the NEXT
	// request on a keep-alive connection (measured: that is exactly what calling Observe
	// inline here used to add). The dashboard's writer goroutine invokes this closure right
	// before the insert, the same place and for the same reason e.Redact() already runs
	// there — see dash/capture.go's write(). Everything the closure needs is captured by
	// value/reference now, cheaply: no scan happens until the writer calls it.
	//
	// RecordOutcome (this request's own status + message count, for the NEXT request's
	// retry check) is folded into the SAME closure rather than called separately on the
	// request path, so it still runs strictly after Observe reads the PREVIOUS state —
	// exactly the ordering the retry rule depends on — and in the same relative order
	// requests arrived in (the writer processes one channel, FIFO, same as the order
	// Record() was called in). The trade-off: if this row is ever DROPPED (a full capture
	// channel under heavy load — see dash.Recorder.Record's own doc), RecordOutcome never
	// runs for it either, so a retry right after a dropped row could be missed. Fail-open,
	// consistent with every other field here: a missed retry reads as a fresh wake, not a
	// wrong one.
	//
	// failed is this proxy's own analogue of a transcript's isApiErrorMessage: a call that
	// never completed with a 2xx. status 0 (never reached an upstream) is deliberately NOT
	// "failed" — ClassifyWaitingFor's own !usageOK-and-no-stop-reason fallback already reads
	// that case as WaitingUnknown rather than WaitingRetry.
	waits, tenantID, sessionID, threadID, waitBody, status := c.waits, e.TenantID, e.SessionID, e.ThreadID, c.waitBody, c.status
	toolUseNames, stopReason, terminal := usage.ToolUseNames, usage.StopReason, c.terminal
	toolParams, spawn := usage.ToolParams, usage.Spawn
	if usage.ToolUseIncomplete {
		terminal = []string{} // the name list may miss the true last call: never "done" (#857)
	}
	failedCall := status != 0 && status/100 != 2
	provider := bschemas.ModelProvider(c.provider)
	// Both closures run on the writer goroutine, one after the other: normalize once, lazily.
	var normMsgs []bschemas.ChatMessage
	var normDone bool
	early, scanStat := c.early, c.scanStat
	norm := func() []bschemas.ChatMessage {
		if !normDone {
			// The scan that ran while the request was upstream already normalized this body: reuse it when it
			// is ready, else normalize here, as before. The check never blocks.
			if r := early.result(); r != nil {
				normMsgs = r.Messages()
				if scanStat != nil {
					scanStat.writerEarly.Add(1)
				}
			} else {
				normMsgs = apply.NormalizeMessages(provider, waitBody)
				if scanStat != nil && early != nil {
					scanStat.writerOwn.Add(1)
				}
			}
			normDone = true
		}
		return normMsgs
	}
	if c.rec.LifecycleOn() {
		ts, endTS, cacheW, cacheW1h := e.TS, time.Now().UnixMilli(), usage.CacheWrite-usage.CacheWrite1h, usage.CacheWrite1h
		rec := c.rec
		e.LifecycleFn = func() *dash.LifecycleRow {
			return rec.ObserveLifecycle(tenantID, sessionID, threadID, ts, endTS,
				reqfacts.ScanLifecycle(norm, waitBody), cacheW, cacheW1h)
		}
	}
	e.BypassCount, e.BypassBytes = c.trace.BypassCount, len(waitBody)
	e.WaitCause = func() dash.WaitCauseResult {
		obs := waits.ObserveWith(tenantID, sessionID, threadID, waitBody, norm, waitcause.ForClient(c.agent))
		waits.RecordOutcome(tenantID, sessionID, threadID, obs.MsgCount, status)
		waitingFor, waitTool := waitcause.ClassifyWaitingForClient(
			c.agent, terminal, toolUseNames, stopReason, failedCall, usageOK, obs.OpenBackground, obs.OpenTeammate)
		res := dash.WaitCauseResult{
			WaitingFor: waitingFor, WaitTool: waitTool, WakeCause: obs.WakeCause,
			OpenBackground: obs.OpenBackground, OpenTeammate: obs.OpenTeammate,
			MonitorExpiryMs: obs.MonitorExpiryMs,
		}
		// The metadata that predicts when the user returns (#1198). The wait tool's `timeout` and
		// `run_in_background` are kept only for a wait on the tool's own result (waitTool is set).
		if waitTool != "" {
			if toolParams.HasTimeout {
				v := toolParams.TimeoutMs
				res.WaitToolTimeoutMs = &v
			}
			if toolParams.HasBackground {
				v := toolParams.Background
				res.WaitToolBackground = &v
			}
		}
		// A prompt is known only for a response that was read: an unknown cause says nothing.
		if waitingFor != waitcause.WaitingUnknown {
			res.WaitPrompt = waitcause.PromptNone
			if waitingFor == waitcause.WaitingHuman {
				res.WaitPrompt = waitcause.PromptOfClient(c.agent, toolUseNames)
			}
		}
		// The person's id, raw, for the writer to hash. Read here, on the writer goroutine.
		res.Fanout, res.SpawnType = spawn.N, spawn.Type
		res.UserKey = session.UserKey(c.agent.UserID(waitBody))
		return res
	}
	e.CGLatencyMs, e.UpstreamMs = c.cgMs, c.upstream
	e.TTFBMs, e.SSEBuffered = c.sseTTFBMs, c.sseBuffered
	e.Expands, e.ExpandTokens = c.expands, c.expandTok
	e.SweepExpands, e.SweepExpandTokens, e.SweepFalseExpands = c.sweepExpands, c.sweepExpandTok, c.sweepFalseExp
	addSweepExpandEvents(e, c.sweepExpandRules)
	if d := c.deferFacts; d != nil {
		e.DeferTokens, e.DeferListTokens, e.DeferSearches = d.tokens, d.list, d.searches
		e.DeferDroppedSearches = d.dropped
		e.DeferLoaded, e.DeferWrongGuesses = d.loaded, d.wrong
		e.DeferGrossUSD, e.DeferNetUSD, e.DeferSearchUSD = d.gross, d.net, d.search
		// The ledger's own cost, taken here, before PriceMode can move the net onto the library's figure.
		e.DeferCostUSD = d.gross - d.net
	}
	e.SweepFalseExpandsWarm = c.sweepFalseExpWarm
	e.FreshInput, e.CacheRead = usage.FreshInput, usage.CacheRead
	e.CacheWrite, e.OutputTokens = usage.CacheWrite, usage.Output
	e.CacheWrite1h = usage.CacheWrite1h
	e.Backend = usage.Backend

	// context-guru's own model spend attributable to THIS request: what this request's
	// OWN sink recorded, never a delta of the process-wide counters. The delta was
	// wrong by construction on a multi-tenant proxy — any other tenant's compaction
	// call inside the window landed on this row, and from here in tenant_spend, the
	// tenant's month-to-date figure and cg_tenant_cg_llm_cost_usd. It also let a tenant
	// infer other tenants' compaction activity from its own rows.
	//
	// Priced at the COMPACTION model's rate when the sink knows which model made the calls,
	// falling back to the agent's rate when it does not.
	//
	// It used to always use the agent's rate, on the theory that a cheap model was "close
	// enough" and that over-reporting our own cost was the safe direction. It is not safe in
	// either direction: haiku tokens billed at opus rates is roughly 15x, enough to make a
	// configuration that pays read as one that loses money on the dashboard, which is a
	// decision people make with this number.
	_, cgIn, cgOut := c.llm.Totals()
	// Our compaction calls are prompt-cached too — a cold sweep sends the whole transcript,
	// so the cache-write tier is the LARGEST part of what it costs. This used to pass 0 for
	// both tiers, which under-counted our own spend by roughly 4x on a sweep and disagreed
	// with the per-call figure on the Components tab. A cost that is wrong in the flattering
	// direction is worse than one that is wrong the other way: it argues for spending.
	cgCacheWrite, cgCacheRead := c.llm.CacheTotals()
	cgModel := c.llm.Model()

	// Cache attribution, with a cold start treated as the non-failure it is. The verdict is a
	// label on the row; the savings are the accountant library's.
	//
	// Per THREAD (#423). Keyed on the session alone, a subagent's requests reset the main
	// thread's idle gap, so the main thread's TTL expiries fell through to prefix_change — on
	// one long session, 46 of 65 prefix_change rows ($70.27) were exactly that.
	//
	// A NEW thread whose content is mostly a known thread's is that conversation with a changed
	// prefix (a rewind, an edit, a fork): by the mass it shares it is not a cold start.
	observe := c.rec.ObserveThread
	if c.hasInherit {
		observe = func(tenant, session, threadID, model string, now int64, tail uint64) (bool, bool, int64, bool) {
			return c.rec.ObserveThreadFrom(tenant, session, threadID, c.inherits, model, now, tail)
		}
	}
	seenSession, seenModel, sinceMs, tailChanged := observe(
		e.TenantID, e.SessionID, e.ThreadID, e.Model, e.TS, e.SplitTailHash)
	// The restart grace's clock must also learn this session's ALIAS id (#766): a seesaw-id
	// tenant (the session header on some turns, not others) refreshes the ONE provider entry
	// under either id, so a later headerless turn — whose own derived id IS the alias — must
	// find this turn's warmth under the key it will ask for. The id is derived through the
	// pipeline's own transform (apply.EnforcedSessionIDFor, empty explicit id), because
	// apply derives the id it asks for from the body AFTER transformEnvelope — toolfilter's
	// skills half rewrites the head message the session hash reads — and a hash of the
	// pristine body is a key no request ever derives. Which id comes out: the METADATA
	// session id when the body carries one (metadata.user_id / metadata.taskId), else the
	// content hash of the transformed head — the same ONE rule the ask side resolves
	// (apply.graceAliasID), so the two cannot drift. A closure for the same reason
	// WaitCause is one: the transform and re-parse run on the writer goroutine, not the
	// handler's defer. Only set when this row ran under a session id at all — observe-mode
	// rows carry none — and the closure answers '' when the alias is the request's own id
	// (a headerless turn, or no body to derive from: /compact never stashes one), which
	// observe() above already touched or nothing can.
	//
	// The Bob IDE's X-Task-Id header is an explicit id, like the session header. So the alias
	// of a Bob IDE session is the content hash.
	if e.SessionID != "" {
		pipe, api, bypass := c.aliasPipe, c.aliasAPI, c.aliasBypass
		e.AliasFn = func() string {
			alias := apply.EnforcedSessionIDFor(pipe, tenantID, "", api, provider, bypass, waitBody)
			if alias == "" || alias == sessionID {
				return ""
			}
			return alias
		}
	}
	// Only a known expiry can establish ttl_expiry. OpenAI's 30m is a
	// minimum guarantee, so a later miss cannot be attributed to expiry alone.
	//
	//
	// The cause comes from a measured comparison with this thread's previous request, not from
	// CacheWrite > 0 (issue #600). A miss with nothing measured reads unknown.
	if c.inv != nil && c.inv.System != nil {
		e.SystemHash = c.inv.System.Hash
	}
	if c.inv != nil {
		e.DeclDigest = c.inv.Digest
	}
	//
	// The gap for the TTL test is measured from the last request on the SAME model: a cache entry
	// belongs to one model, so a request on another model between two requests on this one does
	// not refresh it. SinceLastMs below keeps the per-thread gap.
	cause, modelGap, known, lastWasSame, prevPromptTokens, prevOneHour := c.rec.ObservePrefix(e.SessionID, e.ThreadID, c.inherits, c.hasInherit,
		dash.FactsOf(e), e.Status >= 200 && e.Status < 300)
	//
	// A keep-alive ping that READ the cache moved the provider's clock. The idle gap is
	// measured from the last real request, so without this a 297 s gap with a ping at 289 s
	// reads as an expiry on a turn that came 8 s after the refresh.
	age := sinceMs
	if known {
		age = modelGap
		// A ping refreshed the entry of the model that last ran in the thread. Cut the age only
		// when that was this model. This holds after a failed request too, when the gap since
		// the last good request differs from sinceMs.
		if lastWasSame {
			age = entryAgeMs(modelGap, e.TS, c.kaRefreshedAt)
		}
	} else {
		age = entryAgeMs(sinceMs, e.TS, c.kaRefreshedAt)
	}
	e.PrefixCompared = known
	e.AttributeCache(seenSession, seenModel, age, c.attributionTTLMs(e.Provider, e.Model, prevOneHour), cause, prevPromptTokens)
	e.SessionFirst, e.TailChanged = !seenSession, tailChanged
	// The raw gap and what the keep-alive did during it, recorded on the row. The keep-alive's
	// saving is the accountant library's rescue, not computed from these.
	e.SinceLastMs = sinceMs
	e.KeepAlivePings, e.KeepAliveRefreshed = c.kaPings, c.kaRefreshed
	e.KeepAliveStrategyID = c.kaStrategy
	c.fillStrategyMeta(e)

	var price modelinfo.Price
	priced := false
	if c.pricer != nil && c.model != "" {
		price, priced = c.pricer.Price(context.Background(), c.model)
	}
	// Run the accountant BEFORE the event is priced. Only with usage: without it the thread
	// state stays untouched and the event carries no result. Not after several upstream rounds
	// either (see rounds): no Result is attached, so PriceMode books no saving
	// (acc_fallback reason no_result). The thread state is left as the
	// previous request left it; the next request then sees a longer gap, which the library
	// handles (a phase it cannot prove is unknown and falls back too).
	// The job is joined first: every read of the record below needs it. Whatever way the request ended, the client
	// hash the job's begin kept for end is not needed past here. end removes it on the normal path and accDrop on a
	// recovered panic; this covers any path that reached neither.
	c.accWait()
	defer c.savings.drop(c.accReq)
	// accCompute is its own span: the accountant's compute is not the dashboard's capture work (#1059).
	endAcc := timedSpan("accCompute")
	// The rules for which request books no figure are accrec.Recorder.Price's (see accCompute). A request with no
	// figure leaves the event with no Result, so PriceMode books no dollars either (acc_fallback no_result).
	if res, ok := c.accCompute(usage, usageOK); ok {
		e.SetAccountant(res)
	}
	// The thinking blocks this response made, with its thinking_tokens: when the client sends one back, the
	// accountant prices it at its own count (issue #1187). Only a billed response: a rejected one made no block
	// the client keeps.
	if usageOK && !c.rejected() {
		c.savings.recordThinking(usage.Thinking)
	}
	endAcc()
	c.savings.finished(c, usage, usageOK)
	e.SetAccountantContext(c.trace.OpsDropped, c.observe)
	if c.accServerHeldHistory {
		e.SetAccountantServerHeldHistory()
	}
	e.PriceMode(price, usageOK && priced)
	if (c.accReq != nil || c.accServerHeldHistory) && e.AccountantFallback() != "" {
		e.RecordAccountantFallback()
		logFallback(e, c.noResultWhy(usageOK))
	}
	e.CGModel = cgModel
	e.CGFresh, e.CGCacheRead, e.CGCacheWrite, e.CGOutput = cgIn, cgCacheRead, cgCacheWrite, cgOut
	e.PriceCG(c.pricer, price, priced)

	if !captureContent {
		e.Content = nil
		// Strip only the TEXT halves of the recorded calls, and keep the rows. Consent is
		// about storing transcript content; the cost, latency, tokens, gate reason and
		// saving of a call are our own operational metrics, and dropping them would leave
		// an account unable to answer "was this component worth it?" — the reason the
		// per-call record exists — as the price of not storing its transcripts.
		for i := range e.Extractions {
			e.Extractions[i].Before, e.Extractions[i].After = "", ""
			e.Extractions[i].Summary = ""
		}
	} else {
		// Truncate here (a slice reslice, free), but do NOT redact here.
		//
		// finish is called from serve's defer, which runs before the handler RETURNS —
		// not merely after the response body is written. So anything expensive on this
		// line is paid by the next request on a keep-alive connection, i.e. by every real
		// agent. Redaction is nine regexes over up to contentMax x 2 blobs, measured at
		// ~53 ms/request (~25% of a request), against the ~175 ns the channel send costs.
		// It therefore belongs on the writer goroutine, which already owns the event and
		// is already off the hot path.
		//
		// Secrets still never reach disk: the writer redacts BEFORE the INSERT (see
		// Event.Redact, called from Recorder.run). What changed is which goroutine pays,
		// not whether redaction happens.
		if len(e.Content) > contentMax {
			e.Content = e.Content[:contentMax]
		}
		e.ContentCap = contentCap
	}
	// The declared/used inventory goes to its own tables, keyed by session: the NAMES and
	// token weights are NOT under the content gate, because they are identifiers of the
	// caller's own configuration rather than transcript text (see dash/toolinventory.go),
	// and they are not on the Event, because the rows are per session and deduped by
	// declaration-set digest rather than one set per request.
	//
	// The declaration TEXT is a different class of data — a tool schema and a system prompt
	// are whatever the caller wrote, up to and including something they pasted — so it rides
	// the same captureContent decision as the transcript, and the writer stores NULL for it
	// otherwise. Same discipline as Record either way: a non-blocking send, nothing else here.
	// e.Project rides along: invocations are recorded per folder, because one session works in
	// several and a per-session folder cannot scope a per-project report.
	// After the response, on the handler's defer: counter adds only (R46). A panic costs the metric.
	c.inference.Observe(e, time.Since(c.start))
	c.rec.RecordInventory(e.TenantID, e.SessionID, e.TS, c.inv, captureContent, e.Project)
	c.rec.Record(e)
}

// entryAgeMs is the library's apply.EntryAgeMs: the proxy passes facts, and the rule lives with the cold
// check (#1350). attributionTTLMs, below, does the same for apply.BilledLifetimeMs.
func entryAgeMs(sinceMs, tsMs int64, refreshedAt time.Time) int64 {
	return apply.EntryAgeMs(sinceMs, tsMs, refreshedAt)
}

// attributionTTLMs passes the two facts to apply.BilledLifetimeMs, which decides the lifetime the miss verdict
// judges against: the tier the last write was billed at, and whether the last touch was a live free 1-hour ping.
func (c *capture) attributionTTLMs(provider, model string, prevOneHour bool) int64 {
	return apply.BilledLifetimeMs(bschemas.ModelProvider(provider), model, prevOneHour, c != nil && c.kaRefreshedOneHour)
}

// LiveFacts are the request-side inputs of a learned keep-alive campaign, taken from the same places the dashboard row is filled from, so
// a campaign sees at serve time the values it was trained on.
type LiveFacts struct {
	Messages, Tools, SystemBlocks, MaxTokens int
	Effort, Thinking, CacheTTL, Agent        string
	Stream                                   bool
	TTFBMs                                   float64
	// Arrival is this request's identity in a learned campaign's tracker (nil when no campaign applies). It travels with the request, not with its thread,
	// because requests overlap on a thread.
	Arrival *predictive.Arrival
}

// liveFacts reads them off the capture (nil-safe). ttfbMs is the streamed first-byte delay, 0 on buffered responses, as the dashboard records it.
func (c *capture) liveFacts(ttfbMs float64) *LiveFacts {
	defer c.timed("liveFacts")()
	if c == nil {
		return nil
	}
	return &LiveFacts{Messages: c.trace.Messages, Tools: c.meta.Tools, SystemBlocks: c.meta.SystemBlocks, MaxTokens: c.meta.MaxTokens,
		Effort: c.meta.ReasoningEffort, Thinking: c.meta.ThinkingMode, CacheTTL: c.trace.CacheTTL, Agent: c.agent.ModelAgent(c.provider), Stream: c.meta.Stream, TTFBMs: ttfbMs, Arrival: c.predArrival}
}

// noteRounds records how many upstream rounds the request took (1 without an expand). Nil-safe.
func (c *capture) noteRounds(n int) {
	defer c.timed("noteRounds")()
	if c != nil {
		c.rounds = n
	}
}

// accDrop forgets the savings record: the pipeline's rewrite did not go upstream. Nil-safe.
func (c *capture) accDrop() {
	if c != nil {
		c.savings.drop(c.accReq)
		c.accReq = nil
		c.accOpen = false
	}
}

// noResultWhy names the branch that left an event with no accountant Result, for the acc_fallback line. The reason
// no_result covers four unrelated things, and the log line used to say only that.
// rejected is true when the provider answered with a status that is not 2xx. Status 0 means no upstream
// answered (such as /compact), and is not a rejection.
func (c *capture) rejected() bool {
	return accrec.Rejected(c.status)
}

func (c *capture) noResultWhy(usageOK bool) string {
	return accrec.NoResultWhy(c.accReq != nil, usageOK, c.accNoResult, c.rounds)
}

// fillStrategyMeta sets the strategy metadata that one request alone decides (#1198): the host's
// UTC offset and the ping plan. The wait's tool parameters and prompt, the turn, the concurrency and
// the person's hash are filled on the writer (WaitCause, dash.Recorder).
//
// The offset comes from the host's own time zone at the request time, and only on a local proxy,
// where the host is the user's machine. A hosted proxy has no zone of its user, so it leaves the
// field NULL (unknown).
func (c *capture) fillStrategyMeta(e *dash.Event) {
	if !c.hosted {
		_, off := c.start.Zone()
		v := off / 60
		e.TZOffsetMin = &v
	}
	e.ClientEntrypoint = c.agent.Entrypoint
	if p := c.kaPlan; p != nil {
		e.KAPlannedPings, e.KAMinPrefixTokens, e.KAIdleS = &p.MaxPings, &p.MinPrefixTokens, &p.IdleSeconds
	}
}

// startEarlyScan starts the early wait-cause scan of body (earlyscan.go). It needs the tracker (the dashboard
// is on) and a non-empty body. Nil-safe.
func (c *capture) startEarlyScan(h *Handler, ctx context.Context, provider bschemas.ModelProvider, body []byte) {
	if c == nil || h == nil || h.waits == nil || len(body) == 0 {
		return
	}
	c.scanStat = &h.scan
	c.early = startEarlyScan(ctx, provider, body, h.scanGate, h.scanDone, waitcause.ForClient(c.agent))
}

// stopEarlyScan ends the scan's work in progress when the request is over. Nil-safe.
func (c *capture) stopEarlyScan() {
	if c != nil {
		c.early.stop()
	}
}

// earlyOpenTasks is the keeper's non-blocking take of the early scan at response end. Nil-safe.
func (c *capture) earlyOpenTasks(h *Handler) *keepalive.TaskCounts {
	if c == nil || h == nil {
		return nil
	}
	return c.early.openTasks(&h.scan)
}
