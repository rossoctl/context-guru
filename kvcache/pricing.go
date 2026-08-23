package kvcache

import (
	"context"
	"sort"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Pricing is one model's rates for the SIX things a prompt-cache policy can spend money
// on, plus the two assumptions a keep-alive ping forces anyone to state out loud.
//
// # Why this is not modelinfo.Price
//
// modelinfo.Price is what the operator's price list and the public map publish: four tiers
// (input, output, cache read, cache creation). A TTL strategy needs two creation rates,
// because a 1-hour write is 2.0x base input where a 5-minute write is 1.25x, and pricing
// one as the other understates a 1h request by 0.75x of its whole written prefix. No
// gateway publishes a second creation rate, so Write1h is DERIVED from the documented
// multiplier — and the multiplier is a field, not a literal, so an operator whose provider
// charges differently can say so.
//
// # Nothing here is hardcoded in a UI component
//
// Every rate on this struct is served to the page and editable in it, and every edit comes
// back as an override that lands here. The page renders what it was given; it never carries
// a rate of its own. That is the same rule the keep-alive tab keeps, for the same reason:
// the browser has twice duplicated a table the server owns and drifted from it.
type Pricing struct {
	Model string `json:"model"`
	// Input is fresh, uncached input. Output is completion. CacheRead is a cache hit.
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
	// Write5m and Write1h are the two cache-CREATION rates.
	Write5m float64 `json:"write_5m"`
	Write1h float64 `json:"write_1h"`

	// PingInputTokens and PingOutputTokens are the request overhead a keep-alive ping
	// cannot avoid: the fresh input it must carry past the cached prefix, and the smallest
	// generation the provider will accept.
	//
	// A ping's job is to touch the cached prefix and generate nothing. Where a provider
	// accepts a zero-generation request that is free; where it does not — Anthropic's
	// Messages API requires max_tokens >= 1 — the smallest supported request still bills
	// one output token, and the assumption is EXPOSED rather than rounded to zero. See
	// Semantics.ZeroGeneration.
	PingInputTokens  int64 `json:"ping_input_tokens"`
	PingOutputTokens int64 `json:"ping_output_tokens"`

	// Source names where these rates came from, so nobody reads a typed number as a
	// configured one: SourcePriceList (the deployment's own price list answered),
	// "override" (edited in the page), or "" when the model has no rates at all.
	Source string `json:"source"`
	// Known is false when the model has NO rates. Then every dollar figure derived from it
	// is absent rather than zero — an unpriced model is not a free one, and this project has
	// shipped that particular lie five times.
	Known bool `json:"known"`
}

// The documented Anthropic-family multiples, as named defaults rather than literals spread
// across a cost function. Overridable per deployment through Multipliers.
const (
	// DefaultCacheReadMultiple is a cache hit's rate as a multiple of base input.
	DefaultCacheReadMultiple = 0.1
	// DefaultWrite5mMultiple is a 5-minute cache write as a multiple of base input.
	DefaultWrite5mMultiple = 1.25
	// DefaultWrite1hMultiple is a 1-hour cache write as a multiple of base input.
	DefaultWrite1hMultiple = 2.0
	// DefaultPingOutputTokens is the smallest generation Anthropic's Messages API accepts.
	DefaultPingOutputTokens = 1
	// DefaultPingInputTokens is the fresh input a ping carries beyond the cached prefix —
	// the one-token user turn that makes it a legal request.
	DefaultPingInputTokens = 1
)

// Multipliers are the deployment-wide fractions used to fill in a rate a price list does
// not state. Zero means "use the default", so a partially filled struct is safe.
type Multipliers struct {
	CacheRead float64 `json:"cache_read"`
	Write5m   float64 `json:"write_5m"`
	Write1h   float64 `json:"write_1h"`
}

// WithDefaults returns the multipliers with every zero replaced by its default. Exported
// because the dashboard reports the multiples a simulation actually used, and reporting the
// caller's partially filled struct would show zeros where the defaults were applied.
func (m Multipliers) WithDefaults() Multipliers {
	if m.CacheRead <= 0 {
		m.CacheRead = DefaultCacheReadMultiple
	}
	if m.Write5m <= 0 {
		m.Write5m = DefaultWrite5mMultiple
	}
	if m.Write1h <= 0 {
		m.Write1h = DefaultWrite1hMultiple
	}
	return m
}

// DefaultMultipliers is the shipped set.
func DefaultMultipliers() Multipliers { return Multipliers{}.WithDefaults() }

// FromPrice derives a model's six rates from whatever the deployment's Pricer published.
//
// Only Write1h is ever invented, and only from the multiplier: it is the one rate no
// gateway publishes. The rest are passed through untouched, so an operator price list that
// disagrees with the public map wins — which is the whole reason internal/modelinfo.Table
// exists.
//
// A Price with no rates at all yields Known=false, and every dollar figure downstream is
// then omitted rather than computed as zero.
func FromPrice(model string, p modelinfo.Price, m Multipliers, source string) Pricing {
	m = m.WithDefaults()
	out := Pricing{
		Model: model, Input: p.Input, Output: p.Output, CacheRead: p.CacheRead,
		Write5m: p.CacheWrite, Source: source,
		PingInputTokens: DefaultPingInputTokens, PingOutputTokens: DefaultPingOutputTokens,
	}
	if p.Zero() {
		out.Source = ""
		return out
	}
	out.Known = true
	// A price list that omitted a cache tier: fill from the multiplier rather than leave a
	// cached request priced as free.
	if out.CacheRead == 0 {
		out.CacheRead = out.Input * m.CacheRead
	}
	if out.Write5m == 0 {
		out.Write5m = out.Input * m.Write5m
	}
	// The 1-hour rate. Derived from base input where that is known — the multiplier is
	// against INPUT, not against the 5m rate, because that is how the provider documents it.
	// Where input is unknown but a 5m rate is not, scale the 5m rate by the ratio of the two
	// multipliers, which is the same number by a longer route.
	switch {
	case out.Input > 0:
		out.Write1h = out.Input * m.Write1h
	case out.Write5m > 0 && m.Write5m > 0:
		out.Write1h = out.Write5m * (m.Write1h / m.Write5m)
	}
	// A 1-hour entry can never be cheaper to create than a 5-minute one. A price list that
	// says otherwise is a typo, and honouring it would make every 1h strategy look free.
	if out.Write1h < out.Write5m {
		out.Write1h = out.Write5m
	}
	return out
}

// Override is one model's edited rates, as the page sends them back. Every field is a
// POINTER so that "not edited" and "edited to zero" are different facts: a rate of zero is
// a legitimate thing to experiment with, and a sentinel could not say so.
type Override struct {
	Input            *float64 `json:"input,omitempty"`
	Output           *float64 `json:"output,omitempty"`
	CacheRead        *float64 `json:"cache_read,omitempty"`
	Write5m          *float64 `json:"write_5m,omitempty"`
	Write1h          *float64 `json:"write_1h,omitempty"`
	PingInputTokens  *int64   `json:"ping_input_tokens,omitempty"`
	PingOutputTokens *int64   `json:"ping_output_tokens,omitempty"`
}

// Apply lays an override over a derived Pricing. An override makes the row KNOWN: a model
// the price list has never heard of is exactly the case an operator needs to be able to
// price by hand.
func (o Override) Apply(p Pricing) Pricing {
	set := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&p.Input, o.Input)
	set(&p.Output, o.Output)
	set(&p.CacheRead, o.CacheRead)
	set(&p.Write5m, o.Write5m)
	set(&p.Write1h, o.Write1h)
	if o.PingInputTokens != nil {
		p.PingInputTokens = *o.PingInputTokens
	}
	if o.PingOutputTokens != nil {
		p.PingOutputTokens = *o.PingOutputTokens
	}
	if o.Empty() {
		return p
	}
	p.Source = "override"
	p.Known = p.Input != 0 || p.Output != 0 || p.CacheRead != 0 || p.Write5m != 0 || p.Write1h != 0
	return p
}

