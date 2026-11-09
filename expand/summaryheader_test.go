package expand

import (
	"testing"

	"github.com/rossoctl/context-guru/schema"
)

// The restore path recognises a spliced summary by its first line; those must stay the headers the
// summarizers actually write (schema), or a summary is never restored in place (#727 review).
func TestSummaryHeadersMatchSchema(t *testing.T) {
	if summaryHeader != schema.SummaryHeader || cacheAwareSummaryHeader != schema.CacheAwareSummaryHeader {
		t.Fatalf("expand's summary headers drifted from schema's: %q / %q vs %q / %q",
			summaryHeader, cacheAwareSummaryHeader, schema.SummaryHeader, schema.CacheAwareSummaryHeader)
	}
	if !isSummaryText(schema.CacheAwareSummaryHeader+"\nbody") || !isSummaryText(schema.SummaryHeader+"\nbody") {
		t.Fatal("isSummaryText does not accept both summarizers' headers")
	}
}
