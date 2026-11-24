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

// PromptTokens is the prompt size that picks a price tier: fresh input plus cache reads plus cache
// writes. It is the ONE place that says what counts as prompt size. Output tokens do not count.
func PromptTokens(fresh, cacheRead, cacheWrite int64) int64 {
	return fresh + cacheRead + cacheWrite
}

// At returns the rates for a request whose prompt has promptTokens tokens. At or below a tier's
// line, that tier does not apply. Above it, the tier's rates apply to every token. With several
// tiers, the highest line below the prompt size wins. A price with no tiers returns itself.
// The result carries no tiers.
func (p Price) At(promptTokens int64) Price {
	if len(p.Tiers) == 0 {
		return p
	}
	var t *PriceTier
	for i := range p.Tiers {
		c := &p.Tiers[i]
		if promptTokens > c.AbovePromptTokens && (t == nil || c.AbovePromptTokens > t.AbovePromptTokens) {
			t = c
		}
	}
	q := p
	q.Tiers = nil
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

// BuiltinTiers returns the tiers this package knows for a model id, or nil. Other Claude models
// are flat across their window and have none.
func BuiltinTiers(model string) []PriceTier {
	full, _ := Normalize(model)
	if maj, min, ok := tierVersion(full, FamilyHaiku); ok && maj == 5 && min == 5 {
		return []PriceTier{haiku55LongPromptTier}
	}
	if GPT54OrLater(full) && !strings.Contains(full, "mini") && !strings.Contains(full, "nano") {
		return []PriceTier{openAILongPromptTier}
	}
	return nil
}

// WithTiers sets the built-in tiers for the model, unless the price already has tiers: a tier a
// source states wins over the built-in table. A source that bills a flat rate, such as an operator
// price file, must not call it.
func (p Price) WithTiers(model string) Price {
	if len(p.Tiers) == 0 {
		p.Tiers = BuiltinTiers(model)
	}
	return p
}

// GPT54OrLater reports whether an OpenAI GPT model is version 5.4 or later. Routed ids such as
// azure/gpt-5.5 count. The final "gpt-" token is the model the request selects.
func GPT54OrLater(model string) bool {
	model = strings.ToLower(model)
	i := strings.LastIndex(model, "gpt-")
	if i < 0 {
		return false
	}
	major, rest, ok := leadingInt(model[i+4:])
	if !ok {
		return false
	}
	if major != 5 {
		return major > 5
	}
	if rest == "" || rest[0] != '.' {
		return false
	}
	minor, _, ok := leadingInt(rest[1:])
	return ok && minor >= 4
}