// Empty reports whether this override changes nothing.
func (o Override) Empty() bool {
	return o.Input == nil && o.Output == nil && o.CacheRead == nil && o.Write5m == nil &&
		o.Write1h == nil && o.PingInputTokens == nil && o.PingOutputTokens == nil
}

// PriceList is the whole priced surface of one analysis: a row per model in the window,
// plus the multipliers the derived rates came from. This is what the page renders in its
// pricing panel and what it posts back edited.
type PriceList struct {
	Multipliers Multipliers `json:"multipliers"`
	Models      []Pricing   `json:"models"`

	byModel map[string]Pricing
}

// OverrideAll is the overrides key that applies to EVERY model before its own entry.
//
// It exists for the two fields that are provider facts rather than model rates — the ping's
// minimum input and output — which a caller would otherwise have to repeat once per model in
// the window, and would then get wrong for the thirteenth.
const OverrideAll = "*"

// NewPriceList builds the list for a set of models from a Pricer, then applies overrides.
//
// models is used as given (deduped and sorted) so a model with no rates still gets a ROW —
// saying "this model has no rates" is the answer, and omitting it would leave the reader
// unable to tell an unpriced model from one that has no traffic.
//
// overrides[OverrideAll] is applied to every row first, then the row's own entry, so a
// per-model rate always wins over a blanket one.
func NewPriceList(ctx context.Context, models []string, p modelinfo.Pricer, m Multipliers,
	overrides map[string]Override) *PriceList {
	m = m.WithDefaults()
	out := &PriceList{Multipliers: m, Models: []Pricing{}, byModel: map[string]Pricing{}}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(models))
	for _, name := range models {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		uniq = append(uniq, name)
	}
	sort.Strings(uniq)
	for _, name := range uniq {
		var pr modelinfo.Price
		source := ""
		if p != nil {
			if got, ok := p.Price(ctx, name); ok {
				pr, source = got, SourcePriceList
			}
		}
		row := FromPrice(name, pr, m, source)
		if ov, ok := overrides[OverrideAll]; ok {
			row = ov.Apply(row)
		}
		if ov, ok := overrides[name]; ok {
			row = ov.Apply(row)
		}
		out.Models = append(out.Models, row)
		out.byModel[name] = row
	}
	return out
}

