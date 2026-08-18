package cheapmodel

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
)

// Usage tracks cumulative token usage of the cheap (config-source) model across
// all NeedsModel component calls in this process. It is the basis for reporting
// the CONTEXT-GURU components' OWN LLM cost (e.g. extract:code's Starlark-writer
// calls) separately from the agent's cost — the proxy exposes it at /stats and the
// benchmark prices it. Process-global (there is a single cheap model per proxy);
// per-component attribution would need the Model interface to carry a label, a
// deferred refinement — today the LLM component in a config is extract, so the
// global total is that component's cost.
//
// The cache tiers are tracked separately because they are the whole question behind
// issue #28's part A: a preamble breakpoint below the model's minimum cacheable prefix
// is silently ignored, so cacheRead staying at 0 across many calls is the ONLY
// evidence that the split is not paying off. Never infer a cache win from placement.
var (
	llmCalls        atomic.Int64
	llmInputTokens  atomic.Int64
	llmOutputTokens atomic.Int64
	llmCacheWrite   atomic.Int64
	llmCacheRead    atomic.Int64
)

// Sink accumulates the cheap-model usage of ONE scope — in the proxy, one request. The
// totals above are a per-PROCESS fact and stay that way (/stats and the benchmark read
// them); a per-request bill cannot be derived from them by subtraction, because on a
// multi-tenant proxy any other tenant's call landing inside the subtraction window is
// billed to whoever happens to be in flight. That was the cg_llm_cost_usd defect: it
// propagated into tenant_spend and month-to-date spend, and let a tenant infer other
// tenants' compaction activity from its own rows.
//
// Carried on the CONTEXT rather than as a components.Ctx field, because the context is
// what already reaches every model call: apply builds components.Ctx from it, components
// derive their timeouts from it, and internal/extract sees only a context.Context. One
// plumbing point, and no call path can silently miss it.
type Sink struct {
	calls      atomic.Int64
	in, out    atomic.Int64
	cacheWrite atomic.Int64
	cacheRead  atomic.Int64
	// parent is the sink this one nests inside, so a narrower scope can be measured without
	// hiding the call from the wider one. The proxy installs a per-REQUEST sink; a component
	// that wants per-CALL numbers installs a child for the duration of one call, and the
	// usage must reach both or the request's own bill silently loses those tokens.
	parent *Sink
}

// Totals returns this scope's usage so far, at the same shape as Usage().
func (s *Sink) Totals() (calls, inTokens, outTokens int64) {
	if s == nil {
		return 0, 0, 0
	}
	return s.calls.Load(), s.in.Load(), s.out.Load()
}

// CacheTotals returns this scope's cache-tier tokens (write, read).
func (s *Sink) CacheTotals() (cacheWrite, cacheRead int64) {
	if s == nil {
		return 0, 0
	}
	return s.cacheWrite.Load(), s.cacheRead.Load()
}

type sinkKey struct{}

// WithSink scopes cheap-model accounting for everything done under ctx to s. A call made
// under a context with no sink still counts toward the process totals — nothing is lost,
// it is simply not attributable to one request.
func WithSink(ctx context.Context, s *Sink) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, s)
}

// WithCallSink scopes a NARROWER accounting window inside whatever sink already scopes ctx,
// returning the child so the caller can read exactly one call's usage. Usage recorded under
// the child also reaches every ancestor, so installing one never costs the request its own
// attribution.
func WithCallSink(ctx context.Context) (context.Context, *Sink) {
	child := &Sink{parent: SinkFrom(ctx)}
	return context.WithValue(ctx, sinkKey{}, child), child
}

// add records one call's usage on this sink and every sink it nests inside.
func (s *Sink) add(inTok, outTok, cacheWrite, cacheRead int) {
	// Nil receiver is the common case: a call made with no sink installed counts toward the
	// process totals only, which is correct — it is simply not attributable to one request.
	for cur := s; cur != nil; cur = cur.parent {
		cur.calls.Add(1)
		cur.in.Add(int64(inTok))
		cur.out.Add(int64(outTok))
		cur.cacheWrite.Add(int64(cacheWrite))
		cur.cacheRead.Add(int64(cacheRead))
	}
}

