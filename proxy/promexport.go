package proxy

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/metrics"
)

// Prometheus exposition at /metrics, for Grafana.
//
// Hand-written rather than pulling in prometheus/client_golang. The text format is a
// handful of lines per series, everything here is already computed by the Aggregator
// and the dashboard's store, and the client library would bring a dependency tree plus
// its own registry, collectors and histogram machinery to serialise numbers we already
// have. If native histograms or exemplars are ever wanted, that is the moment to take
// the dependency — not before.
//
// Two families of series:
//
//   - cg_* process-wide, straight off metrics.Aggregator, the same numbers /stats
//     reports. Cheap: one snapshot, no I/O. In-memory, so they start at 0 with the
//     process and cover every tenant — see procCaveat, which says so in the HELP text of
//     every one of them, because these numbers sit beside the dashboard's in Grafana and
//     legitimately disagree with them.
//   - cg_tenant_* per tenant, from the dashboard database. These cost a SQL query, so
//     they are cached for a scrape interval — Grafana will scrape every 15s and a query
//     per scrape per tenant would make observability the load.

// promCacheTTL bounds how stale per-tenant series may be. Just under a typical 15s
// scrape so consecutive scrapes do not serve one cached copy twice.
const promCacheTTL = 10 * time.Second

// Refusals: every way a request can be turned away before it reaches an upstream.
//
// These exist because without them a tenant that is rate-limited or over its budget
// sees failures while the operator's dashboard shows a perfectly healthy service — and
// because a rejection count is the only thing that tells "this tenant is quiet" apart
// from "this tenant is blocked".
//
// The reason is a CLOSED set of constants, never a status line or an error string. A
// label taken from an error message would let a caller mint series at will, and
// tenant × reason is the one place cardinality here could run away.
type refusalReason string

const (
	refuseRateLimit   refusalReason = "rate_limit"     // 429, per-tenant requests/minute
	refuseConcurrency refusalReason = "concurrency"    // 429, per-tenant in-flight cap
	refuseAuth        refusalReason = "auth"           // 401, missing/unknown token
	refuseForbidden   refusalReason = "forbidden"      // 403, disabled account or a gated view
	refuseNoUpstream  refusalReason = "no_upstream"    // 502, nothing configured for the route
	refuseUpstream    refusalReason = "upstream_error" // 502, the upstream call itself failed
	// refuseNoProviderKey is 401 too, and it is the one 401 that is not an authentication
	// failure at all: the account is known and enabled, and what is missing is the
	// caller's OWN provider credential (see errNoProviderKey and refuseRoute). It gets its
	// own series because it is the only refusal in this list the USER can fix, because it
	// becomes the dominant one as soon as a deployment stops injecting a server-held key,
	// and because it is the only one recorded with a tenant on a 401 — so the operator can
	// name the accounts to contact instead of counting them.
	//
	// It PARTITIONS `auth` rather than refining it: a request refused for a missing provider
	// credential is counted here and NOT under auth (see failAuthAs). That is deliberate —
	// the SLO dashboard divides an UNLABELLED sum of this family by refusals + requests, so a
	// request counted under two reasons would inflate the error-rate SLI by one.
	refuseNoProviderKey refusalReason = "no_provider_key"
)

// refusalReasons is the exposition order, and the reason every series is present with a
// value of 0 rather than absent: a Grafana rate() over a family that only appears once
// something breaks renders "No data", which reads as healthy.
var refusalReasons = []refusalReason{refuseRateLimit, refuseConcurrency,
	refuseAuth, refuseNoProviderKey, refuseForbidden, refuseNoUpstream, refuseUpstream}

// refusalTotals is the process-wide count per reason. Built once and never written
// again, so an increment on the request path is one map read and one atomic add — no
// lock, and it works in single-tenant mode where there is no tenant to label.
var refusalTotals = func() map[refusalReason]*atomic.Int64 {
	m := make(map[refusalReason]*atomic.Int64, len(refusalReasons))
	for _, r := range refusalReasons {
		m[r] = new(atomic.Int64)
	}
	return m
}()

type refusalKey struct {
	tenant string
	reason refusalReason
}

// refusalByTenant breaks the same counts down per tenant. A sync.Map rather than a
// mutex-guarded map because this is the request path and the steady state is a Load of
// an existing key; the value is an atomic, so two concurrent refusals never contend.
var (
	refusalByTenant sync.Map // refusalKey -> *atomic.Int64
	refusalKeyCount atomic.Int64
)

// maxRefusalKeys bounds the per-tenant breakdown. The tenant set is already bounded by
// the registry, so this is a backstop, not the primary bound — but a metrics map that
// grows with traffic is the kind of thing that only shows up as a memory leak in
// production, and the process-wide totals keep counting after the cap is hit.
const maxRefusalKeys = 2048

