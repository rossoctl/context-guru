package expand

import (
	"testing"

	"github.com/rossoctl/context-guru/schema"
)

// The restore path recognises a spliced summary by its first line, from both summarizers (#727 review).
func TestIsSummaryTextAcceptsBothHeaders(t *testing.T) {
	if !isSummaryText(schema.CacheAwareSummaryHeader+"\nbody") || !isSummaryText(schema.SummaryHeader+"\nbody") {
		t.Fatal("isSummaryText does not accept both summarizers' headers")
	}
	if isSummaryText("plain text") {
		t.Fatal("isSummaryText accepts plain text")
	}
}
