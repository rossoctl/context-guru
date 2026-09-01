package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/modelinfo"
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
	// kaPings / kaRefreshed / kaStrategy are what the idle keep-alive did during the span
	// this request just ended: how many pings it sent, how many tokens the last of them
	// read from cache, and which manager-controlled strategy (if any) resolved the policy
	// that sent them. Recorded on the request path because the keeper's per-session state
	// is cleared the moment a real request arrives, so this is the only point at which all
	// three facts are still in hand.
	kaPings     int
	kaRefreshed int64
	kaStrategy  string
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
func (c *capture) noteKeepAlive(pings int, refreshed int64, strategyID string) {
	if c != nil {
		c.kaPings, c.kaRefreshed, c.kaStrategy = pings, refreshed, strategyID
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
	e.Meta = c.meta
	e.FromTrace(c.trace, c.unique)
	// The provider's terminal reason, off the same response bytes the token tiers came
	// from. Present even when `usage` was not (see Usage.StopReason).
	e.StopReason = usage.StopReason
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
	seenSession, seenModel, sinceMs, tailChanged := c.rec.ObserveSplit(
		e.TenantID, e.SessionID, e.Model, e.TS, e.SplitTailHash)
	// Anthropic's prompt cache has a 5-minute TTL; a gap wider than that explains a
	// miss without blaming a prefix change (TTL wins ties).
	e.AttributeCache(seenSession, seenModel, sinceMs, 5*60*1000, e.CacheWrite > 0)
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
	c.rec.RecordInventory(e.TenantID, e.SessionID, e.TS, c.inv, captureContent)
	c.rec.Record(e)
}
