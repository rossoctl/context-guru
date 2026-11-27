package modelinfo

import "strings"

// PriceTier is the rate set a model bills when a request's prompt is larger than
// AbovePromptTokens. Some providers bill the WHOLE request at the higher rates above the line, not
// only the tokens past it.
//
// A rate is stated in one of two ways. An absolute rate (Input, Output, CacheRead, CacheWrite, per
// token) wins. When it is 0, the rate is the base rate times the multiple (InputMult, OutputMult,
// ReadMult, WriteMult). A multiple of 0 means 1. The write multiple applies to the 5-minute and the
// 1-hour write: the 1-hour rate is derived from Input, so it follows Input.
type PriceTier struct {
	AbovePromptTokens int64

	Input, Output, CacheRead, CacheWrite       float64
	InputMult, OutputMult, ReadMult, WriteMult float64
}

// MaxTiers is how many tiers a Price holds. The array keeps Price comparable with ==, which the
// packages that alias Price rely on. The live feed has at most 2 lines per model.
const MaxTiers = 3

// PromptTokens is the prompt size that picks a price tier: fresh input plus cache reads plus cache
// writes. It is the ONE place that says what counts as prompt size. Output tokens do not count.
func PromptTokens(fresh, cacheRead, cacheWrite int64) int64 {
	return fresh + cacheRead + cacheWrite
}

// TierBelow picks the tier that applies to a prompt of promptTokens tokens. lines holds each tier's
// AbovePromptTokens line, 0 for an unused entry. It returns the index of the highest line that the
// prompt is above. ok is false when the price is flat or the prompt is at or below every line. It is
// the ONE place that says which tier applies. Price.At and components.TokenRates call it.
func TierBelow(promptTokens int64, lines []int64) (idx int, ok bool) {
	var best int64
	for i, c := range lines {
		if c > 0 && promptTokens > c && (!ok || c > best) {
			idx, best, ok = i, c, true
		}
	}
	return idx, ok
}

// At returns the rates for a request whose prompt has promptTokens tokens. At or below a tier's
// line, that tier does not apply. Above it, the tier's rates apply to every token. With several
// tiers, the highest line below the prompt size wins. A price with no tiers returns itself.
// The result carries no tiers.
func (p Price) At(promptTokens int64) Price {
	var t *PriceTier
	var lines [MaxTiers]int64
	for i := range p.Tiers {
		lines[i] = p.Tiers[i].AbovePromptTokens
	}
	if i, ok := TierBelow(promptTokens, lines[:]); ok {
		t = &p.Tiers[i]
	}
	q := p
	q.Tiers = [MaxTiers]PriceTier{}
	if t == nil {
		return q
	}
	q.Input = tierRate(t.Input, p.Input, t.InputMult)
	q.Output = tierRate(t.Output, p.Output, t.OutputMult)
	q.CacheRead = tierRate(t.CacheRead, p.CacheRead, t.ReadMult)
	q.CacheWrite = tierRate(t.CacheWrite, p.CacheWrite, t.WriteMult)
	return q
}

// tierRate is a stated rate, else the base rate times the multiple (0 reads as 1).
func tierRate(stated, base, mult float64) float64 {
	if stated != 0 {
		return stated
	}
	if mult == 0 {
		return base
	}
	return base * mult
}

// Built-in tiers, as multiples of the base price so they work with any base price.
var (
	// OpenAI GPT-5.4 and later: above 272,000 prompt tokens every token bills at 2x input, 2x
	// cached read, 2x cache write and 1.5x output. https://developers.openai.com/api/docs/pricing
	openAILongPromptTier = PriceTier{AbovePromptTokens: 272_000, InputMult: 2, OutputMult: 1.5, ReadMult: 2, WriteMult: 2}
	// Claude Haiku 5.5: above 100,000 prompt tokens every rate is 5x.
	// https://platform.claude.com/docs/en/about-claude/pricing
	haiku55LongPromptTier = PriceTier{AbovePromptTokens: 100_000, InputMult: 5, OutputMult: 5, ReadMult: 5, WriteMult: 5}
)

// BuiltinTier returns the tier this package knows for a model id. Other Claude models are flat
// across their window and have none.
func BuiltinTier(model string) (PriceTier, bool) {
	full, _ := Normalize(model)
	if maj, min, ok := tierVersion(full, FamilyHaiku); ok && maj == 5 && min == 5 {
		return haiku55LongPromptTier, true
	}
	if GPT54OrLater(full) && !strings.Contains(full, "mini") && !strings.Contains(full, "nano") {
		return openAILongPromptTier, true
	}
	return PriceTier{}, false
}

// HasTiers reports whether the price has at least one tier.
func (p Price) HasTiers() bool {
	for _, t := range p.Tiers {
		if t.AbovePromptTokens > 0 {
			return true
		}
	}
	return false
}

// WithTiers sets the built-in tier for the model, unless the price already has tiers: a tier a
// source states wins over the built-in table. Both the public feed and the operator price file
// call it, so one model gets one answer whatever the config.
func (p Price) WithTiers(model string) Price {
	if !p.HasTiers() {
		if t, ok := BuiltinTier(model); ok {
			p.Tiers[0] = t
		}
	}
	return p
}

// GPT54OrLater reports whether an OpenAI GPT model is version 5.4 or later. Routed ids such as
// azure/gpt-5.5 count.
func GPT54OrLater(model string) bool { return gptAtLeast(model, 4) }
