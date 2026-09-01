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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
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
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

// Options configures upstreams and credential injection. Each upstream is a base
// URL the matching route forwards to (the incoming path is appended).
//
// Credentials: by DEFAULT the caller's own auth header is forwarded unchanged, so
// each user's traffic is billed to their own provider account. Setting a key here
// replaces it — the eval-containers gateway model, where the agent holds only a
// placeholder and the real provider key lives in the gateway env. That is an explicit
// single-tenant/local fallback, never the hosted default.
type Options struct {
	OpenAIUpstream    string // e.g. https://api.openai.com
	AnthropicUpstream string
	// BobUpstream, when set, enables the Bob (BobShell) gateway: Bob's
	// OpenAI-dialect model calls (POST /inference/v1/chat/completions) are reduced
	// and forwarded here, and every other path Bob calls (control-plane:
	// /admin/v1/profile, /inference/v1/model/info, …) is proxied through verbatim
	// so the CLI boots and authenticates. Point Bob's CUSTOM_BASE_URL at this proxy.
	BobUpstream  string // e.g. https://api.us-east.bob.ibm.com
	OpenAIKey    string // when set, REPLACES the caller's Authorization: Bearer
	AnthropicKey string // when set, REPLACES the caller's x-api-key
	// ForceModel, when set, overwrites the request's "model" field. eval-containers
	// uses this to pin every call to EVAL_MODEL regardless of what the agent asked for.
	ForceModel string
	Client     *http.Client
	// UpstreamHeaderTimeout bounds how long an upstream may take to send its first byte
	// (0 = defaultUpstreamHeaderTimeout). Deliberately NOT a whole-request timeout — see
	// upstreamTransport for the 502s that cost.
	UpstreamHeaderTimeout time.Duration
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
	// InjectAlways | InjectNever). Empty defaults to auto.
	//
	// `auto` injects when the request already declares tools, the store persists, and THE
	// PIPELINE CONTAINS AT LEAST ONE OFFLOAD. It does NOT depend on the request carrying a
	// marker — that would make the tools array vary turn to turn, and every variation is a
	// whole-prefix cache miss (see the note on expand.InjectAuto).
	//
	// This comment used to claim a marker condition that expand.Inject explicitly disclaims,
	// and the pipeline condition was missing entirely. The consequence was live: a
	// cachesplit-only pipeline forwarded `context_guru_expand` to the provider, a model that
	// saw marker-shaped text in a file called it, and the call could only ever fail because
	// nothing in that pipeline mints a marker to resolve.
	//
	// `always` still injects unconditionally — an operator asking for it by name gets it.
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
	// Cache is the single-tenant host's prompt-cache policy (the idle keep-alive and the
	// mixed-TTL head). In hosted mode each tenant's own `cache:` block is used instead.
	Cache CachePolicy
	// Upstreams is the operator's allow-list, by name, consulted only in hosted
	// mode. A tenant selects a NAME; it can never supply a URL.
	Upstreams map[string]Upstream
	// Limits bounds what one tenant can consume of the shared box: request rate,
	// in-flight concurrency, and (process-wide) concurrent compaction-model calls.
	// Zero values disable each bound. Ignored in single-tenant mode, where there is
	// nobody to protect anyone from.
	Limits Limits
	// Spend reports a tenant's month-to-date cost, for DISPLAY on the settings page.
	// nil = no figure shown. There is no cap: every tenant spends their own provider
	// credential, so there is no shared budget to guard.
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
	// sent holds the last body forwarded upstream per session, so a component can put a question to
	// the request's own model with the bytes the provider's cache was populated from. See
	// prefixask.go for the bounds and for what happens when one is hit.
	sent *sentStash
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
	// authLim bounds FAILED proxy authentications per client address — the control on the
	// agent-key oracle, see authenticate. Anonymous like regLim: its keys are client
	// addresses, so its refusals are counted process-wide only.
	authLim *Limiter
	// pwLim and codeLim bound sign-in attempts: password checks and emailed-code
	// submissions. Anonymous like regLim (their keys are an email or a client address,
	// never a tenant id) and separate from each other because their budgets differ —
	// see passwordAttemptsPerMinute and codeAttemptsPerMinute. Without these the
	// 6-digit code is not a second factor and the argon2 verify is an amplifier.
	pwLim   *Limiter
	codeLim *Limiter
	// keeper runs the idle prompt-cache keep-alive: the one cache mechanism that has to act
	// BETWEEN requests, when no request is in flight and no component can run. Always
	// present; it does nothing at all until a tenant opts in. See keepalive.go.
	keeper *keeper
	// promCache memoises the Prometheus body for a scrape interval; the per-tenant
	// series cost a SQL query and Grafana scrapes every few seconds.
	promCache promCache
	// metricsInflight collapses every concurrent cache-miss into the one render already
	// running — see metricsHandler.
	metricsInflight singleflight.Group
}