// recordRefusal counts one refused request.
//
// tenantID must be a registry tenant id or "". Empty means process-wide only, which is
// the right answer twice over: in single-tenant mode there is no tenant, and a caller
// that failed to authenticate has no identity we are willing to turn into a label.
func recordRefusal(reason refusalReason, tenantID string) {
	if c := refusalTotals[reason]; c != nil {
		c.Add(1)
	}
	if tenantID == "" {
		return
	}
	k := refusalKey{tenant: tenantID, reason: reason}
	if c, ok := refusalByTenant.Load(k); ok {
		c.(*atomic.Int64).Add(1)
		return
	}
	if refusalKeyCount.Load() >= maxRefusalKeys {
		return
	}
	c, loaded := refusalByTenant.LoadOrStore(k, new(atomic.Int64))
	if !loaded {
		refusalKeyCount.Add(1)
	}
	c.(*atomic.Int64).Add(1)
}

// refusalSnapshot reads both families for one scrape.
func refusalSnapshot() (map[refusalReason]int64, map[string]map[refusalReason]int64) {
	totals := make(map[refusalReason]int64, len(refusalReasons))
	for r, c := range refusalTotals {
		totals[r] = c.Load()
	}
	byTenant := map[string]map[refusalReason]int64{}
	refusalByTenant.Range(func(k, v any) bool {
		key := k.(refusalKey)
		if byTenant[key.tenant] == nil {
			byTenant[key.tenant] = map[refusalReason]int64{}
		}
		byTenant[key.tenant][key.reason] = v.(*atomic.Int64).Load()
		return true
	})
	return totals, byTenant
}

// TenantMetricsSource supplies per-tenant rollups for the exporter.
type TenantMetricsSource interface {
	// TenantMetrics returns one row per tenant that has traffic in the window.
	TenantMetrics(since int64) ([]TenantMetricRow, error)
}

// TenantMetricRow is one tenant's rollup. Deliberately flat: every field becomes one
// Prometheus series, and a nested shape would just have to be flattened here anyway.
type TenantMetricRow struct {
	TenantID                string
	Label                   string
	Requests                int64
	TokensBefore            int64
	TokensAfter             int64
	SavedUnique             int64
	CacheRead               int64
	CacheWrite              int64
	FreshInput              int64
	OutputTokens            int64
	CostUSD                 float64
	BaselineUSD             float64
	CGLLMCostUSD            float64
	CacheSavedUSD           float64
	CachesplitSavedUSD      float64
	CachesplitHistoricalUSD float64
	CGLatencyMs             float64
	UpstreamMs              float64
	Sessions                int64
	ArchivedCount           int64
	ArchivedBytes           int64
}

type promCache struct {
	mu   sync.Mutex
	at   time.Time
	body string
}

// metricsHandler serves the Prometheus endpoint.
//
// Gated exactly like /stats, and for the same reason: this is a service-wide view
// across every tenant, including per-tenant cost. Loopback is allowed by default
// because Prometheus normally runs beside the proxy; a remote scraper needs
// METRICS_TOKEN.
func (h *Handler) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if !h.metricsAllowed(r) {
		failAuth(w, statusError{http.StatusForbidden,
			"/metrics is a service-wide view; scrape it from loopback or set METRICS_TOKEN"})
		return
	}
	h.promCache.mu.Lock()
	if time.Since(h.promCache.at) < promCacheTTL && h.promCache.body != "" {
		body := h.promCache.body
		h.promCache.mu.Unlock()
		writeMetrics(w, body)
		return
	}
	h.promCache.mu.Unlock()
	// Rendered OUTSIDE the lock: renderMetrics does real work (a per-tenant DB query), and
	// holding the lock across it used to serialize every concurrent scraper behind whichever
	// one rendered first — one slow render, and every other caller queues up behind it rather
	// than getting the stale-but-fine cached body. A cache-miss race now costs at most one
	// redundant render, not a growing queue.
	body := h.renderMetrics()
	h.promCache.mu.Lock()
	h.promCache.at, h.promCache.body = time.Now(), body
	h.promCache.mu.Unlock()
	writeMetrics(w, body)
}

