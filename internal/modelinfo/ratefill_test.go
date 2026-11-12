package modelinfo

import (
	"context"
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"strconv"
	"testing"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.
// The four table/*/claude-sonnet-5 rows were re-baselined by #334, which
// makes a bare id resolve to the qualified entry it is the tail of; every other row is unchanged.
//
// It also guards the FMA fix in Price.Cost and CostWithCacheWrite1h (#1128): without the explicit
// float64 rounding of each product, the price/1h rows fail on arm64 only. CI runs on amd64, where
// they pass either way, so run this test on an arm64 machine after a change to those functions.

func TestRateFillTableAndPrice(t *testing.T) {
	docs := map[string]string{
		"plain":     "",
		"readfrac":  "cache_read_frac: 0.2\n",
		"writefrac": "cache_write_frac: 1.5\n",
		"bothfracs": "cache_read_frac: 0.3\ncache_write_frac: 1.1\n",
	}
	const models = `models:
  - {match: "claude-opus*", in: 5.0, out: 25.0}
  - {match: "aws/claude-sonnet-5", in: 3.0, out: 15.0}
  - {match: "aws/claude-fable-5-1*", in: 10.0, out: 50.0, cache_read: 0.7}
  - {match: "claude-haiku*", in: 1.0, out: 5.0, cache_write: 1.4}
  - {match: "gemini*", in: 2.0, out: 12.0}
  - {match: "azure/gpt-5.5", in: 2.5, out: 15.0}
  - {match: "premium", in: 4.0, out: 20.0}
  - {match: "mythos*", in: 8.0, out: 40.0, cache_read: 0.4, cache_write: 9.5}
`
	got := map[string]string{}
	for name, head := range docs {
		tb, err := ParseTable([]byte(head + models))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ratefilltest.Models {
			p, ok := tb.Price(context.Background(), m)
			got["table/"+name+"/"+m] = strconv.FormatBool(ok) + " " +
				ratefilltest.F(p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CacheWrite1h())
		}
	}
	for _, in := range []float64{0, 1e-6, 3e-6, 5e-6} {
		p := Price{Input: in, CacheWrite: in * 1.3}
		got["price/1h/"+ratefilltest.F(in)] = ratefilltest.F(p.CacheWrite1h(),
			p.CostWithCacheWrite1h(10, 20, 30, 40, 50), p.CostWithCacheWrite1h(10, 20, 30, 40, 0))
	}
	ratefilltest.Check(t, got)
}
