// Package proxy is the context-guru HTTP proxy: it runs the component pipeline
// on inbound chat requests, then forwards them to the configured upstream
// provider. It is the eval-containers gateway (exposes /openai + /anthropic on
// one port) and the standalone LLM-proxy integration.
//
// It reuses bifrost's ChatMessage type but not its transport: the transport
// can't inject an in-process Go plugin, so we drive the request path directly.
// Message rewriting is byte-lossless (headroom invariant I1) — only the
// `messages` array is re-serialized; every other field of the original body is
// preserved verbatim via sjson.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Options configures upstreams and credential injection. Each upstream is a base
// URL the matching route forwards to (the incoming path is appended). When a key
// is set it replaces the client's auth on forward — this is the eval-containers
// gateway model, where the agent holds only a placeholder key and the real
// provider key lives in the gateway env. Leave keys empty to pass the incoming
// auth through unchanged (local/dev use).
type Options struct {
	OpenAIUpstream    string // e.g. https://api.openai.com
	AnthropicUpstream string
	// BobUpstream, when set, enables the Bob (BobShell) gateway: Bob's
	// OpenAI-dialect model calls (POST /inference/v1/chat/completions) are reduced
	// and forwarded here, and every other path Bob calls (control-plane:
	// /admin/v1/profile, /inference/v1/model/info, …) is proxied through verbatim
	// so the CLI boots and authenticates. Point Bob's CUSTOM_BASE_URL at this proxy.
	BobUpstream  string // e.g. https://api.us-east.bob.ibm.com
	OpenAIKey    string // injected as Authorization: Bearer <key>
	AnthropicKey string // injected as x-api-key: <key>
	// ForceModel, when set, overwrites the request's "model" field. eval-containers
	// uses this to pin every call to EVAL_MODEL regardless of what the agent asked for.
	ForceModel string
	Client     *http.Client
	// CheapModel is the static "config"-source LLM client for NeedsModel
	// components (nil = none). The "incoming"-source client is built per request
	// from the route's upstream + the gateway's real key.
	CheapModel components.Model
	// Windows resolves a model's context window (max input tokens) so fraction-based
	// Trigger thresholds scale with the model. nil => window unknown (0), fractions
	// ignored, absolutes apply. Built in main from internal/modelinfo.
	Windows interface {
		Window(ctx context.Context, model string) (int, bool)
	}
	// InjectExpand controls advertising the context_guru_expand tool on outgoing
	// requests so Offload markers are actually recoverable (expand.InjectAuto |
	// InjectAlways | InjectNever). Empty defaults to auto. auto injects only when the
	// request already declares tools, carries an expandable marker, and the store
	// persists — safe for any agent.
	InjectExpand string
	// CacheMode controls cache-aware compaction ("auto"|"on"|"off"; empty=auto).
	// auto/on keep offloaders from mutating already-cached content on prompt-caching
	// backends; off restores legacy compact-everything (for confirmed non-caching backends).
	CacheMode string
	// Prices resolves a model's per-token rates so the dashboard can price each
	// request AT WRITE TIME (so history does not reprice when a rate changes). nil =
	// no pricing, and every captured row is marked partially accounted rather than
	// being priced as free. Built in main from internal/modelinfo.
	Prices modelinfo.Pricer
	// Preset labels captured rows with the configuration in effect, so the dashboard
	// can filter and compare by preset.
	Preset string
	// Dashboard, when non-nil, enables the persistent observability layer: each
	// request is captured off the hot path into a durable store and the dashboard UI
	// + API are mounted. nil = the proxy behaves exactly as before.
	Dashboard *dash.Recorder

	// PipelineFor builds a pipeline for a per-request override on /compact
	// (?preset=… or x-context-guru-pipeline: a,b,c). nil = overrides ignored, the
	// handler always uses the configured pipeline. Supplied by main (which holds
	// the config + emitter) so proxy stays decoupled from the config package.
	PipelineFor func(preset string, names []string) (*components.Pipeline, error)
	// Mode is the operating mode (#31): components.ModeSync (default, and byte-identical
	// to pre-mode behavior) or ModeObserve. Empty = sync. Explicit by design — never
	// inferred from the rest of the configuration.
	Mode components.Mode
	// Observe tunes observe mode's off-path measurement. Ignored in sync mode.
	Observe ObserveOptions

	// Tenants, when non-nil, makes this a HOSTED multi-tenant proxy: every chat
	// request must carry a token that resolves to a tenant, and that tenant's
	// configuration — not Options — decides the pipeline, the state store, the
	// operating mode and which upstream the traffic goes to. nil keeps the
	// single-tenant behaviour byte-identical to before tenancy existed.
	Tenants *TenantSource
	// Upstreams is the operator's allow-list, by name, consulted only in hosted
	// mode. A tenant selects a NAME; it can never supply a URL.
	Upstreams map[string]Upstream
	// Limits bounds what one tenant can consume of the shared box: request rate,
	// in-flight concurrency, and (process-wide) concurrent compaction-model calls.
	// Zero values disable each bound. Ignored in single-tenant mode, where there is
	// nobody to protect anyone from.
	Limits Limits
	// Spend reports a tenant's month-to-date cost, for the monthly cap. nil disables
	// cap enforcement — which is only safe when the upstream credential is not shared.
	Spend SpendChecker
	// PresetNames and ComponentNames are what the settings page may offer. Supplied by
	// the host because the registries live in `config` and `components`, and this
	// package stays decoupled from both.
	PresetNames    []string
	ComponentNames []string
	// MetricsToken, when set, lets a remote Prometheus scrape /metrics with a bearer
	// token. Empty means loopback only in hosted mode.
	MetricsToken string
	// TenantMetrics supplies per-tenant rollups for /metrics. nil = process-wide
	// series only.
	TenantMetrics TenantMetricsSource
	// Version is reported as a label on cg_build_info.
	Version string
	// StatsTrusted reports whether a request may read /stats, which is a
	// PROCESS-WIDE aggregate across every tenant. In hosted mode this must be
	// restricted (loopback or a manager); nil means loopback only. Ignored in
	// single-tenant mode, where /stats stays open as it always was — the benchmark
	// harnesses in deploy/harbor read it.
	StatsTrusted func(*http.Request) bool
}