// upstreamTransport is the default upstream client's transport, and the reason there is no
// http.Client.Timeout on it.
//
// http.Client.Timeout covers reading the response BODY, so on a streaming proxy it is not a
// stall detector, it is a hard ceiling on how long a generation may take. It was 5 minutes,
// and Claude Code streams with thinking enabled: a long turn on a large session hit exactly
// 297,9xx ms of upstream time and came back 502, which the agent shows as an API error after
// four or five minutes of apparently healthy work. Eleven such failures on one account, all
// of them streaming, all of them large — while 160 shorter streamed turns from the same
// account through the same upstream succeeded. Nothing was wrong upstream and nothing was
// wrong with the pipeline (our own time on those requests was 25-84 ms); we cut the
// connection ourselves.
//
// ResponseHeaderTimeout is the right shape instead: it bounds waiting for the FIRST byte, so
// a dead upstream is still caught, and a stream that is producing tokens is never
// interrupted for having produced them for too long. It also still bounds a non-streaming
// request end to end, because such a response's headers do not arrive until it is generated
// — which is why the default is generous rather than tight.
func upstreamTransport(headerTimeout time.Duration) http.RoundTripper {
	if headerTimeout <= 0 {
		headerTimeout = defaultUpstreamHeaderTimeout
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = headerTimeout
	return t
}

// defaultUpstreamHeaderTimeout is how long an upstream may take to say ANYTHING before we
// treat it as gone. Generous on purpose: a non-streaming request generates its whole answer
// before sending headers, so this is that request's whole budget.
const defaultUpstreamHeaderTimeout = 10 * time.Minute

// New builds the proxy handler. agg may be nil (no /stats rollups).
func New(pipe *components.Pipeline, st store.Store, agg *metrics.Aggregator, opts Options) *Handler {
	c := opts.Client
	if c == nil {
		c = &http.Client{Transport: upstreamTransport(opts.UpstreamHeaderTimeout)}
	}
	h := &Handler{pipe: pipe, store: st, agg: agg, opts: opts, client: c,
		tracker: modes.NewTracker(0), rec: opts.Dashboard,
		sent: newSentStash()}
	if h.mode() == components.ModeObserve {
		h.pool = modes.NewPool(opts.Observe.MaxQueue, opts.Observe.Workers)
		h.shadow = store.NewMemory(store.Options{})
	}
	// The single-tenant view of this handler's own configuration. In hosted mode it
	// is never consulted (tenancyFor goes to Options.Tenants); it exists so the
	// request path has one shape.
	h.static = &Tenancy{Preset: opts.Preset, Pipe: pipe, Store: st,
		Shadow: h.shadow, Mode: h.mode(), Cache: opts.Cache}
	h.limiter = NewLimiter(opts.Limits)
	h.keeper = newKeeper(h)
	h.keeper.start()
	h.regLim = newAnonLimiter(Limits{RequestsPerMinute: registrationsPerMinute})
	h.authLim = newAnonLimiter(Limits{RequestsPerMinute: authFailuresPerMinute})
	h.pwLim = newAnonLimiter(Limits{RequestsPerMinute: passwordAttemptsPerMinute})
	h.codeLim = newAnonLimiter(Limits{RequestsPerMinute: codeAttemptsPerMinute})
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
func (h *Handler) Close() {
	h.pool.Stop()
	// The keeper holds request bodies and, on a caller-pays upstream, callers' provider
	// credentials. Stopping it drops both, which is why this is not optional on a host that
	// builds and discards handlers.
	h.keeper.Stop()
}

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
	// Token counting, forwarded verbatim with no pipeline. Absent this route, a client that
	// asks how big its context is gets a 404 and falls back to working it out with inference
	// requests — billed calls, added by a proxy sold on reducing them. See counttokens.go.
	m.HandleFunc("POST /anthropic"+countTokensPath, h.countTokens(h.anthropicCountTokensUpstream()))
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
	// includes per-tenant cost. Wrapped in a timeout matching Prometheus's own
	// scrape_timeout: the dashboard DB's connection pool is bounded (dash/store.go), so a
	// burst past it now queues instead of growing unbounded — better than the OOM crash
	// that replaced, but a queued scrape must still get a fast, clear answer rather than
	// hang the connection indefinitely.
	m.HandleFunc("GET /metrics", http.TimeoutHandler(http.HandlerFunc(h.metricsHandler), 10*time.Second,
		"metrics query timed out").ServeHTTP)
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
			tn, err := h.authenticate(r)
			if err != nil {
				h.refuse(w, r, err)
				return
			}
			// Metered like every other authenticated route: this one forwards arbitrary
			// methods and paths, so unmetered it was the cheapest way to spend the box.
			release, ok := h.meter(w, r, tn)
			defer release()
			if !ok {
				return
			}
			if up, err = h.upstreamFor(r, tn, pickBob, upstream{}); err != nil {
				h.refuseRoute(w, r, tn, err)
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
		setUpstreamAuth(req.Header, up)
		if isBobProfile(r) {
			// We have to read this one response, so do not let it arrive compressed.
			req.Header.Del("Accept-Encoding")
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
		if isBobProfile(r) && h.forwardBobProfile(w, resp) {
			return
		}
		h.stream(w, resp)
	}
}

// bobProfilePath is the profile route Bob's CLI calls before anything else, and the
// one response the catch-all does not forward verbatim. See forwardBobProfile.
const bobProfilePath = "/admin/v1/profile"

func isBobProfile(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.URL.Path, bobProfilePath)
}

// forwardBobProfile forwards Bob's profile with the per-instance REGION fields removed,
// and reports whether it answered the request.
//
// Bob's client does not keep the base URL it was configured with. Having fetched the
// profile, `Pc.resolveBaseUrl` (bobshell 1.0.6) replaces the HOSTNAME of that URL with
// `api.<region_domain>` from the profile — keeping the scheme and, fatally, the PORT.
// Pointed at a context-guru instance on 127.0.0.1:4111, its very next call goes to
// http://api.us-east.bob.ibm.com:4111, where nothing listens: observed live as
// "Request failed after 6 attempts: fetch failed", with not one model request ever
// reaching the proxy while the profile call itself succeeded — the confusing shape,
// because the proxy looks half-working and its log is empty.
//
// The rewrite is skipped when the instance carries no region (`hasRegion` false), so
// dropping those two fields is all it takes for the client to keep talking to the
// endpoint its operator configured. Nothing else in the document is touched, and a
// body we cannot parse is passed through unchanged rather than mangled.
func (h *Handler) forwardBobProfile(w http.ResponseWriter, resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBytes))
	if err != nil {
		return false
	}
	resp.Body.Close()
	out, ok := stripProfileRegion(body)
	if !ok {
		out = body
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
	return true
}

