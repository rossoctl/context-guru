package kvcache

import (
	"context"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/modelinfo/ratefilltest"
	"strconv"
	"testing"
)

// Part of issue #856: one function decides a cache price. This test records what each caller
// priced BEFORE the change (fixture made on origin/main c2a06dd5) and fails if any value moves.

func TestRateFillFromPriceAndPriceList(t *testing.T) {
	prices := map[string]modelinfo.Price{
		"blank":      {Input: 3e-6, Output: 1.5e-5},
		"stated":     {Input: 3e-6, Output: 1.5e-5, CacheRead: 4e-7, CacheWrite: 4e-6},
		"readonly":   {Input: 3e-6, Output: 1.5e-5, CacheRead: 4e-7},
		"writeonly":  {Input: 3e-6, Output: 1.5e-5, CacheWrite: 4e-6},
		"noinput":    {Output: 1e-5, CacheWrite: 3e-6},
		"outputonly": {Output: 1e-5},
		"zero":       {},
	}
	mults := map[string]Multipliers{
		"default": {},
		"set":     {CacheRead: 0.2, Write5m: 1.5, Write1h: 3},
		"readset": {CacheRead: 0.07},
	}
	got := map[string]string{}
	for pn, p := range prices {
		for mn, m := range mults {
			for _, model := range ratefilltest.Models {
				r := FromPrice(model, p, m, "src")
				got["from/"+pn+"/"+mn+"/"+model] = strconv.FormatBool(r.Known) + " " +
					ratefilltest.F(r.Input, r.Output, r.CacheRead, r.Write5m, r.Write1h)
			}
		}
	}
	tb, err := modelinfo.ParseTable([]byte("models:\n  - {match: \"claude-opus*\", in: 5, out: 25}\n  - {match: \"gemini*\", in: 2, out: 12}\n  - {match: \"mythos*\", in: 8, out: 40, cache_read: 0.4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for mn, m := range mults {
		pl := NewPriceList(context.Background(), ratefilltest.Models, tb, m, nil)
		for _, r := range pl.Models {
			got["list/"+mn+"/"+r.Model] = strconv.FormatBool(r.Known) + " " +
				ratefilltest.F(r.Input, r.Output, r.CacheRead, r.Write5m, r.Write1h)
		}
		got["list/"+mn+"/mults"] = ratefilltest.F(pl.Multipliers.CacheRead, pl.Multipliers.Write5m, pl.Multipliers.Write1h)
	}
	ratefilltest.Check(t, got)
}