// ObserveOptions tunes observe mode: one option per real decision.
type ObserveOptions struct {
	// MaxQueue bounds the off-path measurement queue; a full queue DROPS (counted) rather
	// than blocking the request path. 0 = modes.DefaultMaxQueue.
	MaxQueue int `yaml:"max_queue"`
	// Workers is the number of drain goroutines. 0 = modes.DefaultWorkers (1), which keeps
	// one measurement's cheap-model call in flight per process.
	Workers int `yaml:"workers"`
}

// upstream binds a provider to its base URL, the canonical provider path to POST
// to (decoupled from the gateway's incoming /openai|/anthropic namespace), and a
// credential injector.
type upstream struct {
	base   string
	path   string
	setKey func(http.Header)
}

// Handler serves the proxy + management routes.
type Handler struct {
	pipe   *components.Pipeline
	store  store.Store
	agg    *metrics.Aggregator
	opts   Options
	client *http.Client
	// tracker owns the per-session cached-prefix boundary. Always present: every mode
	// benefits from reading and recording it in one locked step (the previous
	// read-then-deferred-write raced between concurrent turns of a session).
	tracker *modes.Tracker
	// pool runs off-path measurements. nil in sync mode — there are none.
	pool *modes.Pool
	// observeSeq numbers observations so each request enqueues one.
	observeSeq atomic.Uint64
	// shadow is observe mode's own state store, separate from the live one. Observe must
	// not write into the live store — a real request would then replay a decision that was
	// never enforced — but it also cannot simply discard its writes: offloaders FREEZE a
	// decision and replay it on every later turn, which is where most of the sustained
	// saving comes from. Throwing that away each turn makes observe see only the current
	// tail and UNDER-project by ~3x against what sync achieves.
	//
	// So observe gets a store of its own: as persistent as the live one, and completely
	// disjoint from it.
	shadow store.Store
	// rec is the dashboard's capture pipeline (nil when the dashboard is off). Every
	// use is nil-guarded, so the disabled path costs one nil check per request.
	rec *dash.Recorder
	api *dash.API
	// static is the single-tenant Tenancy: the handler's own pipeline, store and
	// mode presented through the same type the hosted path uses. Built once so the
	// request path has exactly ONE way to reach a pipeline, rather than a branch at
	// every use — which is how a hosted deployment would eventually leak the shared
	// one into a tenant's request.
	static *Tenancy
	// limiter enforces Options.Limits. Always non-nil; a zero Limits makes it a no-op.
	limiter *Limiter
	// regLim bounds self-registration attempts per client address. Separate from
	// limiter because its keys are IPs rather than tenant ids, and its bound is a
	// fixed property of what registration is, not an operator setting.
	regLim *Limiter
	// spend memoises month-to-date cost lookups so the cap costs one query a minute
	// per tenant rather than one per request.
	spend *spendCache
	// promCache memoises the Prometheus body for a scrape interval; the per-tenant
	// series cost a SQL query and Grafana scrapes every few seconds.
	promCache promCache
}

// New builds the proxy handler. agg may be nil (no /stats rollups).
func New(pipe *components.Pipeline, st store.Store, agg *metrics.Aggregator, opts Options) *Handler {
	c := opts.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	h := &Handler{pipe: pipe, store: st, agg: agg, opts: opts, client: c,
		tracker: modes.NewTracker(0), rec: opts.Dashboard}
	if h.mode() == components.ModeObserve {
		h.pool = modes.NewPool(opts.Observe.MaxQueue, opts.Observe.Workers)
		h.shadow = store.NewMemory(store.Options{})
	}
	// The single-tenant view of this handler's own configuration. In hosted mode it
	// is never consulted (tenancyFor goes to Options.Tenants); it exists so the
	// request path has one shape.
	h.static = &Tenancy{Preset: opts.Preset, Pipe: pipe, Store: st,
		Shadow: h.shadow, Mode: h.mode()}
	h.limiter = NewLimiter(opts.Limits)
	h.regLim = newAnonLimiter(Limits{RequestsPerMinute: registrationsPerMinute})
	h.spend = newSpendCache(0)
	if agg != nil {
		agg.SetMode(h.mode())
	}
	if h.rec != nil {
		h.api = dash.NewAPI(h.rec)
		// Publish the off-path pool's counters to the dashboard, the same layering /stats
		// uses: the pool lives in `modes`, which sits above both `metrics` and `dash`, so
		// the host is the only place that can join them. Read at serve time, not captured
		// now, and left unset in sync mode so the UI shows no phantom queue.
		if h.pool != nil {
			pool := h.pool
			h.rec.SetObserveQueue(func() dash.QueueStats {
				q := pool.Stats()
				return dash.QueueStats{Queued: q.Queued, Pending: q.Pending,
					Processed: q.Processed, Dropped: q.Dropped, Errors: q.Errors}
			})
		}
	}
	return h
}

// Close shuts down the off-path worker pool and waits briefly for its goroutines to exit,
// so a host that builds and discards handlers (tests, a reload) leaks none. Safe on a
// sync-mode handler and safe to call twice.
func (h *Handler) Close() { h.pool.Stop() }

