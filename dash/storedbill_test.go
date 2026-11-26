package dash

import (
	"math/rand"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/modelinfo/modelinfotest"
)

// The stored bill (Event.CostUSD) comes from the accountant now. It must give the exact bits of the formula
// that stored it before: the price at the tier of the prompt size, then modelinfo.Price.StoredBill
// (each product rounded on its own, every write at the 5-minute rate, the 1-hour premium after).
func TestStoredBillIsTheSameBitsAsTheOldFormula(t *testing.T) {
	prices := map[string]modelinfo.Price{
		"claude-sonnet-5":  {Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6},
		"claude-opus-5-5":  {Input: 5e-6, Output: 25e-6, CacheRead: 0.5e-6, CacheWrite: 6.25e-6},
		"claude-haiku-5":   {Input: 1.1e-6, Output: 5.5e-6, CacheRead: 0.11e-6, CacheWrite: 1.375e-6},
		"gpt-5.6":          modelinfo.Price{Input: 5e-6, CacheRead: 0.5e-6, CacheWrite: 5e-6, Output: 30e-6}.WithTiers("gpt-5.6"),
		"claude-haiku-5-5": modelinfo.Price{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6, Output: 5e-6}.WithTiers("claude-haiku-5-5"),
		// A price that names only its input rate: every other rate is the model's own multiple.
		"claude-sonnet-4-6": modelinfo.Price{Input: 3e-6, Output: 15e-6}.Resolved("claude-sonnet-4-6"),
	}
	rng := rand.New(rand.NewSource(7))
	fixed := [][5]int64{ // fresh, read, write, output, write1h
		{100, 8000, 1200, 50, 0}, {7, 271_993, 0, 1, 0}, {10, 120_000, 5000, 200, 5000}, {10, 20_000, 5000, 200, 3000},
		{1, 1, 1, 1, 1}, {0, 0, 0, 0, 0}, {123_457, 98_765, 54_321, 4_567, 20_000},
	}
	for model, p := range prices {
		n := 0
		check := func(c [5]int64) {
			e := &Event{Model: model, FreshInput: c[0], CacheRead: c[1], CacheWrite: c[2], OutputTokens: c[3], CacheWrite1h: c[4]}
			e.Price(p, true)
			size := modelinfo.PromptTokens(c[0], c[1], c[2])
			want := modelinfotest.StoredBill(p.At(size), c[0], c[1], c[2], c[3], c[4])
			if e.CostUSD != want {
				t.Errorf("%s %v: stored bill %v, old formula %v (diff %g)", model, c, e.CostUSD, want, e.CostUSD-want)
			}
			n++
		}
		for _, c := range fixed {
			check(c)
		}
		for i := 0; i < 3000; i++ {
			w := rng.Int63n(60_000)
			check([5]int64{rng.Int63n(2000), rng.Int63n(400_000), w, rng.Int63n(8000), rng.Int63n(w + 1)})
		}
		if n == 0 {
			t.Fatal("no rows checked")
		}
	}
}