// SourcePriceList is what a rate the deployment's own Pricer answered is labelled.
//
// It deliberately does not name WHICH pricer: the chain is assembled by the proxy
// (operator price list first, then the public LiteLLM map) and this layer is handed the
// composed Pricer, so claiming "operator_table" here would be a guess. The label says the
// rate came from the configured price list; "override" says a person typed it.
const SourcePriceList = "price_list"

// For returns one model's rates. A model the list has never seen comes back Known=false
// with its own name on it, so the caller can report WHICH model it could not price.
func (l *PriceList) For(model string) Pricing {
	if l == nil {
		return Pricing{Model: model}
	}
	if p, ok := l.byModel[model]; ok {
		return p
	}
	return Pricing{Model: model, PingInputTokens: DefaultPingInputTokens,
		PingOutputTokens: DefaultPingOutputTokens}
}

// Semantics is the provider's cache behaviour, made explicit and configurable because it
// is not the same everywhere and because getting it wrong silently changes every result.
//
// Anthropic's documented behaviour is the shipped default:
//
//   - "the cache is refreshed for no additional cost each time the cached content is
//     used" — so a plain cache HIT resets the entry's lifetime. HitRefreshesTTL=true.
//   - a keep-alive read is just a use, so it refreshes the entry for its OWN tier's
//     lifetime. PingRefreshesTTL=true.
//   - refreshing does not change the tier an entry was created at, so a refresh buys another
//     lifetime OF THAT TIER — five minutes for a 5-minute entry, an hour for a one-hour one.
//   - max_tokens must be >= 1, so a ping cannot generate nothing. ZeroGeneration=false.
type Semantics struct {
	HitRefreshesTTL  bool `json:"hit_refreshes_ttl"`
	PingRefreshesTTL bool `json:"ping_refreshes_ttl"`
	ZeroGeneration   bool `json:"zero_generation"`
}