// Mux wires the routes: chat proxying + health/stats/expand management.
func (h *Handler) Mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /openai/v1/chat/completions", h.chat(bschemas.OpenAI, upstream{
		base:   h.opts.OpenAIUpstream,
		path:   "/v1/chat/completions",
		setKey: bearerKey(h.opts.OpenAIKey),
	}, pickOpenAI))
	m.HandleFunc("POST /anthropic/v1/messages", h.chat(bschemas.Anthropic, upstream{
		base:   h.opts.AnthropicUpstream,
		path:   "/v1/messages",
		setKey: headerKey("x-api-key", h.opts.AnthropicKey),
	}, pickAnthropic))
	m.HandleFunc("POST /compact", h.compact)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	// A browser asks for /favicon.ico unprompted, and in hosted mode Bob's "/" catch-all
	// would otherwise answer it with a 401 — putting a red error in the console of every
	// dashboard user on every page load, which reads as "this thing is broken". Answering
	// 204 here beats explaining it: this pattern is more specific than "/", so it wins.
	m.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("GET /stats", h.stats)
	// Prometheus, for Grafana. Same gate as /stats: it is a service-wide view that
	// includes per-tenant cost.
	m.HandleFunc("GET /metrics", h.metricsHandler)
	m.HandleFunc("GET /expand", h.expand)
	// The dashboard mounts /dashboard/ (embedded UI) and /api/* (JSON + SSE) only
	// when enabled, so an unconfigured proxy's route table is byte-identical to before.
	if h.api != nil {
		h.api.Mount(m)
	}
	// Control plane: registration, sign-in, settings, tokens, the manager's roster.
	// Mounted after the dashboard so both live under /api/, and only in hosted mode.
	h.MountControl(m)
	// Bob (BobShell) gateway. Bob is OpenAI-compatible but calls Bob-specific
	// paths: its model call is POST /inference/v1/chat/completions (reduced like
	// any OpenAI chat), and its control-plane calls (/admin/v1/profile,
	// /inference/v1/model/info, …) must pass through verbatim so the CLI boots and
	// authenticates. The "/" catch-all is less specific than every route above, so
	// it only receives what nothing else matched. Enabled only when BobUpstream is
	// set, so default proxy behavior (unknown path => 404) is unchanged.
	if h.opts.BobUpstream != "" || h.opts.Tenants != nil {
		m.HandleFunc("POST /inference/v1/chat/completions", h.chat(bschemas.OpenAI, upstream{
			base: h.opts.BobUpstream,
			path: "/inference/v1/chat/completions",
			// setKey nil in single-tenant mode: pass Bob's own auth (BOBSHELL key)
			// straight through. In hosted mode upstreamFor always injects, because the
			// client's header holds OUR token, which must not leave the box.
		}, pickBob))
		m.HandleFunc("/", h.passthrough(h.opts.BobUpstream))
	}
	return m
}

