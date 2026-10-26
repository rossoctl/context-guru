package schema

// The first line of the message a summarizer splices in place of the turns it replaces. apply reads
// them to recognise a splice that kept the message count (a one-message span), which must still go
// through the count-changing rebuild (#727 review). One place for both, so a builder and the check
// cannot drift apart.
const (
	SummaryHeader           = "=== History Summary ==="                         // summarize
	CacheAwareSummaryHeader = "=== Compacted Context (cache-aware summary) ===" // cache_aware_summarizer
)
