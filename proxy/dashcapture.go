package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/internal/waitcause"
	"github.com/rossoctl/context-guru/predictive"
	"github.com/tidwall/gjson"
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

// capture is the per-request scratchpad the chat handler fills as it goes.
type capture struct {
	rec       *dash.Recorder
	pricer    modelinfo.Pricer
	preset    string
	route     string
	provider  string
	model     string
	agent     string
	project   string
	start     time.Time
	cgMs      float64
	upstream  float64
	status    int
	expands   int
	expandTok int
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
	// thread is the session thread this request belongs to (see internal/thread), set by
	// noteThread once the session id is known. "" is the primary thread. inherits/hasInherit
	// name the thread a NEW thread continues by content (thread.Result.Inherits).
	thread     string
	agentComp  bool
	inherits   string
	hasInherit bool
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
	// kaPings / kaRefreshed / kaStrategy are what the idle keep-alive did during the span
	// this request just ended: how many pings it sent, how many tokens the last of them
	// read from cache, and which manager-controlled strategy (if any) resolved the policy
	// that sent them. Recorded on the request path because the keeper's per-session state
	// is cleared the moment a real request arrives, so this is the only point at which all
	// three facts are still in hand.
	kaPings     int
	kaRefreshed int64
	kaStrategy  string
	// kaRefreshedAt is when the provider's entry was last known refreshed (see kaArrival).
	kaRefreshedAt time.Time
	// inv is the request's DECLARED inventory (tool / MCP / skill names and their token
	// weights) plus the tool calls of its last tool-using turn — nil when the request
	// declares no tools, and nil when the dashboard is off. Read on the request path with
	// the pristine body, like meta; emitted at finish, where the session id is known.
	inv *dash.Inventory
}

// newCapture starts a capture for one request, or returns nil when the dashboard
// is off — every call site is nil-safe, so the disabled path costs one nil check.
func (h *Handler) newCapture(r *http.Request, provider, route string, tn *Tenancy) *capture {
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
		agent: r.UserAgent(), start: time.Now(),
		llm:    &cheapmodel.Sink{},
		unique: map[string]int{},
		waits:  h.waits,
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
	if c == nil {
		return ctx
	}
	return cheapmodel.WithSink(ctx, c.llm)
}