// passthrough transparently forwards a request to the Bob upstream unchanged —
// for Bob's control-plane calls that must not be rewritten. Bob's own auth
// header passes straight through (no key injection); the response is streamed
// back as-is. Only the model route is reduced; everything else Bob calls lands
// here and is proxied verbatim.
func (h *Handler) passthrough(base string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		up := upstream{base: base}
		// Hosted mode: authenticate and resolve the tenant's Bob endpoint, then inject
		// the real credential. Without this, the catch-all would be an open forwarder
		// AND would send the caller's header — our token — to Bob.
		if h.opts.Tenants != nil {
			tn, err := h.tenancyFor(r)
			if err != nil {
				failAuth(w, err)
				return
			}
			if up, err = h.upstreamFor(tn, pickBob, upstream{}); err != nil {
				failAuth(w, err)
				return
			}
		}
		if up.base == "" {
			recordRefusal(refuseNoUpstream, "")
			http.Error(w, "no upstream configured", http.StatusBadGateway)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, _ := io.ReadAll(r.Body)
		target := up.base + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		copyHeaders(req.Header, r.Header)
		if up.setKey != nil {
			for _, hd := range tokenHeaders {
				req.Header.Del(hd)
			}
			up.setKey(req.Header)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			recordRefusal(refuseUpstream, "")
			// Log the detail, return a fixed string — see the hosted path below for why
			// err.Error() must not reach the caller (it embeds the upstream URL).
			slog.Warn("context-guru: upstream call failed", "err", err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			return
		}
		h.stream(w, resp)
	}
}

// compact runs the pipeline over the request body's messages and returns the
// rewritten body — without forwarding upstream. This is the "compact a context,
// hand it back" endpoint: a caller (e.g. the llm-d-router request-inline-
// compaction step) POSTs an inference request body and gets a smaller body of
// the same shape back. Fail-open: any parse/serialize trouble returns the
// original body with 200, so the caller's passthrough contract always holds.
//
// Provider defaults to OpenAI; ?provider=anthropic switches dialects. Config
// overrides (when Options.PipelineFor is set): ?preset=<name> or header
// x-context-guru-pipeline: comp1,comp2. Session/bypass honor the usual headers.
func (h *Handler) compact(w http.ResponseWriter, r *http.Request) {
	// Hosted mode authenticates this endpoint too. It runs the pipeline over a
	// caller-supplied transcript and can invoke the cheap model, so leaving it open
	// would be both an unmetered compute endpoint and a way to write into whichever
	// state store it happened to reach.
	tn, err := h.tenancyFor(r)
	if err != nil {
		failAuth(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	provider := bschemas.OpenAI
	if strings.EqualFold(r.URL.Query().Get("provider"), "anthropic") {
		provider = bschemas.Anthropic
	}

	pipe := tn.Pipe
	if h.opts.PipelineFor != nil {
		preset := r.URL.Query().Get("preset")
		var names []string
		if hp := r.Header.Get("x-context-guru-pipeline"); hp != "" {
			names = splitComma(hp)
		}
		if preset != "" || len(names) != 0 {
			// ponytail: rebuild per override request; add an LRU cache if override QPS ever matters.
			if p, err := h.opts.PipelineFor(preset, names); err == nil {
				pipe = p
			} // build error => fall back to the configured pipeline (fail open)
		}
	}

	// No upstream here, so there is no "incoming" model; only the static
	// "config"-source client (and any endpoint pinned in a component's model: block).
	models := components.ModelSpec{Static: h.opts.CheapModel}
	// Honor the same cache-aware behavior as the live chat path so /compact (used by
	// offline replay/eval) reflects production. ?cache=on|off|auto overrides for A/B.
	cacheMode := h.opts.CacheMode
	if q := r.URL.Query().Get("cache"); q != "" {
		cacheMode = q
	}
	// Resolve the model's context window here too, exactly as the chat path does. It used
	// to be hard-coded 0 ("unknown"), which silently disabled every fraction-based Trigger
	// threshold AND extract_llm's context-pressure triggering on this endpoint — so
	// /compact did not reflect production, and offline replay/eval measured a different
	// component than the one that ships.
	window := 0
	if h.opts.Windows != nil {
		if w, ok := h.opts.Windows.Window(r.Context(), gjson.GetBytes(body, "model").String()); ok {
			window = w
		}
	}
	// Use the SAME boundary tracker as the chat path. /compact used to fall through to
	// apply's legacy store-backed prevLen, which the chat path had already moved off, so this
	// endpoint kept two properties the chat path had shed: concurrent turns of one session
	// race on a read-then-deferred-write, and the boundary lives in a store key (`cg:len:`)
	// that competes for the pin budget instead of in memory.
	//
	// The motivation is measurement rather than a live cache regression: /compact is the
	// offline replay/eval endpoint, so a boundary derived differently from production means
	// eval measures a different component than the one that ships. That is the same class of
	// divergence as the window this handler used to hard-code as unknown, a few lines above —
	// and it went unnoticed for the same reason, because both are silent.
	cp := h.newCapture(r, string(provider), "/compact", tn)
	cp.noteModel(gjson.GetBytes(body, "model").String())
	start := time.Now()
	res := apply.BodyOpts(r.Context(), pipe, tn.Store, apply.Opts{
		Provider:  provider,
		Body:      body,
		Session:   r.Header.Get("x-context-guru-session"),
		Tenant:    tn.ID,
		Bypass:    strings.EqualFold(r.Header.Get("x-context-guru-bypass"), "true"),
		Models:    models,
		Window:    window,
		CacheMode: cacheMode,
		Tracker:   h.tracker,
	})
	cp.noteCG(float64(time.Since(start).Microseconds()) / 1000.0)
	cp.noteTrace(res.Trace)
	w.Header().Set("Content-Type", "application/json")
	w.Write(res.Body)
	// After the response: /compact never calls a provider, so there is no usage to
	// report and the row is honestly marked partially accounted.
	cp.finish(Usage{}, false, h.captureContentFor(tn), h.contentCap(), h.contentMax())
}

// splitComma splits a comma-separated header value into trimmed, non-empty names.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func bearerKey(key string) func(http.Header) {
	if key == "" {
		return nil
	}
	return func(h http.Header) { h.Set("Authorization", "Bearer "+key) }
}

func headerKey(name, key string) func(http.Header) {
	if key == "" {
		return nil
	}
	return func(h http.Header) { h.Set(name, key) }
}

// incomingModel builds an LLM client that reuses the proxied request's own model
// and the route's upstream + credential, so a NeedsModel component can call the
// same backend the request targets. Prefers the gateway's injected key (gateway
// mode); falls back to the client's own auth header (pass-through). Returns nil
// when no upstream/model/key is resolvable, and the component degrades.
func (h *Handler) incomingModel(provider bschemas.ModelProvider, up upstream, body []byte, r *http.Request) components.Model {
	if up.base == "" {
		return nil
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		return nil
	}
	switch provider {
	case bschemas.Anthropic:
		key := h.opts.AnthropicKey
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			return nil
		}
		return cheapmodel.Anthropic{BaseURL: up.base, Model: model, APIKey: key, Client: h.client}
	case bschemas.OpenAI:
		key := h.opts.OpenAIKey
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			return nil
		}
		return cheapmodel.OpenAI{BaseURL: up.base, Model: model, APIKey: key, Client: h.client}
	}
	return nil
}

// capturePath, when set (CONTEXT_GURU_CAPTURE), names a JSONL file the proxy
// appends each inbound request to (for offline replay-based component evaluation).
var capturePath = os.Getenv("CONTEXT_GURU_CAPTURE")

// captureRequest appends {provider, model, body} for one request. Best-effort;
// any error is ignored (never affects request handling).
func captureRequest(provider string, body []byte) {
	if capturePath == "" {
		return
	}
	f, err := os.OpenFile(capturePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{
		"provider": provider,
		"model":    gjson.GetBytes(body, "model").String(),
		"body":     json.RawMessage(body),
	})
	f.Write(append(rec, '\n'))
}

// pick* select which of a tenant's upstream names applies to a route. One closure
// per route beats a dialect enum: the route already knows, and the compiler checks
// the field name.
func pickAnthropic(t *Tenancy) string { return t.UpAnthropic }
func pickOpenAI(t *Tenancy) string    { return t.UpOpenAI }
func pickBob(t *Tenancy) string       { return t.UpBob }