// DefaultSemantics is Anthropic's documented behaviour.
func DefaultSemantics() Semantics {
	return Semantics{HitRefreshesTTL: true, PingRefreshesTTL: true}
}

// pingOutput is the output tokens one ping is billed for under these semantics.
func (s Semantics) pingOutput(p Pricing) int64 {
	if s.ZeroGeneration {
		return 0
	}
	if p.PingOutputTokens < 0 {
		return 0
	}
	return p.PingOutputTokens
}

// RequestCost prices one request's four billed tiers at a chosen write tier.
//
//	request_cost = input×input_rate + read×cache_read_rate + write×write_rate(tier) + output×output_rate
//
// The write rate is the tier's, never a blend: that is the whole reason Pricing carries two.
func (p Pricing) RequestCost(input, read, write, output int64, tier TTL) float64 {
	return float64(input)*p.Input + float64(read)*p.CacheRead +
		float64(write)*p.writeRate(tier) + float64(output)*p.Output
}

// writeRate is the creation rate for one tier. TTLNone has no write — a request carrying no
// cache_control creates nothing — so it returns 0 and the caller must have billed those
// tokens as fresh input instead.
func (p Pricing) writeRate(tier TTL) float64 {
	switch tier {
	case TTL5m:
		return p.Write5m
	case TTL1h:
		return p.Write1h
	}
	return 0
}

// KeepAliveCost is what ONE keep-alive costs on a prefix of `cached` tokens.
//
//	keep_alive_cost = cached×cache_read_rate + ping_input×input_rate + ping_output×output_rate
//
// It is the same arithmetic whether the entry is held at five minutes or at an hour — a
// refresh is a cache read at 0.1x either way — and that is worth saying out loud, because
// the obvious guess is that a "1-hour ping" costs more. The difference between a 5-minute
// and a 1-hour keep-alive is not the price of one ping. It is (a) the CREATION tier that had
// to be paid to put the entry there, 1.25x against 2.0x of base input, and (b) how OFTEN a
// ping is needed: a five-minute entry has to be touched roughly twelve times as often as a
// one-hour one to be held for the same wall-clock span. Charging two different per-ping rates
// would be inventing a price no provider publishes.
func (p Pricing) KeepAliveCost(cached int64, s Semantics) float64 {
	return float64(cached)*p.CacheRead + float64(p.PingInputTokens)*p.Input +
		float64(s.pingOutput(p))*p.Output
}

// RecreateCost is what a keep-alive costs when it arrives TOO LATE: the entry has already
// lapsed, so the "refresh" is a cache write, at 12.5x a read for the 5-minute tier and 20x
// for the one-hour tier.
//
//	recreate_cost = cached×write_rate(tier) + ping_input×input_rate + ping_output×output_rate
//
// This is the pathology of a schedule whose interval exceeds the lifetime it is protecting,
// and pricing it as a read is the arithmetic error that would hide it completely.
func (p Pricing) RecreateCost(cached int64, tier TTL, s Semantics) float64 {
	return float64(cached)*p.writeRate(tier) + float64(p.PingInputTokens)*p.Input +
		float64(s.pingOutput(p))*p.Output
}

// HoldCost is what it costs to hold a prefix of `cached` tokens at `tier` from nothing: the
// creation of the entry, plus `pings` keep-alives to carry it further.
//
// This is the figure the pricing panel shows beside each configured rate — "what would
// another five minutes of this cost me, and what would another hour" — so the reader sees a
// dollar amount on their own median prefix rather than a per-token rate to multiply in their
// head. pings=0 is the plain write.
func (p Pricing) HoldCost(cached int64, tier TTL, pings int, s Semantics) float64 {
	return float64(cached)*p.writeRate(tier) + float64(pings)*p.KeepAliveCost(cached, s)
}

// UncachedCost is the same prompt with no prompt cache at all: every prefix token billed
// as fresh input. The denominator of the "incremental cache premium", and the No-cache
// baseline's own arithmetic.
//
//	uncached_cost = (input + prefix)×input_rate + output×output_rate
func (p Pricing) UncachedCost(input, prefix, output int64) float64 {
	return float64(input+prefix)*p.Input + float64(output)*p.Output
}