// noteTrace records the pipeline's outcome and computes each component's
// unique-savings share. The dedup map lives in the recorder (process-wide), which
// is what makes "unique" mean the same thing here and in /stats.
func (c *capture) noteTrace(tr apply.Trace) {
	if c == nil {
		return
	}
	c.trace = tr
	if tr.Run == nil {
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
	if c != nil {
		c.agentComp = v
	}
}

// noteThread records which thread of the session this request belongs to.
func (c *capture) noteThread(res thread.Result) {
	if c != nil {
		c.thread = res.ID
		c.inherits, c.hasInherit = res.InheritsFrom()
	}
}

// noteWaitBody stashes the PRISTINE request body for issue #424's RECORD half, so finish()
// can scan it (waitcause.Tracker.Observe) after the request has already gone upstream. This
// is a slice-header copy — the request path pays nothing beyond this one field write, however
// large the body: see internal/waitcause's own benchmark for what the scan itself costs, and
// why that cost belongs on finish()'s side of the line, not here. Call site: proxy.go, right
// after noteThread, with `orig` — the body as the agent sent it, before any pipeline rewrite,
// the same body threadFor already matches on.
func (c *capture) noteWaitBody(body []byte) {
	if c != nil {
		c.waitBody = body
	}
}

// noteMeta records the request's own metadata. Called once, from the chat handler, with
// the PRISTINE inbound body — what the client asked for, before the pipeline rewrote
// anything. That is the honest subject: `temperature` and `reasoning_effort` are the
// caller's choices, and reporting a value we injected as theirs would make the dashboard's
// per-effort cost breakdown a statement about ourselves.
func (c *capture) noteMeta(m dash.Meta) {
	if c != nil {
		c.meta = m
	}
}

// noteProject records the directory the agent was started in. Called once, from the chat handler,
// with the PRISTINE inbound body — for the same reason noteMeta is: the pipeline may compact the
// environment block out of the transcript it forwards, and reading the project from the rewritten
// body would record "unattributed" on exactly the requests the pipeline acted on.
func (c *capture) noteProject(p string) {
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
	if c == nil {
		return
	}
	c.inv = dash.ScanInventory(provider, body)
}

// The dialect map metaFromBody normalizes. Two dialects onto one set of columns, because a
// column that means `reasoning_effort` for one client and nothing for another cannot be
// aggregated:
//
//	Anthropic  max_tokens, temperature, top_p, stream, tools[], tool_choice{type},
//	           system[] (blocks, or a bare string), thinking{type,budget_tokens},
//	           output_config{effort}
//	OpenAI     max_tokens / max_completion_tokens, temperature, top_p, stream, tools[],
//	           tool_choice (a bare string, or {function:{name}}), reasoning_effort,
//	           and no top-level system at all — it is a role=system message
//
// metaFromBody reads the request's metadata off the raw body in ONE structural pass.
//
// One pass, and it is measured rather than assumed. This runs on the request goroutine, so
// the shape of the cost matters more than its absolute size:
//
//	one ForEach over the top-level object   0.55 ms/MB, 1 allocation
//	one gjson path query per field (12)     2.7  ms/MB, ~5 allocations
//
// The per-field version re-scans past the whole `messages` array once per field, which is
// 5x the work for a body of any interesting size — so the single pass wins, even though it
// pays for one body-length copy (`gjson.ParseBytes` is `Parse(string(json))`). For scale:
// the pipeline on this same path already runs seven whole-body path queries to count
// breakpoints, and re-serializes the body outright when a component rewrites a message.
// ponytail: costs one body-sized short-lived copy; swap for a zero-copy iterator if this
// ever appears in a profile.
//
// The ForEach never parses `messages` — each value is skipped structurally by byte — so the
// cost is linear in the body and independent of the transcript's depth or message count.
//
// Breakpoint counts are NOT read here: the pipeline already counts them to respect the
// provider's cap of four, and they reach the event on the trace for free (see FromTrace).
func metaFromBody(body []byte) dash.Meta {
	m := metaFromBodyRefs(body)
	// COPY EVERY STRING OUT OF THE BODY. gjson's String() returns a substring of `body` without
	// copying, so a Meta built from it holds the WHOLE request body alive for as long as the Meta
	// lives — and it lives far past the request: the event carries it to the writer, and the SSE
	// hub keeps the last 256 events for late subscribers. On CG2, 256 kept events from
	// 1,000-message sessions held 1-20 MB bodies each: four OOM kills at the 8 GiB limit on
	// 2026-10-19/20. strings.Clone makes each field a few bytes of its own.
	m.ReasoningEffort = strings.Clone(m.ReasoningEffort)
	m.ThinkingMode = strings.Clone(m.ThinkingMode)
	m.ToolChoice = strings.Clone(m.ToolChoice)
	return m
}

// metaFromBodyRefs reads the fields. Its strings ALIAS body; only metaFromBody may return them.
func metaFromBodyRefs(body []byte) dash.Meta {
	var m dash.Meta
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		switch k.Str {
		case "max_tokens", "max_completion_tokens":
			// Either dialect's output cap. Anthropic's spelling wins if a body carries
			// both, which is what an Anthropic-shaped upstream would honour too.
			if m.MaxTokens == 0 || k.Str == "max_tokens" {
				m.MaxTokens = int(v.Int())
			}
		case "temperature":
			// Number-typed only, and by POINTER: "0" and absent are different facts, and a
			// non-numeric value is a malformed request rather than a temperature.
			if v.Type == gjson.Number {
				f := v.Float()
				m.Temperature = &f
			}
		case "top_p":
			if v.Type == gjson.Number {
				f := v.Float()
				m.TopP = &f
			}
		case "stream":
			m.Stream = v.Bool()
		case "reasoning_effort": // OpenAI: top level
			m.ReasoningEffort = v.String()
		case "output_config": // Anthropic: nested, never top level
			// Both accepted forms: the bare level string and {"type": "<level>"}.
			if e := v.Get("effort"); e.Exists() {
				if e.IsObject() {
					m.ReasoningEffort = e.Get("type").String()
				} else {
					m.ReasoningEffort = e.String()
				}
			}
		case "thinking":
			m.ThinkingMode = v.Get("type").String()
			m.ThinkingBudget = int(v.Get("budget_tokens").Int())
		case "tool_choice":
			m.ToolChoice = toolChoiceMode(v)
		case "tools":
			m.Tools = int(v.Get("#").Int())
		case "system":
			// A bare string is one block; an array is its length. A COUNT, never the text:
			// the system prompt is a request's most sensitive content, and its shape is
			// what explains a cache prefix. The text belongs to transcript capture, under
			// that gate and that redaction.
			if v.IsArray() {
				m.SystemBlocks = int(v.Get("#").Int())
			} else if v.Type == gjson.String && v.Str != "" {
				m.SystemBlocks = 1
			}
		}
		return true
	})
	return m
}

