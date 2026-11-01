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
// through Normalize first, so case, spaces, a provider prefix ("aws/", "vertex_ai/",
// "us.anthropic.") and a "[1m]" variant suffix never change the answer. The match is a substring of
// the full normalized id, as every old copy did.
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