func (h *Handler) chat(provider bschemas.ModelProvider, static upstream, pick func(*Tenancy) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authenticate FIRST, before reading a body or doing any work. In hosted mode
		// an unauthenticated caller must not be able to make the proxy buffer 32 MiB.
		tn, err := h.tenancyFor(r)
		if err != nil {
			failAuth(w, err)
			return
		}
		up, err := h.upstreamFor(tn, pick, static)
		if err != nil {
			failAuth(w, err)
			return
		}
		// Limits and the spend cap, before the body is read. Refusing a request that
		// would exceed a bound must not first cost us 32 MiB of buffering.
		release, err := h.limiter.Acquire(tn.ID)
		defer release()
		if err != nil {
			failAuth(w, err)
			return
		}
		if err := h.checkSpend(tn); err != nil {
			failAuth(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Pin the model if configured (eval-containers EVAL_MODEL).
		if h.opts.ForceModel != "" {
			if out, err := sjson.SetBytes(body, "model", h.opts.ForceModel); err == nil {
				body = out
			}
		}
		// Optional request capture (CONTEXT_GURU_CAPTURE): append the pristine inbound
		// body (post force-model, pre-pipeline) as one JSONL record per request, so an
		// external evaluator can REPLAY the exact traffic through any pipeline via
		// /compact — identical requests, no agent nondeterminism, no repeated LLM spend.
		captureRequest(string(provider), body)
		// Rewrite the messages via the shared apply path; fail open. Supply the
		// LLM clients NeedsModel components may call: the per-request "incoming"
		// model (the route's upstream + the gateway's real key + the request's
		// model) and the static "config" cheap model.
		models := components.ModelSpec{
			Incoming: h.incomingModel(provider, up, body, r),
			Static:   h.opts.CheapModel,
		}
		// Resolve the model's context window (dynamic, cached) so fraction-based
		// triggers scale with the model; 0 when unknown (absolutes apply).
		window := 0
		if h.opts.Windows != nil {
			if w, ok := h.opts.Windows.Window(r.Context(), gjson.GetBytes(body, "model").String()); ok {
				window = w
			}
		}
		bypassed := strings.EqualFold(r.Header.Get("x-context-guru-bypass"), "true")
		// The agent's OWN compaction request rides the same route. Bypass it exactly as the
		// header does — compacting it destroys content the summary is supposed to carry
		// verbatim, and the loss is unrecoverable once the summary replaces the transcript
		// (see isAgentCompaction). Counted as a gate on a `bypass` pseudo-component so
		// /stats shows components.bypass.gates.agent_compaction — a silent bypass is
		// indistinguishable from a broken pipeline.
		if !bypassed && isAgentCompaction(body) {
			bypassed = true
			if h.agg != nil {
				rep := components.Report{Component: "bypass", Skipped: true, Mode: tn.Mode}
				rep.Gate("agent_compaction")
				h.agg.Component(rep)
			}
		}
		// Start the dashboard capture (nil when the dashboard is off). It only holds
		// values the request path already computed; nothing here does I/O.
		cp := h.newCapture(r, string(provider), up.path, tn)
		cp.noteModel(gjson.GetBytes(body, "model").String())
		// The request's own metadata (effort, thinking, sampling, tool_choice, shape),
		// read from the PRISTINE body in one pass before the pipeline touches it.
		cp.noteMeta(metaFromBody(body))
		// Fail open around the whole pre-forward rewrite (pipeline + expand injection): a
		// panic anywhere here must forward the PRISTINE inbound body, never 500 the client.
		// apply.BodyFull has its own recover; this backstops expand.Inject and anything else.
		orig := body
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("context-guru: recovered from panic before forward; sending original", "panic", rec)
					body = orig
				}
			}()
			var added time.Duration
			var tr apply.Trace
			body, added, tr = h.applyMode(&reqInfo{
				ctx:      r.Context(),
				provider: provider,
				body:     body,
				session:  r.Header.Get("x-context-guru-session"),
				bypassed: bypassed,
				models:   models,
				window:   window,
				tn:       tn,
			})
			addedMs := float64(added.Microseconds()) / 1000.0
			cp.noteCG(addedMs)
			cp.noteTrace(tr)
			if h.agg != nil && !bypassed {
				h.agg.RecordAddedLatency(addedMs)
				h.agg.RecordEligibility(tr.AttemptedTokens, tr.FrozenTokens)
			}
			// Advertise the expand tool so the model can recover any offloaded content
			// (closes the reversibility loop h.serve drives). Sticky/idempotent + appended
			// last to keep the provider prefix cache warm; gated by InjectExpand + store.
			// Skipped in observe mode: nothing was offloaded, so there is nothing to
			// recover, and injecting a tool declaration would MODIFY the request — which is
			// precisely the one thing observe mode promises never to do.
			// Skipped on a bypassed request too: bypass promises a byte-identical forward,
			// nothing was offloaded on this turn, and on an agent-compaction request an
			// advertised expand tool is actively harmful — the summarizer may call it, and a
			// tool_use with no text replayed to the client counts as a FAILED compaction
			// (three of those and Claude Code disables auto-compact for the session).
			if tn.Mode != components.ModeObserve && !bypassed {
				im := h.opts.InjectExpand
				if im == "" {
					im = expand.InjectAuto
				}
				body, _ = expand.Inject(string(provider), im, body, tn.Store.Persists())
			}
		}()
		h.serve(w, r, provider, up, body, bypassed, cp, tn)
	}
}

// API exposes the dashboard's HTTP surface so the host can attach an authenticator.
// nil when the dashboard is off.
func (h *Handler) API() *dash.API { return h.api }

// maxExpandRounds caps the expand continuation loop (headroom's default).
const maxExpandRounds = 3

// maxRequestBytes caps an inbound request body so a single huge POST can't exhaust
// proxy memory (the body is buffered and token-counted several times). Generous
// enough for very long agent transcripts; requests above it get 413.
const maxRequestBytes = 32 << 20 // 32 MiB

// The expand tool is advertised on the outgoing request by expand.Inject (called
// in chat, gated by Options.InjectExpand + store.Persists), appended last and
// idempotently so the provider prefix cache stays byte-stable across turns. That
// closes the loop: the marker text tells the model to call context_guru_expand, and
// the tool is now actually declared, so the continuation loop below can fire.

var errNoUpstream = errors.New("no upstream configured")