// toolChoiceMode normalizes the two dialects' tool_choice shapes to the forcing MODE.
// OpenAI sends a bare string ("auto"|"none"|"required") or an object naming a function;
// Anthropic sends an object ({"type":"auto"|"any"|"none"|"tool"}). The forced tool's name
// is deliberately dropped: it is unbounded client text and no aggregate asks for it.
func toolChoiceMode(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.Str
	}
	if v.IsObject() {
		if t := v.Get("type"); t.Type == gjson.String {
			return t.Str
		}
		if v.Get("function.name").Exists() {
			return "tool" // OpenAI's object form forces one named function
		}
	}
	return ""
}

func (c *capture) noteCG(ms float64) {
	if c != nil {
		c.cgMs = ms
	}
}
func (c *capture) noteUpstream(ms float64, status int) {
	if c != nil {
		c.upstream, c.status = ms, status
	}
}

// noteKeepAlive records what the idle keep-alive did during the span this request ended.
func (c *capture) noteKeepAlive(ka kaArrival) {
	if c != nil {
		c.kaPings, c.kaRefreshed, c.kaStrategy = ka.pings, ka.refreshed, ka.strategyID
		c.kaRefreshedAt = ka.refreshedAt
	}
}

func (c *capture) noteModel(model string) {
	if c != nil {
		c.model = model
	}
}
func (c *capture) noteExpand(tokens int) {
	if c != nil {
		c.expands++
		c.expandTok += tokens
	}
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
	if c == nil {
		return
	}
	c.sseTTFBMs, c.sseBuffered = ttfbMs, buffered
}

