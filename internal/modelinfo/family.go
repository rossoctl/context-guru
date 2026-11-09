package modelinfo

import "strings"

// The Claude tiers Family can name.
const (
	FamilyOpus   = "opus"
	FamilySonnet = "sonnet"
	FamilyHaiku  = "haiku"
	FamilyFable  = "fable"
)

// Family names the Claude tier in a model id: FamilyOpus, FamilySonnet, FamilyHaiku or
// FamilyFable. It returns "" for any other id, including a gateway alias that hides the model.
//
// One rule, because copies drifted: none of the earlier per-package copies knew "fable". The id goes
// through Normalize first, so case, spaces and a "[1m]" variant suffix never change the answer.
// Normalize does not strip a provider prefix from the full id. It does not need to: the match is a
// substring of the full id ("aws/", "vertex_ai/" and "us.anthropic." prefixes never hold a tier
// word), as every old copy did.
//
// The tier order here is opus, sonnet, haiku, fable. The old billed-factor copy tested haiku first.
// No real id holds two tier words, so the result is the same.
//
// Family says WHICH tier. Each caller still decides what a tier or "" means for it. A tier can have
// no measured value in a caller's table; that caller keeps its own default.
func Family(model string) string {
	full, _ := Normalize(model)
	for _, f := range [...]string{FamilyOpus, FamilySonnet, FamilyHaiku, FamilyFable} {
		if strings.Contains(full, f) {
			return f
		}
	}
	return ""
}

// IsClaude reports whether the id names a Claude or Anthropic model by those words. It is
// narrower than Family != "": a bare tier word such as "opus" does not count.
func IsClaude(model string) bool {
	full, _ := Normalize(model)
	return strings.Contains(full, "claude") || strings.Contains(full, "anthropic")
}