// serve forwards the request and runs the expand continuation loop: if the model
// calls the expand tool (and ONLY that tool), resolve the originals from the store,
// append the tool-result turn, and re-invoke upstream — up to a few rounds.
//
// Streaming (SSE) works too: when the outgoing request carries expandable markers
// (`<<cg:…>>`, i.e. offload has happened, so the model MIGHT call expand), the SSE
// response is buffered and reconstructed so the loop can inspect it. If it is not a
// lone expand call, the buffered SSE bytes are replayed to the client verbatim (a
// one-time latency cost, no correctness change). Requests without markers — early in
// a session — stream straight through with zero added latency and no possible expand.
//
// Buffering is the only thing that stops a stream being a stream, so the marker test
// is scoped to the model-visible content and both outcomes are counted (agg.RecordSSE
// → /stats sse_streamed / sse_buffered). It previously matched the expand tool
// description this proxy injects itself, so it was unconditionally true and the
// zero-added-latency promise above never held for any request (issue #26).
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, provider bschemas.ModelProvider, up upstream, body []byte, bypassed bool, cp *capture, tn *Tenancy) {
	// ONE condition governs both halves of the loop: the tool is intercepted exactly when
	// it is advertised on the outgoing request. Those used to be different conditions —
	// advertised when the request had tools, intercepted (for SSE) when it had markers —
	// so a marker-free tools-bearing request declared a tool whose use then streamed
	// straight to a client that has no such tool. Reading the outgoing body rather than
	// trusting Inject's return value also covers a request that already carried the tool.
	//
	// Since injection now requires markers (expand.Inject, InjectAuto), this keeps the
	// documented fast path: no offload yet → no tool → no buffering, zero added latency.
	advertised := expand.HasTool(string(provider), body)
	// SSE accounting is PER CLIENT REQUEST, not per upstream round: one client request
	// that drives several expand rounds waited for all of them, so timing a single
	// round would report a healthy TTFB for a client that waited three round-trips.
	// Recorded in a defer so every terminal return path is covered exactly once —
	// stream-through, aggregate-failure replay, normal-answer replay, nothing-resolved
	// replay, and the round-cap exit.
	reqStart := time.Now()
	sse, sseBuffered := false, false
	var sseFirstByte time.Time // zero on buffered paths: the client's first byte is the write itself
	// The response's billed token tiers, harvested out of band (see sniffer) and
	// handed to the dashboard AFTER the client's response is complete, so capture
	// can never delay or fail a request.
	var usage Usage
	var usageOK bool
	defer func() {
		if sse && h.agg != nil {
			h.agg.RecordSSE(msSince(reqStart, sseFirstByte), sseBuffered)
		}
		if h.agg != nil && usageOK {
			h.agg.RecordUsage(usage.FreshInput, usage.CacheRead, usage.CacheWrite, usage.Output)
		}
		cp.finish(usage, usageOK, h.captureContentFor(tn), h.contentCap(), h.contentMax())
	}()
	for round := 0; ; round++ {
		upStart := time.Now()
		resp, err := h.doUpstream(r, up, body)
		if err != nil {
			// LOG it, and record it on the captured row. An upstream failure used to be
			// invisible in both places: the caller got a 502 and the operator got nothing
			// — no log line, and a dashboard row with status 0 that reads as "unknown"
			// rather than "the upstream refused". On a shared service that is the
			// difference between debugging a report and disbelieving it.
			slog.Warn("context-guru: upstream call failed", "tenant", tn.ID,
				"route", up.path, "upstream", up.base, "round", round, "err", err)
			cp.noteUpstream(float64(time.Since(upStart).Microseconds())/1000.0,
				http.StatusBadGateway)
			// And count it: this path does not go through failAuth, so without this the
			// only 502s on the dashboard would be our own misconfiguration.
			recordRefusal(refuseUpstream, tn.ID)
			// The client gets a fixed string, NOT err.Error(). http.Client returns a
			// *url.Error, which stringifies as `Post "<the full upstream URL>": ...` —
			// so echoing it hands every caller the operator's upstream address, and if
			// that URL ever carries userinfo, the credential with it. checkBaseURL
			// rejects userinfo for hosted upstreams, but the single-tenant
			// --anthropic-upstream/--openai-upstream flags are not validated, so this
			// is the one place that would publish it. The detail is in the log line
			// directly above, where the operator can see it and the caller cannot.
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			return
		}
		upMs := float64(time.Since(upStart).Microseconds()) / 1000.0
		cp.noteUpstream(upMs, resp.StatusCode)
		if h.agg != nil {
			h.agg.RecordUpstreamLatency(upMs, bypassed)
		}
		isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
		// Inspect for a lone expand call when the tool is actually advertised and we haven't
		// hit the round cap. Nothing else can produce a call to it, so this is both the
		// necessary and the sufficient condition — and for SSE it is what decides whether we
		// pay the buffering cost.
		checkExpand := advertised && round < maxExpandRounds
		if !checkExpand {
			// sseBuffered is sticky: if an earlier round was buffered the client already
			// lost its stream, so this request counts as buffered however it ends.
			sse = sse || isSSE
			// Stream straight through, sniffing usage from a bounded head+tail window as
			// the bytes go by (no buffering of the whole response, no added latency).
			first, u, ok := h.stream(w, resp)
			if !sseBuffered {
				sseFirstByte = first
			}
			if ok {
				usage, usageOK = u, true
			} else {
				// No billed tiers, but the response may still have said WHY it stopped. The
				// two facts are independent (see Usage.StopReason), and dropping the reason
				// here would blank stop_reason for exactly the rows already marked partial.
				usage.StopReason = u.StopReason
			}
			return
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if isSSE {
			// Buffered: the client sees nothing until the whole stream has arrived, so its
			// first byte lands no earlier than the write on whichever path we return from.
			sse, sseBuffered, sseFirstByte = true, true, time.Time{}
		}
		if u, ok := responseUsage(resp.Header.Get("Content-Type"), respBody); ok {
			usage, usageOK = u, true
		} else {
			usage.StopReason = u.StopReason // same reasoning as the stream path above
		}

		// Reconstruct the message the loop reasons over. For SSE, aggregate the events;
		// if that fails, replay the raw stream unchanged (fail-open).
		msg := respBody
		if isSSE {
			agg, ok := expand.AggregateSSE(string(provider), respBody)
			if !ok {
				writeRaw(w, resp, respBody)
				return
			}
			msg = agg
		}

		calls, otherTools := expand.ResponseCalls(string(provider), msg)
		if len(calls) == 0 || otherTools {
			writeRaw(w, resp, respBody) // normal answer (or other tools) — replay verbatim
			return
		}
		// Build a tool_result for EVERY expand call — the provider requires one per
		// tool_call_id or the continuation is malformed. Expired/unknown ids get an
		// explicit placeholder rather than being omitted.
		resolved := map[string]string{}
		got := 0
		for _, c := range calls {
			if orig, ok := expand.Resolve(tn.Store, c.HashID); ok {
				resolved[c.CallID] = orig
				got++
				// The agent needed this content back — don't re-compact it on later turns
				// (that would loop it straight back into another expand). Keep it verbatim.
				offload.MarkKeptVerbatim(tn.Store, orig)
				back := schema.TextTokens(orig)
				if h.agg != nil {
					h.agg.RecordExpand(back) // bounce: offload had to come back
				}
				cp.noteExpand(back) // and on the dashboard row, or SavedAdjusted over-reports
			} else {
				resolved[c.CallID] = "[expand: original for id " + c.HashID + " is no longer available]"
			}
		}
		next, ok := expand.Continuation(string(provider), body, msg, resolved)
		if got == 0 || !ok {
			writeRaw(w, resp, respBody) // nothing recovered; return the model's own call
			return
		}
		body = next // loop: re-invoke with the originals in hand
	}
}

// writeRaw replays a fully-buffered upstream response (SSE or JSON) to the client
// with its original headers/status — byte-for-byte, flushing for SSE.
func writeRaw(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) doUpstream(r *http.Request, up upstream, body []byte) (*http.Response, error) {
	if up.base == "" {
		return nil, errNoUpstream
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, up.base+up.path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	if up.setKey != nil {
		// Gateway mode: drop the client's placeholder auth, inject the real key. Every
		// slot the tenant resolver reads a token from is dropped, not just the two
		// providers use — a slot we accept a credential in is a slot that must never
		// reach an upstream, and in a hosted deployment that credential is the token
		// WE minted.
		for _, hd := range tokenHeaders {
			req.Header.Del(hd)
		}
		up.setKey(req.Header)
	}
	return h.client.Do(req)
}

// stream copies an upstream response through with flushing (SSE-friendly), while
// keeping a BOUNDED head+tail window of the bytes so the response's billed token
// tiers can be read afterwards. The window is why observability costs nothing
// here: no whole-response buffering, no extra pass, and the client sees each chunk
// as soon as it arrives.
//
// head+tail rather than tail alone because Anthropic reports the input tiers in
// the FIRST SSE event (message_start) and the output count in the last, while
// OpenAI reports everything in a final chunk.
//
// It also returns the instant the client got its first byte (zero if the body was
// empty), which is the SSE TTFB accounting in serve.
func (h *Handler) stream(w http.ResponseWriter, resp *http.Response) (firstByte time.Time, u Usage, ok bool) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flush, _ := w.(http.Flusher)
	sn := newSniffer(h.rec != nil || h.agg != nil)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if firstByte.IsZero() {
				firstByte = time.Now()
			}
			w.Write(buf[:n])
			sn.write(buf[:n])
			if flush != nil {
				flush.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	u, ok = responseUsage(resp.Header.Get("Content-Type"), sn.bytes())
	return firstByte, u, ok
}

// msSince returns milliseconds from start to at (falling back to now if the
// response carried no bytes).
func msSince(start, at time.Time) float64 {
	if at.IsZero() {
		at = time.Now()
	}
	return float64(at.Sub(start).Microseconds()) / 1000.0
}

// captureContent / contentCap / contentMax read the dashboard's content-capture
// settings, with safe zero values when the dashboard is off.
func (h *Handler) captureContent() bool {
	return h.rec != nil && h.rec.Opts().CaptureContent
}

// captureContentFor decides whether THIS request's transcript text may be stored.
// Two independent gates, both of which must pass.
//
// The operator's flag is the first: content capture writes agent output — source
// code, tool results — through a best-effort denylist redactor whose own review
// found credential shapes passing through, which is why it is opt-in at all.
//
// The tenant's consent is the second, and it exists only in hosted mode. On a
// shared service the operator enabling a feature is not the same act as a user
// agreeing to have their transcripts retained, so one flag cannot stand for both.
// Single-tenant, there is no second party to consent, so the operator's flag is
// the whole decision — unchanged from before tenancy existed.
func (h *Handler) captureContentFor(tn *Tenancy) bool {
	if !h.captureContent() {
		return false
	}
	if h.opts.Tenants == nil {
		return true
	}
	return tn != nil && tn.CaptureContent
}
func (h *Handler) contentCap() int {
	if h.rec == nil {
		return 0
	}
	return h.rec.Opts().ContentCap
}
func (h *Handler) contentMax() int {
	if h.rec == nil {
		return 0
	}
	return h.rec.Opts().ContentMaxPerRequest
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	// /stats is a PROCESS-WIDE aggregate over every tenant. Single-tenant it stays
	// open exactly as it was — deploy/harbor/*.py reads it, and a proxy showing its
	// own numbers is the point of the tool. Hosted, it is one tenant's traffic mixed
	// with everyone else's, so it needs a gate; loopback is the default because the
	// benchmark harnesses run on the same box as the proxy.
	if h.opts.Tenants != nil && !h.statsTrusted(r) {
		failAuth(w, statusError{http.StatusForbidden,
			"/stats is a service-wide aggregate; use the dashboard for your own traffic"})
		return
	}
	if h.agg == nil {
		w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	snap := h.agg.Snapshot()
	// Fill the CG components' own LLM cost (cheap-model usage) — kept out of the
	// metrics package (layering) and merged here at serve time.
	snap.LLMCalls, snap.LLMInputTokens, snap.LLMOutputTokens = cheapmodel.Usage()
	// Same layering rationale: the deadline and its counters live in the component
	// package, merged here rather than making `metrics` depend on `components/offload`.
	// Without llm_timeouts, a run whose compaction model kept hitting its ceiling is
	// indistinguishable from a run that had little to compact — the arm reads as fast
	// because it silently stopped working. llm_call_timeout_ms travels with the counts
	// because a timeout total is meaningless without the budget it was measured against.
	snap.LLMTimeouts = offload.LLMTimeouts()
	snap.LLMErrors = offload.LLMErrors()
	snap.LLMCallTimeoutMs = offload.LLMCallTimeout().Milliseconds()
	// Freeze-replay health, same layering: the counters live with the code that owns
	// them (offload for the replay path, the store for dropped/repaired decisions).
	snap.FrozenHits, snap.FrozenMisses = offload.FrozenStats()
	if fl, ok := h.store.(*store.Memory); ok { // process store; hosted per-tenant stores report via the dashboard
		snap.FrozenDropped, snap.FrozenRepaired = fl.FrozenLossStats()
		snap.FrozenFlips = snap.FrozenDropped - snap.FrozenRepaired
	}
	// Cached-prefix restarts after an agent compaction. Same layering as the pool counters
	// below: the counter lives in `modes`, so the host merges it here.
	snap.CompactionResets = modes.CompactionResets()
	// Off-path pool counters, same layering: the pool lives in `modes`, so `metrics` cannot
	// read it and the host merges it here. Without this the observe-mode docs describe a
	// `dropped` counter no consumer can reach — the pool tracked it correctly and nothing
	// served it.
	if h.pool != nil {
		q := h.pool.Stats()
		snap.ObserveQueue = &metrics.QueueStats{
			Queued: q.Queued, Pending: q.Pending,
			Processed: q.Processed, Dropped: q.Dropped, Errors: q.Errors,
		}
	}
	// extract_llm economics (#28 part F). Net-after-cost is the honest headline: the three
	// LLM* fields above report what the component SPENT, and until now nothing anywhere
	// compared that against what its savings were WORTH — which is how an 82x loss stayed
	// invisible. Computed here, the layer that knows the model pricing and the cache mode.
	// Purely additive: every pre-existing field keeps its name for deploy/harbor/*.py.
	cacheWrite, cacheRead := cheapmodel.CacheUsage()
	pricing := cheapmodel.PricingFromEnv()
	cost := pricing.Cost(snap.LLMInputTokens, snap.LLMOutputTokens, cacheWrite, cacheRead)
	// Value a saved token at the rate it would actually have been billed. On a caching
	// backend a removed token saves the cache-READ rate (~10x cheaper), which is exactly
	// why the component can be underwater while its token count looks impressive.
	//
	// Pass the RATE, not a pre-multiplied total: ExtractSnapshot applies it to
	// extract_llm's OWN gross_saved_tokens. Multiplying snap.SavedTokens here would price
	// the WHOLE pipeline's savings (format, dedup, cmdfilter, extract, …) against
	// extract_llm's cost alone and display the component as comfortably POSITIVE on a
	// preset like codesmart — inverting its own arithmetic in the one field an operator
	// reads. The rate-based signature makes that mistake unrepresentable.
	perSavedTok := agentCacheReadPerMTok / 1e6
	if h.opts.CacheMode == "off" {
		perSavedTok = agentFreshPerMTok / 1e6
	}
	xs := metrics.ExtractSnapshot(cost, perSavedTok, cacheWrite, cacheRead)
	snap.Extract = &xs
	json.NewEncoder(w).Encode(snap)
}

// Agent-model token rates used to VALUE saved tokens at /stats (claude-sonnet-5 class,
// $3/MTok fresh, 0.1x cache read). Mirrors components/offload/extract_econ.go, which
// applies the same rates inside the gate; kept as local constants rather than a shared
// export because the two layers may legitimately be priced differently (the gate prices
// the traffic it sees; /stats prices the aggregate).
const (
	agentFreshPerMTok     = 3.00
	agentCacheReadPerMTok = 0.30
)

// expand resolves a stashed original by id — the HTTP side of reversibility (the
// model-callable tool loop is a separate concern, added with response handling).
func (h *Handler) expand(w http.ResponseWriter, r *http.Request) {
	// Hosted mode: the tenancy decides WHICH store is searched, which is the outer
	// half of the ownership check below. Session scoping alone was enough when one
	// store served one deployment; with a store per tenant, resolving against the
	// caller's own store means a guessed id cannot even name a stash belonging to
	// someone else.
	tn, err := h.tenancyFor(r)
	if err != nil {
		failAuth(w, err)
		return
	}
	id := r.URL.Query().Get("id")
	// Scope retrieval to the caller's session: a stash is keyed by a global content
	// hash, so without this check any client reaching the proxy could fetch another
	// session's offloaded original by supplying its id (cross-session/tenant disclosure).
	// The session comes from the same x-context-guru-session header used on chat requests
	// (or ?session=). The model-driven expand loop needs no such check — it only ever sees
	// markers minted from its own request.
	sess := r.Header.Get("x-context-guru-session")
	if sess == "" {
		sess = r.URL.Query().Get("session")
	}
	// Through session.Scoped, exactly like the chat path: owner keys are recorded
	// under the TENANT-SCOPED session, so comparing the raw header against them
	// missed every time and this endpoint 404'd for every hosted tenant. Scoped()
	// also sanitises the header (see its comment), and an empty id stays empty here
	// so OwnsKey keeps rejecting it rather than matching the empty-content hash.
	if sess != "" {
		sess = session.Scoped(tn.ID, sess, "", "")
	}
	if !offload.OwnsKey(tn.Store, sess, id) {
		http.Error(w, "expired, unknown, or not owned by this session", http.StatusNotFound)
		return
	}
	orig, ok := expand.Resolve(tn.Store, id)
	if !ok {
		http.Error(w, "expired or unknown id", http.StatusNotFound)
		return
	}
	w.Write([]byte(orig))
}

// copyHeaders copies headers except hop-by-hop ones; the caller's provider auth
// (Authorization / x-api-key) passes straight through to the upstream.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length", "Host":
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "x-context-guru-") {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}