func (c *capture) finish(usage Usage, usageOK bool, captureContent bool, contentCap, contentMax int) {
	if c == nil {
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
	e.Agent = dash.AgentFor(c.agent)
	e.Project = c.project
	e.Meta = c.meta
	e.FromTrace(c.trace, c.unique)
	e.ThreadID = c.thread
	e.AgentCompaction = c.agentComp
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
	toolUseNames, stopReason := usage.ToolUseNames, usage.StopReason
	failedCall := status != 0 && status/100 != 2
	provider := bschemas.ModelProvider(c.provider)
	// Both closures run on the writer goroutine, one after the other: normalize once, lazily.
	var normMsgs []bschemas.ChatMessage
	var normDone bool
	norm := func() []bschemas.ChatMessage {
		if !normDone {
			normMsgs, normDone = apply.NormalizeMessages(provider, waitBody), true
		}
		return normMsgs
	}
	if c.rec.LifecycleOn() {
		ts, endTS, cacheW, cacheW1h := e.TS, time.Now().UnixMilli(), usage.CacheWrite-usage.CacheWrite1h, usage.CacheWrite1h
		rec := c.rec
		e.LifecycleFn = func() *dash.LifecycleRow {
			return rec.ObserveLifecycle(tenantID, sessionID, threadID, ts, endTS,
				scanLifecycle(norm, waitBody), cacheW, cacheW1h)
		}
	}
	e.BypassCount, e.BypassBytes = c.trace.BypassCount, len(waitBody)
	e.WaitCause = func() dash.WaitCauseResult {
		obs := waits.ObserveWith(tenantID, sessionID, threadID, waitBody, norm)
		waits.RecordOutcome(tenantID, sessionID, threadID, obs.MsgCount, status)
		waitingFor, waitTool := waitcause.ClassifyWaitingFor(
			toolUseNames, stopReason, failedCall, usageOK, obs.OpenBackground, obs.OpenTeammate)
		return dash.WaitCauseResult{
			WaitingFor: waitingFor, WaitTool: waitTool, WakeCause: obs.WakeCause,
			OpenBackground: obs.OpenBackground, OpenTeammate: obs.OpenTeammate,
			MonitorExpiryMs: obs.MonitorExpiryMs,
		}
	}
	e.CGLatencyMs, e.UpstreamMs = c.cgMs, c.upstream
	e.TTFBMs, e.SSEBuffered = c.sseTTFBMs, c.sseBuffered
	e.Expands, e.ExpandTokens = c.expands, c.expandTok
	e.FreshInput, e.CacheRead = usage.FreshInput, usage.CacheRead
	e.CacheWrite, e.OutputTokens = usage.CacheWrite, usage.Output
	e.CacheWrite1h = usage.CacheWrite1h

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

	// Cache attribution, with a cold start treated as the non-failure it is. BEFORE
	// pricing, because whether this is the session's first request is an input to a dollar
	// figure and not only to a label — see Event.cachesplitSavedUSD.
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
	cause, modelGap, known, lastWasSame := c.rec.ObservePrefix(e.SessionID, e.ThreadID, c.inherits, c.hasInherit,
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
	e.AttributeCache(seenSession, seenModel, age, cacheAttributionTTLMs(e.Provider, e.Model), cause)
	e.SessionFirst, e.TailChanged = !seenSession, tailChanged
	// The raw gap and what the keep-alive did during it. Both feed a dollar figure rather
	// than only a label (see Event.keepaliveSavedUSD), so they have to be set before Price.
	e.SinceLastMs = sinceMs
	e.KeepAlivePings, e.KeepAliveRefreshed = c.kaPings, c.kaRefreshed
	e.KeepAliveStrategyID = c.kaStrategy

	var price modelinfo.Price
	priced := false
	if c.pricer != nil && c.model != "" {
		price, priced = c.pricer.Price(context.Background(), c.model)
	}
	e.Price(price, usageOK && priced)
	if cgIn > 0 || cgOut > 0 || cgCacheWrite > 0 || cgCacheRead > 0 {
		cgPrice, cgPriced := price, priced
		if c.pricer != nil && cgModel != "" && cgModel != c.model {
			if p2, ok := c.pricer.Price(context.Background(), cgModel); ok {
				cgPrice, cgPriced = p2, true
			}
		}
		if cgPriced {
			e.CGLLMCostUSD = cgPrice.Cost(cgIn, cgCacheRead, cgCacheWrite, cgOut)
		}
	}

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
	c.rec.RecordInventory(e.TenantID, e.SessionID, e.TS, c.inv, captureContent, e.Project)
	c.rec.Record(e)
}

// entryAgeMs is how old the provider's cache entry was when the request arrived: the gap since
// the last real request, cut short by a ping that refreshed the entry later than that request.
func entryAgeMs(sinceMs, tsMs int64, refreshedAt time.Time) int64 {
	if refreshedAt.IsZero() || sinceMs <= 0 {
		return sinceMs
	}
	if age := tsMs - refreshedAt.UnixMilli(); age >= 0 && age < sinceMs {
		return age
	}
	return sinceMs
}

func cacheAttributionTTLMs(provider, model string) int64 {
	ttl, kind := apply.CacheLifetime(bschemas.ModelProvider(provider), model, nil)
	if kind != apply.CacheLifetimeExact {
		return 0
	}
	return ttl.Milliseconds()
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
	if c == nil {
		return nil
	}
	return &LiveFacts{Messages: c.trace.Messages, Tools: c.meta.Tools, SystemBlocks: c.meta.SystemBlocks, MaxTokens: c.meta.MaxTokens,
		Effort: c.meta.ReasoningEffort, Thinking: c.meta.ThinkingMode, CacheTTL: c.trace.CacheTTL, Agent: c.agent, Stream: c.meta.Stream, TTFBMs: ttfbMs, Arrival: c.predArrival}
}