func writeMetrics(w http.ResponseWriter, body string) {
	// version=0.0.4 is the classic text exposition format; naming it explicitly stops
	// a scraper guessing.
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// metricsAllowed gates the endpoint. A bearer token is accepted so Prometheus can
// scrape from another host; loopback needs none, which keeps the common
// same-box deployment configuration-free.
func (h *Handler) metricsAllowed(r *http.Request) bool {
	if tok := h.opts.MetricsToken; tok != "" {
		// Constant time: a byte-by-byte == leaks the shared secret's prefix to a
		// scraper that can time its own requests.
		// Read the auth slot directly rather than through TokenFromRequest: the scrape
		// credential is the OPERATOR's shared secret, not a cg_live_ tenant token, so it
		// would not survive that function's shape check.
		if subtle.ConstantTimeCompare([]byte(headerCredential(r.Header.Get("Authorization"))), []byte(tok)) == 1 {
			return true
		}
	}
	// In single-tenant mode there is no cross-tenant data to protect, so the endpoint
	// is as open as /stats has always been.
	if h.opts.Tenants == nil {
		return true
	}
	return h.statsTrusted(r)
}

// promLine writes one sample. Labels are pre-escaped by the caller's use of
// escapeLabel; the value uses %g so integers stay integral and floats keep precision.
func promLine(b *strings.Builder, name, labels string, v float64) {
	if labels == "" {
		fmt.Fprintf(b, "%s %g\n", name, v)
		return
	}
	fmt.Fprintf(b, "%s{%s} %g\n", name, labels, v)
}

func promHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// procCaveat is appended to the HELP of every series read from the in-process
// Aggregator, which is all of the cg_* family below.
//
// It exists because these numbers sit next to the dashboard's in Grafana and disagree
// with it — observed live: /metrics said 24 requests / 28,644 tokens-before while the
// dashboard said 26 / 28,656 — for two reasons that are both correct behaviour and
// neither of which was written down anywhere the reader would look. The aggregator is
// memory: it starts at zero when the process starts, while the dashboard reads the
// persistent database and keeps history across restarts. And it is process-wide across
// every tenant, while the dashboard is scoped to one. HELP text is the fix because it
// travels with the metric into every scraper, explorer and panel tooltip.
//
// The series are NOT re-sourced from the store: their meaning is "what this process did",
// /stats reports the same snapshot, deploy/harbor/*.py reads /stats, and the persistent
// per-tenant view already exists as cg_tenant_*.
const procCaveat = " Counted in THIS PROCESS since it started and summed over every " +
	"tenant: it restarts from 0 with the process (a rate() handles that) and it will not " +
	"equal the dashboard's figure for the same window, which is database-backed and " +
	"tenant-scoped. Use the cg_tenant_* series for the persistent, per-tenant numbers."

// monthToDateCaveat marks a per-tenant series as a MONTH-TO-DATE gauge rather than a
// counter, and says why.
//
// These four families were typed `counter` and are not one. They are re-queried from the
// store for the current calendar month, so they reset to 0 at the month boundary — and
// they SHRINK mid-month whenever request rows migrate to Box archival. rate() and
// increase() both treat a fall as a counter reset and extrapolate a spike at exactly the
// moment the number went DOWN. The _total suffix is kept because dashboards and scrapes
// already reference these names; the TYPE is what a query engine acts on.
func monthToDateCaveat(help string) string {
	return help + " A MONTH-TO-DATE GAUGE, not a counter, despite the _total name: it is " +
		"re-read from the store for the current calendar month, so it resets at the month " +
		"boundary and FALLS mid-month as request rows migrate to cold storage. Do not wrap it " +
		"in rate() or increase() — both read a fall as a counter reset and extrapolate a spike " +
		"exactly where the value went down. For a per-second rate use the process-wide cg_* " +
		"counters instead."
}

// promHeaderProc writes a header for an in-process series, caveat attached.
func promHeaderProc(b *strings.Builder, name, help, typ string) {
	promHeader(b, name, help+procCaveat, typ)
}

// escapeLabel escapes a label value per the exposition format. Tenant labels are
// hex ids and operator-supplied labels, so this is belt and braces — but a stray
// newline in a label value would corrupt every series after it.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func (h *Handler) renderMetrics() string {
	var b strings.Builder
	b.Grow(8 << 10)

	// --- process-wide -------------------------------------------------------
	if h.agg != nil {
		s := h.agg.Snapshot()

		promHeaderProc(&b, "cg_requests_total", "Requests processed by context-guru.", "counter")
		promLine(&b, "cg_requests_total", "", float64(s.Requests))

		promHeaderProc(&b, "cg_tokens_before_total", "Content tokens seen before compaction.", "counter")
		promLine(&b, "cg_tokens_before_total", "", float64(s.TokensBefore))
		promHeaderProc(&b, "cg_tokens_after_total", "Content tokens after compaction.", "counter")
		promLine(&b, "cg_tokens_after_total", "", float64(s.TokensAfter))
		promHeaderProc(&b, "cg_saved_tokens_total",
			"Tokens removed, counted gross (the same compaction re-counted on every turn the agent re-sends its transcript).", "counter")
		promLine(&b, "cg_saved_tokens_total", "", float64(s.SavedTokens))
		// The aggregate unique figure is the sum of the per-component ones: the Snapshot
		// carries unique savings per component, and summing here keeps one definition of
		// "unique" rather than inventing a second that could drift from /stats.
		var uniqueTotal int64
		for _, c := range s.Components {
			uniqueTotal += c.SavedUnique
		}
		promHeaderProc(&b, "cg_saved_tokens_unique_total",
			"Tokens removed, deduplicated by content — the honest figure. The gross counter over-counts by ~13x on agent traffic, because an agent re-sends its whole transcript every turn.", "counter")
		promLine(&b, "cg_saved_tokens_unique_total", "", float64(uniqueTotal))
		promHeaderProc(&b, "cg_savings_ratio", "Fraction of content tokens removed.", "gauge")
		promLine(&b, "cg_savings_ratio", "", s.SavingsPct/100)

		promHeaderProc(&b, "cg_billed_tokens_total", "Provider-billed tokens by tier.", "counter")
		promLine(&b, "cg_billed_tokens_total", `tier="fresh_input"`, float64(s.FreshInputTokens))
		promLine(&b, "cg_billed_tokens_total", `tier="cache_read"`, float64(s.CacheReadTokens))
		promLine(&b, "cg_billed_tokens_total", `tier="cache_write"`, float64(s.CacheWriteTokens))
		promLine(&b, "cg_billed_tokens_total", `tier="output"`, float64(s.OutputTokens))

		// Cache hit rate is THE number to alert on: compaction that breaks the provider
		// prefix cache costs more than the tokens it saves, and this is where that shows
		// up first.
		if hit := s.CacheReadTokens + s.FreshInputTokens; hit > 0 {
			promHeaderProc(&b, "cg_cache_hit_ratio",
				"Cache-read tokens over cache-read plus fresh input. A fall here is the first sign compaction is churning the provider's prefix cache.", "gauge")
			promLine(&b, "cg_cache_hit_ratio", "", float64(s.CacheReadTokens)/float64(hit))
		}

		promHeaderProc(&b, "cg_added_latency_ms", "Mean latency context-guru itself adds per request.", "gauge")
		promLine(&b, "cg_added_latency_ms", "", s.AddedLatencyMsAvg)
		promHeaderProc(&b, "cg_upstream_latency_ms", "Mean upstream latency.", "gauge")
		promLine(&b, "cg_upstream_latency_ms", "", s.UpstreamMsAvg)

		promHeaderProc(&b, "cg_expand_bounces_total",
			"Times the model had to call context_guru_expand to recover offloaded content. Rising means compaction is hiding things the agent still needs.", "counter")
		promLine(&b, "cg_expand_bounces_total", "", float64(s.Bounces))
		promHeaderProc(&b, "cg_wasted_tokens_total", "Tokens spent recovering offloaded content.", "counter")
		promLine(&b, "cg_wasted_tokens_total", "", float64(s.WastedTokens))

		promHeaderProc(&b, "cg_frozen_decisions_total", "Freeze-replay outcomes for compaction decisions.", "counter")
		promLine(&b, "cg_frozen_decisions_total", `outcome="hit"`, float64(s.FrozenHits))
		promLine(&b, "cg_frozen_decisions_total", `outcome="miss"`, float64(s.FrozenMisses))
		promLine(&b, "cg_frozen_decisions_total", `outcome="dropped"`, float64(s.FrozenDropped))
		promLine(&b, "cg_frozen_decisions_total", `outcome="repaired"`, float64(s.FrozenRepaired))

		promHeaderProc(&b, "cg_sse_streams_total", "Responses by streaming path.", "counter")
		promLine(&b, "cg_sse_streams_total", `path="streamed"`, float64(s.SSEStreamed))
		promLine(&b, "cg_sse_streams_total", `path="buffered"`, float64(s.SSEBuffered))
		promHeaderProc(&b, "cg_sse_ttfb_ms", "Mean time to first byte on streamed responses.", "gauge")
		promLine(&b, "cg_sse_ttfb_ms", "", s.SSETTFBMsAvg)

		// Per component: which parts of the pipeline are earning their place.
		// The outcomes are NESTED, not disjoint: acted ⊆ mutated ⊆ ran. `acted` means the
		// run removed content tokens; `mutated` means it changed the request at all. A
		// component can be mutated-never-acted BY DESIGN — cachesplit moves tokens out of
		// the hashed prefix without removing any, so acted/ran reads 0% and the one
		// component with a measured -34.1% cost effect renders as dead. Any "is this
		// component doing anything?" panel wants mutated. `discarded` is the odd one out:
		// it counts CHANGES the writeback layer threw away, not runs, and it is here
		// because the family is where an operator already looks.
		promHeaderProc(&b, "cg_component_runs_total",
			"Component invocations by outcome. acted (removed content tokens) is a subset of "+
				"mutated (changed the request at all), which is a subset of ran — a component "+
				"that mutates without saving content tokens, like cachesplit moving tokens out "+
				"of the cached prefix, is working. discarded counts changes the writeback layer "+
				"threw away, so it is a count of changes rather than of runs.", "counter")
		names := make([]string, 0, len(s.Components))
		for name := range s.Components {
			names = append(names, name)
		}
		sort.Strings(names) // stable output; a scrape diff should not reorder every line
		for _, name := range names {
			c := s.Components[name]
			l := `component="` + escapeLabel(name) + `"`
			promLine(&b, "cg_component_runs_total", l+`,outcome="ran"`, float64(c.Runs))
			promLine(&b, "cg_component_runs_total", l+`,outcome="acted"`, float64(c.Acted))
			promLine(&b, "cg_component_runs_total", l+`,outcome="mutated"`, float64(c.Mutated))
			promLine(&b, "cg_component_runs_total", l+`,outcome="discarded"`, float64(c.Discarded))
			promLine(&b, "cg_component_runs_total", l+`,outcome="reverted"`, float64(c.Reverted))
		}
		// Why a component declined. Without this, `acted: 0` is undiagnosable: it cannot
		// tell a misfiring guard from a workload with genuinely nothing to do. The gate
		// histogram is already in the snapshot (deep-copied, race-free); the same data is in
		// the logs as ONE string field, which is why grouping a Loki panel by it mints a
		// series per request and hits the 500-series ceiling.
		//
		// Cardinality is bounded by code, not by traffic: gate names are constants in the
		// components, ~7 components x <=8 gates.
		promHeaderProc(&b, "cg_component_gate_declines_total",
			"Candidates a component's named gate turned away. This is the answer to \"it ran "+
				"18,288 times and acted 0 times, is it broken?\" — e.g. toon declining 14,675 "+
				"candidates as not_uniform_object_array is the component working as designed.", "counter")
		for _, name := range names {
			gates := s.Components[name].Gates
			gateNames := make([]string, 0, len(gates))
			for g := range gates {
				gateNames = append(gateNames, g)
			}
			sort.Strings(gateNames) // stable output, same reason as the component order
			for _, g := range gateNames {
				promLine(&b, "cg_component_gate_declines_total",
					`component="`+escapeLabel(name)+`",gate="`+escapeLabel(g)+`"`, float64(gates[g]))
			}
		}
		promHeaderProc(&b, "cg_component_saved_tokens_unique_total",
			"Tokens each component removed, deduplicated by content.", "counter")
		for _, name := range names {
			promLine(&b, "cg_component_saved_tokens_unique_total",
				`component="`+escapeLabel(name)+`"`, float64(s.Components[name].SavedUnique))
		}
		promHeaderProc(&b, "cg_component_duration_ms_total", "Cumulative time in each component.", "counter")
		for _, name := range names {
			promLine(&b, "cg_component_duration_ms_total",
				`component="`+escapeLabel(name)+`"`, s.Components[name].DurationMs)
		}
	}

	// --- refusals -----------------------------------------------------------
	//
	// Outside the h.agg guard: a refused request never reaches the aggregator, and these
	// counters are exactly what is wanted on a proxy with no /stats rollups at all.
	refusals, refusalsByTenant := refusalSnapshot()
	promHeader(&b, "cg_refused_requests_total",
		"Requests refused before an upstream was called, by reason. Any sustained rate here is somebody's agent failing: rate_limit and concurrency are both 429 but have different fixes (raise the rate, raise the in-flight cap), auth/forbidden mean a bad or disabled token, no_provider_key means the account is fine but sent no provider credential of its own — split out of auth because it is the only refusal here the USER fixes, and the one a credential migration has to be able to count, no_upstream is our own misconfiguration and upstream_error is the provider.", "counter")
	for _, r := range refusalReasons {
		promLine(&b, "cg_refused_requests_total", `reason="`+string(r)+`"`, float64(refusals[r]))
	}
	if len(refusalsByTenant) > 0 {
		// Labels come from the registry, like every other per-tenant series, and for the
		// same reason: {{label}} in a legend is unreadable when it is blank. The EMAIL is
		// deliberately absent here too — see tenantLabels.
		byID := map[string]string{}
		if reg := h.registry(); reg != nil {
			if all, err := reg.List(); err == nil {
				for _, t := range all {
					byID[t.ID] = t.Label
				}
			}
		}
		ids := make([]string, 0, len(refusalsByTenant))
		for id := range refusalsByTenant {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		promHeader(&b, "cg_tenant_refused_requests_total",
			"Requests refused per tenant, by reason. This is the one series that tells a quiet tenant apart from a blocked one. Only reasons that have fired are present, so the family is bounded by tenants actually being refused.", "counter")
		for _, id := range ids {
			l := `tenant="` + escapeLabel(id) + `",label="` + escapeLabel(byID[id]) + `"`
			for _, r := range refusalReasons {
				if n := refusalsByTenant[id][r]; n > 0 {
					promLine(&b, "cg_tenant_refused_requests_total",
						l+`,reason="`+string(r)+`"`, float64(n))
				}
			}
		}
	}

	// context-guru's own compaction-model spend. Separate from the agent's cost because
	// conflating them makes it impossible to see whether the tool pays for itself.
	calls, in, out := cheapmodel.Usage()
	promHeader(&b, "cg_llm_calls_total", "Calls context-guru made to its own compaction model.", "counter")
	promLine(&b, "cg_llm_calls_total", "", float64(calls))
	promHeader(&b, "cg_llm_tokens_total", "Tokens context-guru spent on its own compaction model.", "counter")
	promLine(&b, "cg_llm_tokens_total", `direction="input"`, float64(in))
	promLine(&b, "cg_llm_tokens_total", `direction="output"`, float64(out))
	promHeader(&b, "cg_llm_failures_total", "Compaction-model calls that timed out or errored.", "counter")
	promLine(&b, "cg_llm_failures_total", `kind="timeout"`, float64(offload.LLMTimeouts()))
	promLine(&b, "cg_llm_failures_total", `kind="error"`, float64(offload.LLMErrors()))

	// extract_llm economics. The one component that SPENDS money, so gross savings can look
	// impressive while it is underwater — measured live at a NET of -$0.7085, and until these
	// series existed that number appeared nowhere a monitor could reach it (only in /stats,
	// which nothing alerts on). Priced exactly as statsHandler prices it, so /metrics and
	// /stats cannot disagree: same cheapmodel usage, same per-saved-token rate.
	cacheWrite, cacheRead := cheapmodel.CacheUsage()
	perSavedTok := agentCacheReadPerMTok / 1e6
	if h.opts.CacheMode == "off" {
		perSavedTok = agentFreshPerMTok / 1e6
	}
	xs := metrics.ExtractSnapshot(
		cheapmodel.PricingFromEnv().Cost(in, out, cacheWrite, cacheRead), perSavedTok, cacheWrite, cacheRead)
	promHeader(&b, "cg_extract_calls_total",
		"Extraction calls by outcome: made is an LLM call paid for, avoided is a result-cache hit (the cheapest outcome), suppressed is the economic gate declining one.", "counter")
	promLine(&b, "cg_extract_calls_total", `outcome="made"`, float64(xs.Calls))
	promLine(&b, "cg_extract_calls_total", `outcome="avoided"`, float64(xs.CallsAvoided))
	promLine(&b, "cg_extract_calls_total", `outcome="suppressed"`, float64(xs.CallsSuppressed))
	promHeader(&b, "cg_extract_cost_usd", "What extraction spent on its own model calls.", "gauge")
	promLine(&b, "cg_extract_cost_usd", "", xs.ExtractionCostUSD)
	promHeader(&b, "cg_extract_net_value_usd",
		"What extraction's own saved tokens are worth at the rate they would have been billed, MINUS what it spent. NEGATIVE means the component is underwater and should be turned off — alert on it.", "gauge")
	promLine(&b, "cg_extract_net_value_usd", "", xs.NetValueUSD)
	promHeader(&b, "cg_extract_latency_ms",
		"Mean wall time per extraction call. The gate stops speculative calls once this is observed to be slow, so it is an input as well as a symptom.", "gauge")
	promLine(&b, "cg_extract_latency_ms", "", xs.AvgLatencyMs)
	promHeader(&b, "cg_extract_gate_declines_total",
		"Why extraction ran or was suppressed, per reason — the first question about an expensive component is always why it fired.", "counter")
	reasons := make([]string, 0, len(xs.Reasons))
	for r := range xs.Reasons {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		promLine(&b, "cg_extract_gate_declines_total", `reason="`+escapeLabel(r)+`"`, float64(xs.Reasons[r]))
	}

	// --- dashboard / storage health ----------------------------------------
	if h.rec != nil {
		st := h.rec.Stats()
		promHeader(&b, "cg_dash_events_total", "Dashboard capture events by disposition.", "counter")
		promLine(&b, "cg_dash_events_total", `disposition="captured"`, float64(st.Captured))
		promLine(&b, "cg_dash_events_total", `disposition="written"`, float64(st.Written))
		// Dropped events mean the capture queue filled — observability degrading under
		// load, which is exactly when it is most wanted. Worth an alert.
		promLine(&b, "cg_dash_events_total", `disposition="dropped"`, float64(st.Dropped))
		promHeader(&b, "cg_dash_db_bytes", "Size of the local dashboard database, write-ahead log included.", "gauge")
		promLine(&b, "cg_dash_db_bytes", "", float64(h.rec.DBSizeBytes()))
		promHeader(&b, "cg_dash_disk_used_ratio",
			"Fraction of the filesystem holding the dashboard database that is in use. Crossing the high watermark triggers session eviction.", "gauge")
		if frac, ok := h.rec.DiskUsedFraction(); ok {
			promLine(&b, "cg_dash_disk_used_ratio", "", frac)
		}
		promHeader(&b, "cg_archive_sessions_total", "Sessions moved to cold storage.", "counter")
		promLine(&b, "cg_archive_sessions_total", "", float64(h.rec.ArchivedSessionCount()))
		promHeader(&b, "cg_archive_bytes_total", "Bytes stored in cold storage.", "counter")
		promLine(&b, "cg_archive_bytes_total", "", float64(h.rec.ArchivedBytes()))
		// --- the value of the thing --------------------------------------------
		//
		// Until these four existed /metrics could plot every input to the question "is this
		// proxy worth running" and not the answer: tokens, ratios, latency, cache outcomes,
		// and exactly one component's net value in dollars. The dashboard could show it and
		// Grafana could not. Month to date, from the store, matching the per-tenant window
		// below so a service-wide panel and a per-tenant one can never disagree.
		now := time.Now().UTC()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		if v, err := h.rec.ValueMetrics(monthStart); err != nil {
			// Never silently: a family that vanishes reads as "no traffic" in Grafana.
			slog.Warn("context-guru: value metrics query failed; the dollar series are absent from this scrape",
				"err", err)
		} else {
			promHeader(&b, "cg_baseline_cost_usd", monthToDateCaveat(
				"What this month's traffic would have cost with nothing removed: the billed cost plus the removed tokens priced at the tier each request actually paid. Subtract cg_saved_usd to get the bill."), "gauge")
			promLine(&b, "cg_baseline_cost_usd", "", v.BaselineUSD)
			promHeader(&b, "cg_saved_usd", monthToDateCaveat(
				"Provider spend compaction avoided this month: baseline minus billed. BEFORE context-guru's own model spend — use cg_net_saved_usd for the verdict."), "gauge")
			promLine(&b, "cg_saved_usd", "", v.SavedUSD)
			promHeader(&b, "cg_net_saved_usd", monthToDateCaveat(
				"cg_saved_usd minus what context-guru's own compaction models cost. GOES NEGATIVE when a configuration spends more than it saves; that is a real outcome and this series reports it rather than clamping at zero. This is the number to alert on."), "gauge")
			promLine(&b, "cg_net_saved_usd", "", v.NetSavedUSD)
			promHeader(&b, "cg_frozen_tokens_total", monthToDateCaveat(
				"Content tokens cache-aware compaction deliberately left alone because they sat inside the provider's already-cached prefix. The headroom, and usually the explanation for a small cg_saved_usd: rewriting a cached prefix costs more than the tokens it removes, so this is a cost paid on purpose. DO NOT read a low value as \"the cache is cold, it is safe to rewrite deep history\". Zero has TWO causes and they call for opposite actions. (1) OUR OWN prefix tracker was reset (a restart, an evicted entry) while the provider's cache was still warm: measured, 3,092 such requests still cache-HIT for 404,376,878 cache-read tokens, and acting on that reading was worth about -$708 on sonnet-5 against +$0.62 of upside. (2) The turn was a CONFIRMED cold one and the cold sweep already acted, which is free by construction. Tell them apart by the request's cache_miss_reason: a swept turn reads ttl_expiry/cold_start, a reset tracker reads hit. Ctx.ColdCache cannot confuse the two -- it demands a RECORDED previous turn plus the provider TTL and a one-minute margin, so a missing record reads warm, which is case (1) declining itself."), "gauge")
			promLine(&b, "cg_frozen_tokens_total", "", float64(v.FrozenTokens))
		}

		promHeader(&b, "cg_archive_configured", "1 when cold storage is configured and reachable.", "gauge")
		// RemoteReachable, not RemoteName: the name is now reported for a CONFIGURED
		// remote even when the boot probe failed (so the dashboard can distinguish "not
		// configured" from "unreachable"), and this gauge documents both conditions.
		configured := 0.0
		if h.rec.RemoteReachable() {
			configured = 1
		}
		promLine(&b, "cg_archive_configured", "", configured)
	}

	// --- per tenant ---------------------------------------------------------
	if h.opts.TenantMetrics != nil {
		// Month to date, matching the window the settings page reports, so a Grafana
		// panel and a tenant's own view can never disagree about what they have spent.
		now := time.Now().UTC()
		since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		rows, err := h.opts.TenantMetrics.TenantMetrics(since)
		// The label lives in the control database; the request rows only carry the id.
		// Without this merge every per-tenant series is label="", and a Grafana legend of
		// {{label}} renders a column of blanks — the panels work but nobody can read them.
		if reg := h.registry(); reg != nil && err == nil {
			if all, lerr := reg.List(); lerr == nil {
				byID := make(map[string]string, len(all))
				for _, t := range all {
					byID[t.ID] = t.Label
				}
				for i := range rows {
					if rows[i].Label == "" {
						rows[i].Label = byID[rows[i].TenantID]
					}
				}
			}
		}
		if err != nil {
			// Never swallow this. A metric family that silently disappears is the worst
			// kind of monitoring failure: the Grafana panel goes blank, which reads as
			// "no traffic" rather than "the query is broken", and nobody investigates a
			// quiet dashboard.
			slog.Warn("context-guru: per-tenant metrics query failed; those series are absent from this scrape",
				"err", err)
		}
		if err == nil {
			promHeader(&b, "cg_tenant_requests_total", monthToDateCaveat("Requests this calendar month, per tenant."), "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_requests_total", tenantLabels(t), float64(t.Requests))
			}
			promHeader(&b, "cg_tenant_cost_usd", "Provider spend this calendar month, per tenant.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_cost_usd", tenantLabels(t), t.CostUSD)
			}
			promHeader(&b, "cg_tenant_baseline_cost_usd",
				"What this tenant's traffic would have cost without compaction. Subtract cg_tenant_cost_usd for the saving.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_baseline_cost_usd", tenantLabels(t), t.BaselineUSD)
			}
			promHeader(&b, "cg_tenant_cg_llm_cost_usd",
				"What context-guru's own compaction model cost this tenant. Compare against the saving to see whether it paid for itself.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_cg_llm_cost_usd", tenantLabels(t), t.CGLLMCostUSD)
			}
			promHeader(&b, "cg_tenant_cache_saved_usd",
				"DIAGNOSTIC, not a saving of ours: what the PROVIDER's prompt cache saved this tenant against paying the fresh rate for the same tokens. The agent places most of the breakpoints. Watch it because rewriting a live prefix destroys it — a fall here is a compaction pipeline going too deep.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_cache_saved_usd", tenantLabels(t), t.CacheSavedUSD)
			}
			promHeader(&b, "cg_tenant_cachesplit_saved_usd",
				"What context-guru's volatile-tail split (cachesplit) saved this tenant. Counted only where the split ran, the environment snapshot had MOVED since that session's previous request, and the provider then read at least the stable half from cache while writing less than it. The amount is the stable half, not the whole cache_read: with cachesplit off a real session's first request still read 45,805 tokens, so only the difference was ever ours. Priced against a cache miss (creation rate), because those tokens carry cache_control. Expect it to be SMALL against a warm-cache workload: Claude Code captures its environment block once per session, and a session start finds the previous session's prefix already expired unless it began inside the provider's TTL — measured, 1,105 of 1,127 starts were cold. A floor.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_cachesplit_saved_usd", tenantLabels(t), t.CachesplitSavedUSD)
			}
			promHeader(&b, "cg_tenant_cachesplit_historical_usd",
				"The same prefix-split saving as cg_tenant_cachesplit_saved_usd, but on requests written BEFORE it could be measured per request — valued on read from the model's measured stable half, never stored. Only the session's FIRST request of each of those, because crediting a mid-session turn needs the tail hash those rows do not carry. Add it to cg_tenant_cachesplit_saved_usd for the whole-history figure; it stops growing once the pre-instrumentation window ages out of retention.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_cachesplit_historical_usd", tenantLabels(t), t.CachesplitHistoricalUSD)
			}
			promHeader(&b, "cg_tenant_total_cost_usd",
				"What this tenant's requests cost IN TOTAL: the provider's billed cost for the traffic plus context-guru's own compaction-model spend. The bill, before any counterfactual — sum it for the deployment's total cost of all requests. cg_tenant_cost_usd alone is the provider half; cg_tenant_cg_llm_cost_usd is ours.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_total_cost_usd", tenantLabels(t), t.CostUSD+t.CGLLMCostUSD)
			}
			promHeader(&b, "cg_tenant_tokens_total", monthToDateCaveat("Content tokens per tenant this calendar month, before and after compaction."), "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_tokens_total", tenantLabels(t)+`,stage="before"`, float64(t.TokensBefore))
				promLine(&b, "cg_tenant_tokens_total", tenantLabels(t)+`,stage="after"`, float64(t.TokensAfter))
			}
			promHeader(&b, "cg_tenant_saved_tokens_unique_total", monthToDateCaveat("Tokens removed per tenant this calendar month, deduplicated."), "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_saved_tokens_unique_total", tenantLabels(t), float64(t.SavedUnique))
			}
			promHeader(&b, "cg_tenant_billed_tokens_total", monthToDateCaveat("Provider-billed tokens per tenant this calendar month, by tier."), "gauge")
			for _, t := range rows {
				l := tenantLabels(t)
				promLine(&b, "cg_tenant_billed_tokens_total", l+`,tier="cache_read"`, float64(t.CacheRead))
				promLine(&b, "cg_tenant_billed_tokens_total", l+`,tier="cache_write"`, float64(t.CacheWrite))
				promLine(&b, "cg_tenant_billed_tokens_total", l+`,tier="fresh_input"`, float64(t.FreshInput))
				promLine(&b, "cg_tenant_billed_tokens_total", l+`,tier="output"`, float64(t.OutputTokens))
			}
			promHeader(&b, "cg_tenant_sessions", "Distinct sessions this calendar month, per tenant.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_sessions", tenantLabels(t), float64(t.Sessions))
			}
			promHeader(&b, "cg_tenant_added_latency_ms", "Mean latency context-guru added, per tenant.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_added_latency_ms", tenantLabels(t), t.CGLatencyMs)
			}
			promHeader(&b, "cg_tenant_archived_sessions", "Sessions in cold storage, per tenant.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_archived_sessions", tenantLabels(t), float64(t.ArchivedCount))
			}
			promHeader(&b, "cg_tenant_archived_bytes", "Bytes in cold storage, per tenant.", "gauge")
			for _, t := range rows {
				promLine(&b, "cg_tenant_archived_bytes", tenantLabels(t), float64(t.ArchivedBytes))
			}
		}
	}

	if reg := h.registry(); reg != nil {
		if all, err := reg.List(); err == nil {
			promHeader(&b, "cg_tenant_disabled", "1 when a tenant is disabled.", "gauge")
			for _, t := range all {
				v := 0.0
				if t.Disabled {
					v = 1
				}
				promLine(&b, "cg_tenant_disabled",
					`tenant="`+escapeLabel(t.ID)+`",label="`+escapeLabel(t.Label)+`"`, v)
			}
		}
	}

	promHeader(&b, "cg_build_info", "Always 1; carries the build as labels.", "gauge")
	promLine(&b, "cg_build_info", `version="`+escapeLabel(h.opts.Version)+`"`, 1)
	return b.String()
}

// tenantLabels renders the identity labels every per-tenant series carries. The label
// is included alongside the id so a Grafana panel is readable without a join, and the
// EMAIL is deliberately absent: metrics are typically the least access-controlled
// surface in an organisation, and personal data does not belong in a scrape target.
func tenantLabels(t TenantMetricRow) string {
	return `tenant="` + escapeLabel(t.TenantID) + `",label="` + escapeLabel(t.Label) + `"`
}
