package apply

import (
	"log/slog"
	"strconv"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Mixed-TTL: a one-hour anchor on the request's HEAD, the five-minute default on its tail.
//
// The provider offers two cache lifetimes, and the naive reading of that — ask for 1h
// everywhere, stop losing prefixes to idle gaps — loses money. The premium is levied on
// every request that CREATES an entry while the benefit lands only on the ones that would
// otherwise have missed, and on this service's traffic those populations differ by fifty
// times: measured over 19,805 production requests, a blanket 1h costs an extra $773.00 on
// 14,499 cache-creating requests to recover $754.66 on 290, net **−$18.34**.
//
// Mixed TTL fixes the arithmetic rather than the probability. Billing splits at three
// positions — a read for the highest cache hit, a 1h write for the segment between the 1h
// and 5m breakpoints, a 5m write for the rest — so labelling ONLY the head 1h pays the 2.0x
// premium on the head alone. The premium then lands on head-write events (769 in the
// window) instead of on every writer, and both sides of the ledger scale linearly in the
// head's share f of what gets written, which fixes the ratio at 1.79:1 and means the sign
// never flips: net +$20.19 at f=0.10, +$60.56 at f=0.30.
//
// Three constraints make this the design that fits, and each is load-bearing:
//
//   - **It adds no breakpoint.** The cap is four per request and production's modal layout
//     already uses three (two `system`, one trailing content block, 60.6% of requests and
//     88.9% of spend); 16.1% are already AT four, where adding one is a 400 rather than a
//     cost regression. Re-labelling an existing mark's `ttl` spends no slot, which is why
//     this is reachable where a genuinely new split is not.
//   - **1h entries must appear BEFORE 5m ones.** The head does, by construction: the
//     provider hashes `tools` → `system` → `messages`, so anything in the first two arrays
//     precedes every message breakpoint. This function only ever writes 1h into the head,
//     so it cannot produce the illegal order.
//   - **The 1h tier must actually arrive, and on the models that carry this service's spend it
//     does not.** Measured live on this gateway, one request each with the head labelled 1h:
//     `aws/claude-haiku-4-5` came back with `ephemeral_1h_input_tokens: 36,251` of 36,574
//     written — GRANTED; `aws/claude-sonnet-5` came back with `ephemeral_1h_input_tokens: 0`
//     and all 48,212 tokens on the 5-minute tier — silently downgraded, with an otherwise
//     normal 200. So the `ttl` field is NOT stripped in transit (Haiku honouring it proves it
//     arrives); Bedrock's 1h support simply covers the Claude 4.5 family, and this service's
//     spend is Opus 5, Opus 4.8, Sonnet 5 and Opus 4.6. Zero 1h writes appear in 19,805
//     production requests, which is the same fact seen from the billing side.
//
//     Hence: implemented, verifiable, and off. `Usage.CacheWrite1h` is what says whether a
//     request that asked for 1h got it, and while that is zero on a model the honest
//     projection for this policy on that model is $0.
//
// Fails open: any parse trouble returns the input untouched.

// headTTLPaths are the arrays whose breakpoints sit in the hashed prefix ahead of every
// message — the head. `tools` and `system` only: a mark inside `messages` is the tail, and
// promoting it to 1h is both the losing blanket policy and the illegal ordering.
var headTTLPaths = [...]string{"tools", "system"}

// upgradeHeadTTL re-labels the head's existing cache breakpoints as one-hour entries and
// reports the token size of the prefix they cover.
//
// headTokens is the numerator of the head share f, and it is measured here because this is
// the only place that knows which marks were promoted. It is the honest size of the 1h
// entry: the tokens of `tools` plus the `system` blocks up to and including the LAST
// promoted mark. Everything after it stays on the 5m tier and is not part of the premium.
//
// minTokens gates it on the request's own size. A dollar filter, not a probability filter:
// gating this on the best available predictor of a long idle gap leaves the net unchanged
// (−$18.32 against −$18.34 blanket), because premium and benefit scale with the same
// `cache_write` on the same requests and multiplying both by a probability cannot change the
// sign. Gating on SIZE does change it (+$48.81 measured), because it excludes the
// small-prefix requests that pay a premium and can never produce a large miss.
//
// ponytail: the gate is evaluated per request, so a session growing across the threshold starts
// asking for 1h mid-conversation and a compaction can take it back — the head is then re-labelled
// and, because this never STRIPS an existing ttl, a request can carry 1h while its neighbours do
// not. Harmless while this is off and while the provider downgrades it anyway (a differing ttl
// does not change the prefix hash, so neither direction costs a cache miss); if it is ever
// switched on for real, latch the decision per session instead of recomputing it.
//
// The size is estimated as len(body)/4 rather than counted. This runs before any byte offset
// into the body is taken and therefore before anything has been tokenized, and a BPE pass
// over a whole request to decide a coarse 50,000-token threshold would cost more than the
// policy earns. Same estimate, same reason, as splitVolatileTail's own floor.
func upgradeHeadTTL(body []byte, provider bschemas.ModelProvider, minTokens int) (out []byte, upgraded bool, headTokens int) {
	if !explicitBreakpointProvider(provider) {
		return body, false, 0 // no explicit breakpoints, so no TTL to state
	}
	if len(body)/4 < minTokens {
		return body, false, 0
	}
	next := body
	for _, arr := range headTTLPaths {
		res := gjson.GetBytes(next, arr)
		if !res.Exists() || !res.IsArray() {
			continue
		}
		elems := res.Array()
		for i := range elems {
			// Only an EXISTING mark is promoted. Writing a `cache_control` where there was
			// none would add a breakpoint, and on the 16.1% of requests already at the cap
			// of four that is a 400 from the provider rather than a worse price.
			cc := elems[i].Get("cache_control")
			if !cc.IsObject() {
				continue
			}
			if cc.Get("ttl").String() == "1h" {
				// Already 1h — count it as head all the same, so the reported size matches
				// what the provider will bill at the 1h tier.
				upgraded = true
				continue
			}
			set, err := sjson.SetBytes(next, arr+"."+strconv.Itoa(i)+".cache_control.ttl", "1h")
			if err != nil {
				return body, false, 0 // fail open, whole-body: a half-labelled head is worse than none
			}
			next, upgraded = set, true
		}
	}
	if !upgraded {
		return body, false, 0
	}
	headTokens = headPrefixTokens(next)
	slog.Debug("context-guru: asked for the 1h cache tier on the head breakpoints",
		"provider", provider, "head_tokens", headTokens, "body_bytes", len(body))
	return next, true, headTokens
}

// headPrefixTokens counts the prefix the head's 1h entry covers: all of `tools`, plus the
// `system` blocks up to and including the last one carrying a breakpoint.
//
// Counted over the JSON of the tool declarations and the TEXT of the system blocks, which
// is what the provider hashes and bills. It is an estimate in the same sense
// splitVolatileTail's stableTokens is — BPE over our own reconstruction rather than the
// provider's block-granular accounting — so it is used for measuring f and never for
// pricing a request. The billed figure comes from `usage`.
//
// Only called when the upgrade actually fires, which is off by default and gated on size:
// a BPE pass over a ~48k-token head is not something to spend on every request.
func headPrefixTokens(body []byte) int {
	n := 0
	gjson.GetBytes(body, "tools").ForEach(func(_, v gjson.Result) bool {
		n += tokens.Count(v.Raw)
		return true
	})
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return n
	}
	if !sys.IsArray() {
		return n + tokens.Count(sys.String()) // a string system prompt is the whole head
	}
	blocks := sys.Array()
	last := -1
	for i := range blocks {
		if blocks[i].Get("cache_control").IsObject() {
			last = i
		}
	}
	for i := 0; i <= last && i < len(blocks); i++ {
		n += tokens.Count(blocks[i].Get("text").String())
	}
	return n
}
