package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// BenchmarkAccumulateDeclFilterRow prices one stored row: the work per row of DeclFilterSavings.
func BenchmarkAccumulateDeclFilterRow(b *testing.B) {
	price := func(string) (modelinfo.Price, bool) {
		p := modelinfo.Price{Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}.WithTiers("opus")
		return p, true
	}
	var out DeclFilterSaving
	sessions := map[string]bool{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		accumulateDeclFilterRow(&out, sessions, "s", "opus", 1000, 9_000, 90_000, 0, 0, 1000, false, price)
	}
}
