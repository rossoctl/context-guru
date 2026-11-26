package cheapmodel

import "testing"

func TestMinCacheablePrefixByFamily(t *testing.T) {
	for model, want := range map[string]int{
		"claude-sonnet-5": minCacheableSmall, "aws/claude-opus-5[1m]": minCacheableSmall,
		"claude-haiku-4-5": minCacheableHaiku, "claude-fable-5-1": minCacheableHaiku, "alias": minCacheableHaiku,
	} {
		if got := minCacheablePrefix(model); got != want {
			t.Errorf("minCacheablePrefix(%q) = %d, want %d", model, got, want)
		}
	}
}
