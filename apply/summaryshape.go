package apply

import (
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// A one-message span replaced by a summarizer keeps the same count but changes
// the wire structure. Both adapters must rebuild instead of editing only text.
// Both summarizers' headers count: cache_aware_summarizer's splice must reach the
// rebuild too, which is where its cache anchor is written (#723, #727 review).
func summaryStructureChanged(before, after []bschemas.ChatMessage) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range after {
		text := schema.MessageText(after[i])
		if (strings.HasPrefix(text, schema.SummaryHeader) || strings.HasPrefix(text, schema.CacheAwareSummaryHeader)) &&
			text != schema.MessageText(before[i]) {
			return true
		}
	}
	return false
}
