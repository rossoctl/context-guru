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
		if isSummaryHeader(text) && text != schema.MessageText(before[i]) {
			return true
		}
	}
	return false
}

// isSummaryHeader reports whether text is a summary message a summarizer spliced in: summarize's or
// cache_aware_summarizer's. THE ONE TEST for it in apply, used by the equal-count gate above and by
// every count-changing rebuild that has to find the summary (rebuildResponsesCountChanged). A gate
// that accepts a header a rebuild does not drops the splice silently (#727 review, round 2).
func isSummaryHeader(text string) bool {
	return strings.HasPrefix(text, schema.SummaryHeader) || strings.HasPrefix(text, schema.CacheAwareSummaryHeader)
}