// SinkFrom returns the sink scoping ctx, or nil.
func SinkFrom(ctx context.Context) *Sink {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sinkKey{}).(*Sink)
	return s
}

// recordUsageCache adds one call's token usage to the process totals and to the calling
// scope's sink, split by cache tier. inTok is FRESH (uncached) input on both backends —
// see openai.go for why that needs normalizing there.
func recordUsageCache(ctx context.Context, inTok, outTok, cacheWrite, cacheRead int) {
	llmCalls.Add(1)
	llmInputTokens.Add(int64(inTok))
	llmOutputTokens.Add(int64(outTok))
	llmCacheWrite.Add(int64(cacheWrite))
	llmCacheRead.Add(int64(cacheRead))
	SinkFrom(ctx).add(inTok, outTok, cacheWrite, cacheRead)
}

// Usage returns the cumulative cheap-model usage (calls, input tokens, output
// tokens) since process start. Kept at this exact signature for backward
// compatibility — /stats' existing three fields are parsed by deploy/harbor/*.py.
func Usage() (calls, inTokens, outTokens int64) {
	return llmCalls.Load(), llmInputTokens.Load(), llmOutputTokens.Load()
}

// CacheUsage returns the cumulative cache-tier token counts (write, read) for the
// cheap model. read==0 after many calls means the preamble breakpoint is inert.
func CacheUsage() (cacheWrite, cacheRead int64) {
	return llmCacheWrite.Load(), llmCacheRead.Load()
}

// Pricing is the per-million-token price of the extraction model, in dollars. The
// economic gate needs the real cost of a call, not a hard-coded "$0.012" — that figure
// was one workload's average, and it changes with the model, the gateway's contract, and
// the prompt size. Rates come from the environment so an operator prices their own
// deployment without a rebuild; the defaults are claude-haiku-4-5 list rates.
type Pricing struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
}

// HaikuPricing is the default: claude-haiku-4-5 list rates ($1/$5 per MTok), with the
// standard Anthropic cache multipliers (write 1.25x input, read 0.1x input).
func HaikuPricing() Pricing {
	return Pricing{InputPerMTok: 1.00, OutputPerMTok: 5.00, CacheWritePerMTok: 1.25, CacheReadPerMTok: 0.10}
}

// PricingFromEnv returns HaikuPricing overridden by any of CHEAP_MODEL_PRICE_IN,
// _OUT, _CACHE_WRITE, _CACHE_READ (dollars per million tokens). An unparseable or
// absent value leaves the default — pricing must never fail a request.
func PricingFromEnv() Pricing {
	p := HaikuPricing()
	for _, f := range []struct {
		env string
		dst *float64
	}{
		{"CHEAP_MODEL_PRICE_IN", &p.InputPerMTok},
		{"CHEAP_MODEL_PRICE_OUT", &p.OutputPerMTok},
		{"CHEAP_MODEL_PRICE_CACHE_WRITE", &p.CacheWritePerMTok},
		{"CHEAP_MODEL_PRICE_CACHE_READ", &p.CacheReadPerMTok},
	} {
		if v, err := strconv.ParseFloat(os.Getenv(f.env), 64); err == nil && v >= 0 {
			*f.dst = v
		}
	}
	return p
}

// Cost prices one call's token usage in dollars.
func (p Pricing) Cost(inTok, outTok, cacheWrite, cacheRead int64) float64 {
	const perM = 1_000_000.0
	return (float64(inTok)*p.InputPerMTok +
		float64(outTok)*p.OutputPerMTok +
		float64(cacheWrite)*p.CacheWritePerMTok +
		float64(cacheRead)*p.CacheReadPerMTok) / perM
}

// AvgCallCost returns the OBSERVED mean dollar cost of one extraction call so far, and
// whether there is any observation yet. This is what the economic gate should spend
// against: it reflects this deployment's real prompt sizes, this model's real pricing,
// and whether the preamble cache is actually working — none of which a constant can.
// Callers must handle ok==false (no calls yet) with a prior estimate.
func AvgCallCost(p Pricing) (float64, bool) {
	calls := llmCalls.Load()
	if calls == 0 {
		return 0, false
	}
	total := p.Cost(llmInputTokens.Load(), llmOutputTokens.Load(),
		llmCacheWrite.Load(), llmCacheRead.Load())
	return total / float64(calls), true
}