// stripProfileRegion removes region/region_domain from every instance in a Bob profile
// document, reporting false when the body is not one (then it must be forwarded as-is).
func stripProfileRegion(b []byte) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	insts, ok := doc["instances"].([]any)
	if !ok {
		return nil, false
	}
	for _, it := range insts {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		delete(m, "region")
		delete(m, "region_domain")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
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
	// Hosted mode authenticates AND METERS this endpoint too. It runs the pipeline over a
	// caller-supplied transcript and can invoke the cheap model, so leaving it open would
	// be both an unmetered compute endpoint and a way to write into whichever state store
	// it happened to reach — and authenticating without metering left the second half of
	// that: the most expensive route on the box was the only one with no rate limit.
	tn, err := h.authenticate(r)
	if err != nil {
		h.refuse(w, r, err)
		return
	}
	release, ok := h.meter(w, r, tn)
	defer release()
	if !ok {
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
	// effPreset is what the captured row must be LABELLED with: the pipeline that actually
	// ran on this request. Empty means no override took effect, so the tenant default
	// newCapture already recorded stands. See capture.notePreset.
	effPreset := ""
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
				// Same precedence PipelineFor itself applies: an explicit component list wins
				// over ?preset=. Such a list has no preset name, so it is labelled "custom",
				// exactly as a configuration document with a bare `pipeline:` is (see
				// buildTenantConfig) — never the preset name that did NOT run.
				effPreset = preset
				if len(names) != 0 {
					effPreset = "custom"
				}
			} // build error => fall back to the configured pipeline (fail open)
		}
	}

	// No upstream here, so there is no "incoming" model; only the static
	// "config"-source client (and any endpoint pinned in a component's model: block).
	models := components.ModelSpec{Static: h.staticModel()}
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
	cp.notePreset(effPreset)
	cp.noteModel(gjson.GetBytes(body, "model").String())
	start := time.Now()
	// cp.llmCtx: our own compaction-model spend under this context is charged to THIS
	// request's row, not to whichever tenant is in flight when the call returns.
	res := apply.BodyOpts(cp.llmCtx(r.Context()), pipe, tn.Store, apply.Opts{
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

// selfRates resolves the per-token rates of the model THIS request targets, for a component
// that will call the same model itself (model.source: incoming, the shipped default). Without
// it such a component prices its own spend at a built-in constant — measured on a real
// session, that understated one call by about 3x, and the economic gate spends on that number.
//
// Nil pricer or an unnameable model returns the zero value, which the component must read as
// "unknown" and fall back on, never as free.
func (h *Handler) selfRates(ctx context.Context, model string) components.TokenRates {
	if h.opts.Prices == nil || model == "" {
		return components.TokenRates{}
	}
	p, ok := h.opts.Prices.Price(ctx, model)
	if !ok || p.Zero() {
		return components.TokenRates{}
	}
	return components.TokenRates{
		Input: p.Input, Output: p.Output, CacheRead: p.CacheRead, CacheWrite: p.CacheWrite,
	}
}

// ratesFor hands the pipeline the operator's rate card as a lookup, so a component that
// compacts with a model OTHER than the request's own can price its own spend from the same
// card the invoice is computed from.
//
// selfRates answers "what does the request's model cost"; this answers "what does THIS model
// cost", which is the question a component naming a cheap model has. Without it the component
// falls back to CHEAP_MODEL_PRICE_* (haiku LIST rates), which is a different card from the one
// the dashboard prices with — measured 32% apart on the same 93 calls.
func (h *Handler) ratesFor(ctx context.Context) func(string) components.TokenRates {
	if h.opts.Prices == nil {
		return nil
	}
	return func(model string) components.TokenRates { return h.selfRates(ctx, model) }
}

// incomingModel builds an LLM client that reuses the proxied request's own model
// and the route's upstream, so a NeedsModel component can call the same backend the
// request targets.
//
// It is the SECOND way a credential leaves this box, and it must agree with the first.
// setUpstreamAuth resolved that once, for this route, and the answer is `up.setKey`:
//
//   - setKey == nil — the caller's own provider credential is what goes upstream, so the
//     component spends the caller's key too. That is the hosted default and the point of
//     it: a component that calls an LLM spends money, and spending a server-held key here
//     would bill one account for every user's compaction.
//   - setKey != nil — gateway mode. setUpstreamAuth DELETES the caller's auth slots and
//     injects the server's key, because the caller holds only a placeholder. So the
//     caller's value must not travel via this client either, to the very same up.base.
//     The injector holds its key in a closure that cannot be read back, so the only
//     server key reachable here is the statically configured one — which is exactly the
//     single-tenant gateway this branch is for. Hosted mode degrades instead, for the
//     same reason staticModel is withheld there.
//
// Reading up.setKey rather than re-deriving the credential is the fix and the invariant:
// there is one decision, and both exits read it.
//
// FAIL OPEN: no credential, no upstream or no model name returns nil, and the
// component degrades to its deterministic path. It never errors the request.
func (h *Handler) incomingModel(provider bschemas.ModelProvider, up upstream, body []byte, r *http.Request) components.Model {
	if up.base == "" {
		return nil
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		return nil
	}
	key := CallerKey(r)
	// The scheme belongs to the CALLER's credential, so it is captured before any fallback
	// to a server-held key below (which is presented the operator's way, not the caller's).
	scheme := CallerAuthScheme(r)
	if up.setKey != nil {
		key = ""
	}
	if key == "" {
		scheme = ""
	}
	switch provider {
	case bschemas.Anthropic:
		if key == "" {
			key = h.serverKey(h.opts.AnthropicKey)
		}
		if key == "" {
			return nil
		}
		return cheapmodel.Anthropic{BaseURL: up.base, Model: model, APIKey: key, AuthScheme: scheme, Client: h.client}
	case bschemas.OpenAI:
		if key == "" {
			key = h.serverKey(h.opts.OpenAIKey)
		}
		if key == "" {
			return nil
		}
		return cheapmodel.OpenAI{BaseURL: up.base, Model: model, APIKey: key, Client: h.client}
	}
	return nil
}

// serverKey returns a statically configured gateway credential, and nothing at all in
// hosted mode — where a server-held key must not fund a tenant's compaction. Same rule
// as staticModel, one line, so the two cannot drift.
func (h *Handler) serverKey(key string) string {
	if h.opts.Tenants != nil {
		return ""
	}
	return key
}

// staticModel is the "config"-source cheap-model client — nil in hosted mode. It
// authenticates with a SERVER credential (CHEAP_MODEL_KEY), so offering it to a
// tenant's pipeline would bill that server account for their compaction. Components
// that find no model degrade (fail open); they never error.
func (h *Handler) staticModel() components.Model {
	if h.opts.Tenants != nil {
		return nil
	}
	return h.opts.CheapModel
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

// authFailuresPerMinute bounds how fast ONE client address may fail proxy authentication.
//
// 10, in the same spirit as passwordAttemptsPerMinute: enough that a human whose token has
// gone stale, or an agent retrying a bad configuration, sees an honest 401 rather than a
// throttle, and few enough that guessing is not a strategy. A rolling window rather than a
// lockout, deliberately — see passwordAttemptsPerMinute for why a sticky lockout is a
// denial-of-service anyone can aim at a colleague.
const authFailuresPerMinute = 10

// authenticate resolves the caller's tenancy, and THROTTLES failures per client address.
//
// The throttle is the point. An agent that can set no header of ours is identified by the
// sha256 of the provider key it already sends (Resolve → ResolveAgentKey), so the
// difference between errUnboundKey and a request that proceeds answers "is this string a
// bound agent key" — and a hit is not merely confirmation, it IS impersonation: the caller
// becomes that tenant, spends against that account's routes and, with content capture on,
// reads that account's transcripts. Unmetered, that oracle is grindable at whatever rate
// the front end allows (30 r/s per address here). The key-length floor in tenant/ removes
// the most guessable strings; only a limit removes the oracle.
//
// Charged on FAILURE only, so a working credential is never billed for someone else's
// guessing — which is also what keeps a busy legitimate agent out of the bucket entirely.
//
// Keyed by regBucket(registrantIP(r)): the same bucket registration and sign-in attempts
// use, inheriting both its IPv6 /64 granularity (per-address is meaningless against a /64)
// and its rule that X-Forwarded-For is trusted from a loopback peer only, last element
// first — which is the one nginx wrote.
//
// Counted exactly once. The anon limiter records its own refusal with NO key, because a
// client address is attacker-supplied and must never become a metric label, and failAuth
// counts no 429s. The message is ours rather than the limiter's, whose "for this account"
// would be both wrong for an address bucket and a hint that a credential resolved.
func (h *Handler) authenticate(r *http.Request) (*Tenancy, error) {
	tn, err := h.tenancyFor(r)
	if err == nil {
		return tn, nil
	}
	if _, lErr := h.authLim.Acquire(regBucket(registrantIP(r))); lErr != nil {
		return nil, statusError{http.StatusTooManyRequests,
			"too many failed authentications from this address; wait a minute and try again"}
	}
	return nil, err
}

// meter takes this tenant's slot against Options.Limits, refusing the request if it is
// over one. Called by EVERY authenticated entry point, right after authenticate:
//
//	release, ok := h.meter(w, r, tn)
//	defer release()
//	if !ok {
//		return
//	}
//
// It exists because TENANT_RPM and TENANT_CONCURRENT used to be enforced in chat() and
// nowhere else, while two other authenticated routes ran the same box for free —
// /compact drives the whole pipeline (tokenisation, tree-sitter, several passes) over a
// body up to 32 MiB, and the Bob catch-all forwards arbitrary methods and paths. With the
// limiter at one request a minute, chat refused the second call while /compact served
// twenty of twenty. One guard, three call sites, smaller than three copies of it.
//
// release is never nil, so `defer release()` is correct on the refusal path too. The 429
// accounting is unchanged and NOT doubled: the limiter counts its own refusals (limits.go,
// the only place that knows WHICH limit was hit) and failAuth deliberately counts no 429s.
func (h *Handler) meter(w http.ResponseWriter, r *http.Request, tn *Tenancy) (release func(), ok bool) {
	release, err := h.limiter.Acquire(tn.ID)
	if err != nil {
		h.refuse(w, r, err)
		return release, false
	}
	return release, true
}

// refuseRoute refuses a route-resolution failure for a caller who IS authenticated.
//
// It exists for one refusal in particular: errNoProviderKey — "your account is fine, but
// you sent no provider credential of your own". failAuth maps every 401 to reason=auth,
// which is right for an unknown token and useless for this one, and this one is about to
// become the DOMINANT refusal: the moment a deployment stops injecting a server-held
// upstream key, every user who has not yet added their own hits it. Collapsed into `auth`,
// the blast radius of that change is invisible in the metrics exactly when it has to be
// measured and the affected users told.
//
// The tenant is known here — it authenticated, only its credential is missing — so the
// count carries the tenant label failAuth cannot supply, which is what turns "N users
// broke" into "these users broke".
//
// Everything else goes to h.refuse unchanged.
func (h *Handler) refuseRoute(w http.ResponseWriter, r *http.Request, tn *Tenancy, err error) {
	if !errors.Is(err, errNoProviderKey) {
		h.refuse(w, r, err)
		return
	}
	// The same WARN refuse writes, for the same reason: this is a turn-away a user will
	// report, and it has to be findable by tenant (the logger in the context carries it).
	code, msg := statusOf(err)
	logging.From(r.Context()).Warn("cg.refused", "status", code, "reason", msg,
		"path", r.URL.Path, "client", clientIP(r))
	failAuthAs(w, err, refuseNoProviderKey, tn.ID)
}

// failAuthAs writes a refusal exactly as failAuth does, but counts it under the reason
// GIVEN rather than the one inferred from the status code.
//
// ONE refusal, ONE count — which is why this exists rather than simply counting the new
// reason alongside failAuth's. The SLO dashboard divides sum(cg_refused_requests_total),
// UNLABELLED, by refusals + requests (deploy/grafana/dashboards/context-guru-slo.json), so
// a request counted under two reasons inflates the HTTP error-rate SLI by one — on exactly
// the metric a credential migration is judged by, in exactly the period it matters.
//
// The response must stay byte-identical to failAuth's; the reason it is duplicated here at
// all is that failAuth lives in tenancy.go, which another agent owns this cycle. Once that
// settles, this collapses into failAuth's switch (one `errors.Is` case) and disappears.
// TestFailAuthAsMatchesFailAuth fails if the two drift in the meantime.
func failAuthAs(w http.ResponseWriter, err error, reason refusalReason, tenantID string) {
	code, msg := statusOf(err)
	recordRefusal(reason, tenantID)
	w.Header().Set("Content-Type", "application/json")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="context-guru"`)
	}
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}

func (h *Handler) chat(provider bschemas.ModelProvider, static upstream, pick func(*Tenancy) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authenticate FIRST, before reading a body or doing any work. In hosted mode
		// an unauthenticated caller must not be able to make the proxy buffer 32 MiB.
		tn, err := h.authenticate(r)
		if err != nil {
			h.refuse(w, r, err)
			return
		}
		// The per-request logger. Every line this request produces — here, inside the
		// pipeline, inside a component — carries the same tenant, so one LogQL selector
		// is the whole investigation. The tenant ID only: the token it was resolved from
		// never appears in a log line, and the scrubber would replace it if it did.
		//
		// It goes in the CONTEXT because apply and the components already receive one and
		// nothing else would have to change; the session is added below, once apply has
		// resolved it (it is derived from the body, so it is not knowable yet).
		//
		// Attached HERE, before anything below can refuse the request, because refuse()
		// reads its logger out of the context: with this block below the two refusals that
		// follow, every rate-limit and every "no provider credential of your own" came out
		// as a cg.refused line with NO tenant on it, while the tenant sat in hand three
		// lines up. Those are the two refusals a user actually reports, and refuse's own
		// contract is that they are findable by tenant.
		//
		// route is static.path rather than up.path so it is known before the upstream is
		// resolved. They are the same string by construction: upstreamFor carries
		// static.path over verbatim (it substitutes the BASE, never the route).
		lg := slog.Default().With("tenant", tenantLabel(tn.ID), "route", static.path,
			"provider", string(provider))
		r = r.WithContext(logging.With(r.Context(), lg))
		// Limits, before the body is read. Refusing a request that would exceed a bound
		// must not first cost us 32 MiB of buffering.
		release, ok := h.meter(w, r, tn)
		defer release()
		if !ok {
			return
		}
		up, err := h.upstreamFor(r, tn, pick, static)
		if err != nil {
			h.refuseRoute(w, r, tn, err)
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
			Static:   h.staticModel(),
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
			// The gate name here is the SAME string /stats reports as
			// components.bypass.gates.agent_compaction, so the log line and the metric
			// cannot disagree about what happened.
			lg.Debug("cg.bypass", "gate", "agent_compaction")
		}
		// Start the dashboard capture (nil when the dashboard is off). It only holds
		// values the request path already computed; nothing here does I/O.
		cp := h.newCapture(r, string(provider), up.path, tn)
		cp.noteModel(gjson.GetBytes(body, "model").String())
		// The request's own metadata (effort, thinking, sampling, tool_choice, shape),
		// read from the PRISTINE body in one pass before the pipeline touches it.
		cp.noteMeta(metaFromBody(body))
		// Which tools, MCP servers and skills the request DECLARED, and which its last turn
		// called — off the same pristine body, memoized by declaration-set digest.
		cp.noteInventory(string(provider), body)
		// Fail open around the whole pre-forward rewrite (pipeline + expand injection): a
		// panic anywhere here must forward the PRISTINE inbound body, never 500 the client.
		// apply.BodyFull has its own recover; this backstops expand.Inject and anything else.
		orig := body
		var tr apply.Trace // hoisted: the lifecycle log line below reads it
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					lg.Error("context-guru: recovered from panic before forward; sending original", "panic", rec)
					body = orig
				}
			}()
			// A client that received our own expand tool_use answered it itself, with an
			// error — no client implements the tool. Put the content the model asked for where
			// that error is, BEFORE the pipeline reads the transcript, so the restored text is
			// marked kept-verbatim and offload does not compact it straight back into the
			// marker that caused the call. Same gate as expand.Inject below: we repair exactly
			// when we are rewriting this request at all.
			if tn.Mode != components.ModeObserve && !bypassed {
				var repaired int
				body, repaired = h.repairExpandErrors(provider, body, tn, cp)
				if repaired > 0 {
					// A rewrite of the client's own transcript must never be silent: this is
					// the only line that says the response side failed to withhold the call
					// and the request side caught it.
					lg.Debug("cg.expand_repair", "restored", repaired)
				}
			}
			var added time.Duration
			body, added, tr = h.applyMode(&reqInfo{
				// cp.llmCtx: context-guru's OWN compaction-model spend under this context
				// is charged to this request's row, and to no other tenant's.
				ctx:      cp.llmCtx(r.Context()),
				provider: provider,
				body:     body,
				session:  r.Header.Get("x-context-guru-session"),
				bypassed: bypassed,
				models:   models,
				window:   window,
				rates:    h.selfRates(r.Context(), gjson.GetBytes(body, "model").String()),
				tn:       tn,
			})
			addedMs := float64(added.Microseconds()) / 1000.0
			cp.noteCG(addedMs)
			cp.noteTrace(tr)
			// A real request has arrived on this session: cancel any pending keep-alive
			// (this request refreshes the entry itself, and a ping concurrent with it would
			// be a second request against the tenant's budget for nothing), and pick up what
			// the pings during the idle span it just ended actually did. Both facts are
			// needed BEFORE the row is priced — the ping count and the tokens the last ping
			// refreshed are inputs to a dollar figure, not just a label.
			kaPings, kaRefreshed, kaStrategy := h.keeper.arrive(tn.ID, tr.Session)
			cp.noteKeepAlive(kaPings, kaRefreshed, kaStrategy)
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
			// Skipped on a bypassed request too, and this one DOES cost cache sometimes. The
			// earlier claim here — that a bypassed request is a compaction whose prefix is its
			// own, so skipping is free — is true for a real compaction and false for a false
			// positive, and isAgentCompaction has a reachable one: it fires on any last message
			// with role "user" containing a compaction phrase (agentcompaction.go:71-77), and in
			// the Anthropic dialect a tool_result IS a user-role message. So a tool output that
			// quotes one of those phrases — a session reading the docs page, or reading this
			// file — makes an ordinary mid-conversation turn bypass, lose the expand tool, and
			// flip the tools array against the very prefix it shares.
			//
			// Measured on the dashboard: bypassed rows carry prefix_change at 26.7% against
			// 1.44% for non-bypassed, an 18.6x enrichment, with zero cold_start and 22 of 30
			// mid-session. n=30, so treat the RATE as indicative and the mechanism as
			// established.
			//
			// Not fixed here, deliberately: the fix is to stop the detector matching a
			// tool_result (require the phrase in a text block), which changes what bypass means
			// for every caller of it, not just for this injection. That belongs in its own
			// change with its own measurement — the check it needs is a phrase count in a
			// capture against bypassed=1 rows. Injecting into a bypassed request instead is not
			// the answer: bypass promises a byte-identical forward, and breaking that to save a
			// prefix trades a documented guarantee for an unmeasured gain.
			if tn.Mode != components.ModeObserve && !bypassed {
				im := h.opts.InjectExpand
				if im == "" {
					im = expand.InjectAuto
				}
				// Under `auto`, advertise only when THIS request's pipeline can actually mint a
				// marker. Without it, an offloader-free pipeline — `off`, `safe`, or any
				// cachesplit-only configuration — declared a tool whose every use is guaranteed
				// to fail: measured `[Read Bash]` in, `[Read Bash context_guru_expand]` out, and
				// on a transcript containing marker-shaped text the model duly called it and got
				// "[expand: original for id ... is no longer available]".
				//
				// Which presets those are is NOT a list worth writing down here — `mcp` looks
				// offloader-free and is not, because `smartcrush` implements components.Offload.
				// That is exactly why the gate asks the interface (Pipeline.HasOffload) instead
				// of naming presets: a list in a comment is a second source of truth, and this
				// one was wrong about `mcp` in its first draft.
				//
				// `off` mattered most: it is the A/B control arm, and a control that carries an
				// extra tool declaration is not a control.
				if im != expand.InjectAuto || tn.Pipe.HasOffload() {
					body, _ = expand.Inject(string(provider), im, body, tn.Store.Persists())
				}
			}
		}()
		// Load the request's one INFO line with everything the pipeline decided. serve
		// emits it in a defer once the response is finished, so a request produces exactly
		// one lifecycle line whichever way it ends — and the attrs are built here, once,
		// rather than on the response path.
		h.serve(w, r, provider, up, body, bypassed, cp, tn, tr.Session, lifecycleLogger(lg, tr, bypassed))
	}
}

// tenantLabel is the tenant id as a log label. Single-tenant deployments have no
// tenant id at all, and an empty label is DROPPED by promtail — which would leave the
// logs dashboard's tenant selector empty for every local proxy, i.e. for most users.
// "local" says the true thing (there is one tenant, it is this deployment) and makes
// the same dashboard work in both modes.
func tenantLabel(id string) string {
	if id == "" {
		return "local"
	}
	return id
}

// lifecycleLogger returns the request logger with this request's pipeline outcome
// attached: the resolved session (so every line of this request correlates, including
// the ones apply wrote), and the token accounting.
func lifecycleLogger(lg *slog.Logger, tr apply.Trace, bypassed bool) *slog.Logger {
	lg = lg.With("session", tr.Session, "messages", tr.Messages, "bypassed", bypassed,
		"cache_aware", tr.CacheAware, "frozen_tokens", tr.FrozenTokens)
	if tr.Run != nil {
		lg = lg.With("tokens_before", tr.Run.TokensBefore, "tokens_after", tr.Run.TokensAfter,
			"saved", tr.Run.Saved(), "cg_ms", tr.Run.DurationMs)
	}
	return lg
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

// repairExpandErrors resolves every expand tool_result the CLIENT had to answer itself, so
// `No such tool available: context_guru_expand` never reaches the model. It is the request
// half of interception: h.serve withholds the call from the client wherever it can, and this
// covers what it structurally cannot — a batched client tool, the round cap, an event stream
// that will not reconstruct, a non-Anthropic stream, a bypassed turn carrying older markers.
// See expand.RepairToolResults.
//
// Restored content is marked kept-verbatim: an offloader that re-compacted it would trigger
// the same expand call again next turn. That mark is also what keeps the accounting honest —
// the tokens are charged to the dashboard row once, on the turn the original first comes back,
// because the client keeps its own copy of the error and the repair therefore runs again on
// every later turn. Nothing is charged to the process-wide bounce counter, which counts expand
// CALLS, and ten repairs of one stale error are one call.
func (h *Handler) repairExpandErrors(provider bschemas.ModelProvider, body []byte, tn *Tenancy, cp *capture) ([]byte, int) {
	out, restored := expand.RepairToolResults(string(provider), body, func(hashID string) (string, bool) {
		return expand.Resolve(tn.Store, hashID)
	})
	for _, orig := range restored {
		// Charge the row only the FIRST time this original comes back. Repairing the same
		// stale error on ten later turns is one recovery, not ten.
		if !offload.KeptVerbatim(tn.Store, orig) {
			cp.noteExpand(schema.TextTokens(orig))
		}
		offload.MarkKeptVerbatim(tn.Store, orig)
	}
	return out, len(restored)
}

var errNoUpstream = errors.New("no upstream configured")

// serve forwards the request and runs the expand continuation loop: if the model
// calls the expand tool (and ONLY that tool), resolve the originals from the store,
// append the tool-result turn, and re-invoke upstream — up to a few rounds.
//
// Streaming (SSE) keeps streaming while it does. An Anthropic event stream is forwarded
// event by event and forwarding stops at the content_block_start that calls the expand
// tool; the continuation's blocks are then spliced into the same open response, renumbered
// past the blocks already sent (sseSplicer, ssepeek.go). Nothing is buffered to decide
// this, and the client's first byte is the upstream's first event. Both outcomes are
// counted (agg.RecordSSE → /stats sse_streamed / sse_buffered), and a response is filed as
// buffered only when the model opened with the call, so the client really did wait for the
// whole continuation.
//
// Whatever cannot be intercepted — the model batching a client tool with ours, the round
// cap, a stream that will not reconstruct, another dialect, a bypassed turn carrying older
// markers — reaches the client as the model wrote it, is counted (sse_expand_after_stream),
// and is answered on the NEXT request instead (expand.RepairToolResults).
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, provider bschemas.ModelProvider, up upstream, body []byte, bypassed bool, cp *capture, tn *Tenancy, session string, lg *slog.Logger) {
	// ONE condition governs both halves of the loop: the tool is intercepted exactly when
	// it is advertised on the outgoing request. Those used to be different conditions —
	// advertised when the request had tools, intercepted (for SSE) when it had markers —
	// so a marker-free tools-bearing request declared a tool whose use then streamed
	// straight to a client that has no such tool. Reading the outgoing body rather than
	// trusting Inject's return value also covers a request that already carried the tool.
	//
	// Injection no longer requires markers (expand.Inject, InjectAuto): the tools array has
	// to be byte-stable across a session or every change to it costs the whole cached prefix.
	// So for a tools-bearing client this is true from the FIRST turn, and the old "no offload
	// yet → no tool → no buffering" fast path is gone with it. What replaced it is narrower
	// and does the same job: sseSplicer withholds only the block that calls the expand tool
	// and streams everything else as it arrives (see ssepeek.go). Non-Anthropic dialects are
	// not inspected at all.
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
	// status/rounds/expanded exist for the lifecycle log line: the upstream status is
	// the one fact about a request an operator asks for first, and until now it reached
	// the dashboard row and nothing else.
	status, rounds, expanded := 0, 0, 0
	// upStart of the LAST upstream round. The provider's cache lifetime "is measured from the
	// start of the request that writes or reads the cache entry, not from the end of its
	// response", so this — not the moment the response finished — is the instant the idle
	// clock starts from.
	var lastUpStart time.Time
	// sp is nil until an Anthropic event stream needs splicing, and then lives for the rest
	// of the client request: every later round is written into the response it opened.
	var sp *sseSplicer
	// The events the current round withheld, hoisted out of the loop: once a stream is open,
	// EVERY way this request can end has to end it with a complete turn, and these are the
	// bytes that do it. prevWithheld keeps the round before's, because a continuation round
	// that carries no terminator of its own leaves the earlier round's message_stop as the
	// only one the client will ever get.
	var withheld, prevWithheld []byte
	defer func() {
		// Hand the finished request to the keep-alive. Last, so `body` is the bytes that
		// actually went upstream on the final round (an expand round rewrites it) and the
		// prefix a ping would replay is the one the provider just hashed. Costs nothing when
		// no tenant has opted in.
		h.keeper.record(tn, session, lastUpStart, body, up, r, provider, up.path, status, usage, usageOK)
		if sse {
			// The same two facts to both sinks. The aggregator keeps the process-lifetime
			// average; the dashboard row keeps the per-request pair, so "which model, which
			// account, which hour buffers" becomes answerable instead of being one gauge
			// that resets on restart.
			if h.agg != nil {
				h.agg.RecordSSE(msSince(reqStart, sseFirstByte), sseBuffered)
			}
			cp.observeSSE(msSince(reqStart, sseFirstByte), sseBuffered)
		}
		if h.agg != nil && usageOK {
			h.agg.RecordUsage(usage.FreshInput, usage.CacheRead, usage.CacheWrite, usage.Output)
		}
		cp.finish(usage, usageOK, h.captureContentFor(tn), h.contentCap(), h.contentMax())
		// THE one line per request. In a defer so every terminal path emits it exactly
		// once — stream-through, buffered replay, the round-cap exit, an upstream failure
		// (which also logged its own WARN with the reason).
		// serve_ms, not total_ms: reqStart is taken here, AFTER the pipeline ran, so this
		// is the forward-and-respond half. cg_ms above is the other half, and keeping them
		// separate is the point — one is the upstream's latency and one is ours.
		lg.Info("cg.request", "status", status, "serve_ms", msSince(reqStart, time.Time{}),
			"upstream_rounds", rounds, "expands", expanded, "sse", sse, "sse_buffered", sseBuffered,
			"fresh_input", usage.FreshInput, "cache_read", usage.CacheRead,
			"cache_write", usage.CacheWrite, "output", usage.Output, "usage_reported", usageOK)
	}()
	for round := 0; ; round++ {
		rounds = round + 1
		upStart := time.Now()
		lastUpStart = upStart
		resp, err := h.doUpstream(r, up, body)
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			// The bytes the provider's prompt cache is now populated from. Stashed HERE rather than
			// anywhere earlier because only what was ACTUALLY forwarded and accepted is a valid
			// prefix — an aborted or rejected send caches nothing, and appending to bytes the
			// provider never saw would read no cache while looking exactly like a hit that failed.
			// KEYED BY THE SCOPED SESSION ID, which is what serve receives (tr.Session) and what a
			// component reads as Ctx.Session. Keying it by the raw header instead would make every
			// Ask miss while the mechanism looked switched on.
			h.sent.put(session, body)
		}
		if err != nil {
			// LOG it, and record it on the captured row. An upstream failure used to be
			// invisible in both places: the caller got a 502 and the operator got nothing
			// — no log line, and a dashboard row with status 0 that reads as "unknown"
			// rather than "the upstream refused". On a shared service that is the
			// difference between debugging a report and disbelieving it.
			lg.Warn("context-guru: upstream call failed", "upstream", up.base,
				"round", round, "err", err)
			status = http.StatusBadGateway
			cp.noteUpstream(float64(time.Since(upStart).Microseconds())/1000.0,
				http.StatusBadGateway)
			// And count it: this path does not go through failAuth, so without this the
			// only 502s on the dashboard would be our own misconfiguration.
			recordRefusal(refuseUpstream, tn.ID)
			if sp != nil {
				// The client is MID-STREAM. A 502 written into an open event stream is not
				// a status any client can read — it arrives as garbage appended to the
				// model's turn — so end the turn with the events the splice withheld
				// instead. The client sees the model's own call, which is the same fail-open
				// as every other round the loop cannot finish.
				if withheld != nil && h.agg != nil {
					h.agg.RecordSSEExpandAfterStream()
				}
				sp.handBack(withheld)
				sp.terminate(withheld)
				return
			}
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
		status = resp.StatusCode
		cp.noteUpstream(upMs, resp.StatusCode)
		if h.agg != nil {
			h.agg.RecordUpstreamLatency(upMs, bypassed)
		}
		isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
		lg.Debug("cg.upstream", "round", round, "status", resp.StatusCode,
			"upstream_ms", upMs, "sse", isSSE, "expand_advertised", advertised)
		// The expand tool is intercepted exactly when it is advertised. Nothing else can
		// produce a call to it, so this is both the necessary and the sufficient condition.
		if !advertised {
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
		var respBody []byte
		found := false // this round called the expand tool
		switch {
		case isSSE && provider == bschemas.Anthropic:
			// Forward the events as they arrive and stop at the expand call, rather than
			// buffering the response to find out whether it holds one. See ssepeek.go.
			sse = true
			if sp == nil {
				sp = newSSESplicer(w)
			}
			sp.round(resp)
			// The PREVIOUS round's withheld events, because pass is about to overwrite
			// withheld and round 1's message_stop may be the only terminator this client
			// will ever get (see the terminator check below).
			prevWithheld = withheld
			respBody, withheld, found = sp.pass(resp.Body, expand.ToolName)
			resp.Body.Close()
			switch {
			case found && sp.blocks == 0:
				// The model OPENED with the call, so the client holds a message_start and
				// nothing else and waits for the whole continuation. That is a buffered
				// response, and its first byte has to stay unset: filing the message_start's
				// timestamp as the TTFB of a client that waited a round-trip is the
				// mislabelling sse_ttfb_ms_avg_buffered exists to expose.
				sseBuffered, sseFirstByte = true, time.Time{}
			case !sseBuffered:
				sseFirstByte = sp.first
			}
			lg.Debug("cg.sse_splice", "round", round, "expand_call", found,
				"blocks_sent", sp.blocks, "bytes", len(respBody))
		case isSSE:
			// Non-Anthropic dialects are not inspected at all: AggregateSSE only reconstructs
			// the Anthropic event stream, so there is nothing the loop could read even if we
			// held the bytes — and holding them would cost the client its stream for nothing.
			// A raw expand call on this path is repaired on the REQUEST side instead
			// (expand.RepairToolResults).
			sse = true
			first, u, ok := h.stream(w, resp)
			if !sseBuffered {
				sseFirstByte = first
			}
			if ok {
				usage, usageOK = u, true
			} else {
				usage.StopReason = u.StopReason // as on the other stream path
			}
			return
		default:
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			// A JSON round after a stream was opened is an upstream anomaly (the request
			// said stream: true), and there is no way to splice a JSON body into an event
			// stream. withheld deliberately still holds the previous round's events, so
			// bail below ends the client's turn with a complete stream rather than cutting
			// it off mid-message.
		}
		if u, ok := responseUsage(resp.Header.Get("Content-Type"), respBody); ok {
			usage, usageOK = u, true
		} else {
			usage.StopReason = u.StopReason // same reasoning as the stream path above
		}
		if isSSE && withheld == nil {
			// The whole round is already on the wire. Either it never called expand, or it
			// ran past sseRetainMaxBytes and the turn could no longer be rebuilt — and in
			// that second case the client has our tool_use, which is the leak the counter
			// exists for.
			if found && h.agg != nil {
				h.agg.RecordSSEExpandAfterStream()
			}
			sp.terminate(prevWithheld)
			return
		}
		// bail is what the client gets when the loop cannot answer after all. On a spliced
		// stream that is the withheld events ONLY — the client already holds the prefix and
		// this response's headers went out with it — which for a round whose blocks were not
		// renumbered is byte-for-byte the stream as it arrived. It is also the one path that
		// hands a client our own tool_use, so it says so on /stats instead of in a comment.
		bail := func() {
			if sp == nil {
				writeRaw(w, resp, respBody)
				return
			}
			if withheld != nil && h.agg != nil {
				h.agg.RecordSSEExpandAfterStream()
			}
			sp.handBack(withheld)
			// Idempotent via sp.ended: if those events closed the turn this is a no-op, and
			// if the round was truncated it is the only thing that closes it.
			sp.terminate(withheld)
		}
		if round >= maxExpandRounds {
			// The cap is reached, so this call cannot be answered — the client gets it, and
			// bail is what counts that. Withholding first and handing back is the same bytes
			// as streaming the round whole, and it is the only way the leak is visible.
			bail()
			return
		}

		// Reconstruct the message the loop reasons over. For SSE, aggregate the events;
		// if that fails, replay the raw stream unchanged (fail-open).
		msg := respBody
		if isSSE {
			agg, ok := expand.AggregateSSE(string(provider), respBody)
			if !ok {
				bail()
				return
			}
			msg = agg
		}

		calls, otherTools := expand.ResponseCalls(string(provider), msg)
		if len(calls) == 0 || otherTools {
			bail() // normal answer (or other tools) — hand it over unchanged
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
				// Classified as well as counted: the response loop and the request-path repair
				// both reach here, and an id this proxy could have minted with nothing behind it
				// is our own broken reversibility promise rather than the model inventing one.
				// See expand/unresolved.go.
				expand.NoteUnresolved(c.HashID)
				resolved[c.CallID] = expand.Unavailable(c.HashID)
			}
		}
		expanded += got
		// The reversibility loop is the hardest thing here to diagnose after the fact:
		// a model calls expand, an id has aged out of the store, and the symptom is a
		// confused agent rather than an error. So log what it asked for against what came
		// back — `unresolved > 0` is the tell.
		lg.Debug("cg.expand", "round", round, "calls", len(calls),
			"resolved", got, "unresolved", len(calls)-got)
		next, ok := expand.Continuation(string(provider), body, msg, resolved)
		if !ok {
			bail() // malformed shapes — fail open, hand the response over unchanged
			return
		}
		// got == 0 CONTINUES, and that is the change that let the expand tool be advertised
		// on every request in a session (expand.InjectAuto). `resolved` already carries a
		// placeholder for every unresolved id, so the continuation is well formed whether
		// anything came back or not, and the model gets to finish its turn by reading "that
		// id is no longer available" — which is a turn that completes.
		//
		// Replaying the raw response here instead handed the CLIENT a bare tool_use for a
		// tool the client does not implement. On an agent's own compaction request that
		// reads as a summary that came back empty, and three of those disable auto-compact
		// for the session — so the advertise condition was narrowed to marker-bearing turns
		// to avoid ever reaching this line. It bought that safety with the whole prompt-cache
		// prefix, on every turn where the marker set changed. The cost of paying it here
		// instead is one bounded upstream round (maxExpandRounds still caps the loop), and
		// only when a model asks for an id that has aged out of the store.
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
	setUpstreamAuth(req.Header, up)
	return h.client.Do(req)
}

// setUpstreamAuth applies the route's credential decision to the FORWARDED request.
//
// Default (no server-held key): the caller's own provider credential goes upstream
// untouched — their key, their bill. Only our own token is scrubbed out of the auth
// slots, because a token is not a provider credential and must never leave.
//
// Gateway mode (a server key IS configured): every auth slot is dropped and the
// server key injected, because the caller holds only a placeholder.
//
// It is not the only place a credential leaves the box, and the comment here used to
// claim it was. A NeedsModel component with `model.source: incoming` calls the same
// upstream through its own client (incomingModel), which is a second exit for a second
// credential — and while the comment said otherwise, the two disagreed in gateway mode:
// this function injected the server's key while that client carried the caller's to the
// same host. There is one DECISION, `up.setKey`, and both exits now read it; that is the
// invariant, and it holds because neither side derives it independently.
func setUpstreamAuth(dst http.Header, up upstream) {
	if up.setKey == nil {
		scrubToken(dst)
		return
	}
	for _, hd := range authHeaders {
		dst.Del(hd)
	}
	dst.Del(TokenHeader)
	up.setKey(dst)
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
	snap.LLMTruncated = offload.LLMTruncated()
	// summarize owns a separate budget (one call over a whole span, not one tool
	// output), so it reports separately — folded together, a summarize-only pipeline
	// would show llm_timeouts 0 while its own deadline expired on every request.
	snap.SummarizeTimeouts = offload.SummarizeTimeouts()
	snap.SummarizeErrors = offload.SummarizeErrors()
	snap.SummarizeCallTimeoutMs = offload.SummarizeCallTimeout().Milliseconds()
	// agentdiet owns a third budget (a window of steps, between one tool output and a
	// whole span), and runs in its own arm — so it reports its own counters too.
	snap.AgentDietTimeouts = offload.AgentDietTimeouts()
	snap.AgentDietErrors = offload.AgentDietErrors()
	snap.AgentDietCallTimeoutMs = offload.AgentDietCallTimeout().Milliseconds()
	// Freeze-replay health, same layering: the counters live with the code that owns
	// them (offload for the replay path, the store for dropped/repaired decisions).
	// Reversibility's two failure causes, split because they need opposite responses and one of
	// them is an alert. `missing` means this proxy removed content, said it could be had back, and
	// then could not produce it. Nothing else in this snapshot can go non-zero for that: wasted
	// tokens counts successful re-serves, so a broken stash was indistinguishable from a session
	// that simply never called expand. See expand/unresolved.go.
	snap.ExpandUnresolvedMalformed, snap.ExpandUnresolvedMissing = expand.Unresolved()
	snap.FrozenHits, snap.FrozenMisses = offload.FrozenStats()
	if fl, ok := h.store.(*store.Memory); ok { // process store; hosted per-tenant stores report via the dashboard
		snap.FrozenDropped, snap.FrozenRepaired = fl.FrozenLossStats()
		snap.FrozenFlips = snap.FrozenDropped - snap.FrozenRepaired
	}
	// Cached-prefix restarts after an agent compaction. Same layering as the pool counters
	// below: the counter lives in `modes`, so the host merges it here.
	snap.CompactionResets = modes.CompactionResets()
	// The idle keep-alive's ledger, same layering as the pool counters below: the mechanism
	// lives in `proxy` because it acts between requests, so `metrics` cannot reach it. Only
	// published once it has done something — a deployment where nobody opted in shows no
	// field at all rather than a row of zeroes that reads as a broken feature.
	if ka := h.keeper.Stats(); ka.Live > 0 || ka.Pings > 0 || ka.Skipped > 0 {
		snap.KeepAlive = ka
	}
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
	tn, err := h.authenticate(r)
	if err != nil {
		h.refuse(w, r, err)
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
//
// Cookie is dropped, which is the request direction only and therefore safe here even
// though this function is also used response→client (writeRaw, stream): Set-Cookie is a
// different header and is untouched. The reason is the Bob catch-all — it forwards
// verbatim, so a client that presented both a context-guru token and its cg_dash
// DASHBOARD cookie shipped a live browser session to a third-party host. Observed:
// the upstream received `Cookie: cg_dash=<session>`. A browser session is not an upstream
// credential, and no upstream has any use for one.
//
// It belongs here rather than in setUpstreamAuth because setUpstreamAuth's gateway branch
// deletes only the auth slots — and gateway mode is what the hosted deployment runs. Every
// forward path goes through this function; that is the property that makes one line enough.
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
	dst.Del("Cookie") // a browser session is not an upstream credential
}
