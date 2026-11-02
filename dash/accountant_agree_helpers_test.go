package dash_test

import (
	"math"

	"github.com/rossoctl/accountant/test/cases"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The helpers accountant_agree_test.go reuses. accountant_play_test.go holds same-named ones typed for the
// accountant module's cases, so these carry a CF suffix.

func priceOfCF(r cases.Rate) modelinfo.Price {
	return modelinfo.Price{
		Input:      float64(r.Input) / 1e12,
		CacheWrite: float64(r.CacheWrite5m) / 1e12,
		CacheRead:  float64(r.CacheRead) / 1e12,
		Output:     float64(r.Output) / 1e12,
	}
}

func microsCF(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func absCF(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// expectedCF is the case's expected figure for one step, in micro-dollars.
func expectedCF(c cases.Case, s cases.Step) (net int64, byChange map[string]int64) {
	byChange = map[string]int64{}
	rate := c.StepRate(s)
	for _, e := range s.Expected.Ops {
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		if e.Call != nil {
			picos -= c.ModelRate(e.Call.Model).CallPicos(e.Call.Usage)
		}
		byChange[e.ChangeID] += cases.RoundMicros(picos)
		net += picos
	}
	for _, e := range s.Expected.Effects { // breaks and rescues are cache effects, not operations
		if !e.Counted() {
			continue
		}
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		byChange[e.ChangeID] += cases.RoundMicros(picos)
		net += picos
	}
	return cases.RoundMicros(net), byChange
}

// diffCF is one disagreement. change is the ChangeID it is about, or "" when it is about a step or
// the thread.
type diffCF struct {
	step   int // -1 for the thread total
	change string
	text   string
}
