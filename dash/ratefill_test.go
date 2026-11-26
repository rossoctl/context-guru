package dash

import (
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"testing"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.

func TestRateFillMedianPingUSD(t *testing.T) {
	got := map[string]string{}
	for _, model := range ratefilltest.Models {
		for _, cost := range []float64{1.25, 0.4} {
			evs := kaExpiry(2_000_000, "s1", 700, cost, 300_000)
			for _, e := range evs {
				e.Model = model
			}
			v, err := newKAFixture(t, evs...).db.medianPingUSD(Filter{TenantAll: true})
			if err != nil {
				t.Fatal(err)
			}
			got["ping/"+ratefilltest.F(cost)+"/"+model] = ratefilltest.F(v)
		}
	}
	ratefilltest.Check(t, got)
}
