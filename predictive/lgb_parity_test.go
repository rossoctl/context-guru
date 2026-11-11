package predictive

import (
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"sort"
	"testing"
)

type lgbEvent struct {
	ID         int64      `json:"id"`
	Tenant     int        `json:"tenant"`
	Session    int        `json:"session"`
	Thread     int        `json:"thread"`
	Model      string     `json:"model"`
	Ts         int64      `json:"ts"`
	End        int64      `json:"end"`
	Dec        int64      `json:"dec"`
	Prefix     int64      `json:"prefix"`
	CacheWrite int64      `json:"cache_write"`
	Fresh      int64      `json:"fresh"`
	Out        int64      `json:"out"`
	Messages   int        `json:"messages"`
	Tools      int        `json:"tools"`
	SysBlocks  int        `json:"system_blocks"`
	MaxTokens  int        `json:"max_tokens"`
	Stop       string     `json:"stop"`
	Agent      string     `json:"agent"`
	Provider   string     `json:"provider"`
	Thinking   string     `json:"thinking"`
	Effort     string     `json:"effort"`
	CacheTTL   string     `json:"cache_ttl"`
	Stream     bool       `json:"stream"`
	TTFBMs     float64    `json:"ttfb_ms"`
	UpstreamMs float64    `json:"upstream_ms"`
	Cost       float64    `json:"cost"`
	PIn        float64    `json:"p_in"`
	POut       float64    `json:"p_out"`
	PRead      float64    `json:"p_read"`
	PWrite5m   float64    `json:"p_write5m"`
	Feat       []*float64 `json:"feat"`
	Cats       []string   `json:"cats"`
	Probs      []float64  `json:"probs"`
	K          int        `json:"k"`
	KHi        int        `json:"k_hi"`
	Check      bool       `json:"check"`
}

type lgbFixture struct {
	Times   []float64  `json:"times"`
	HiMult  float64    `json:"hi_mult"`
	Numeric []string   `json:"numeric"`
	CatOrd  []string   `json:"cat_order"`
	Events  []lgbEvent `json:"events"`
}

func loadLGBFixture(t testing.TB) (*lgbFixture, Model) {
	f, err := os.Open("testdata/lgb.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx lgbFixture
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModel("testdata/lgb.model.json")
	if err != nil {
		t.Fatal(err)
	}
	return &fx, m
}

func (e *lgbEvent) response() *Response {
	return &Response{Request: Request{Tenant: string(rune('A' + e.Tenant)), Session: "s" + string(rune('a'+e.Session%26)) + string(rune('A'+e.Session/26)), Thread: "t" + string(rune('a'+e.Thread%26)) + string(rune('A'+e.Thread/26)), Model: e.Model, TsMs: e.Ts},
		EndMs: e.End, DecMs: e.Dec, Provider: e.Provider, CacheTTL: e.CacheTTL, Effort: e.Effort, MaxTokens: e.MaxTokens, SystemBlocks: e.SysBlocks, TTFBMs: e.TTFBMs, UpstreamMs: e.UpstreamMs, CostUSD: e.Cost,
		Prefix: e.Prefix, CacheWrite: e.CacheWrite, Fresh: e.Fresh, Output: e.Out, Messages: e.Messages, Tools: e.Tools, StopReason: e.Stop, Agent: e.Agent, Thinking: e.Thinking, Stream: e.Stream,
		PIn: e.PIn, POut: e.POut, PRead: e.PRead, PWrite5m: e.PWrite5m}
}

func (e *lgbEvent) econ(mult float64) Econ {
	return Econ{PrefixTok: float64(e.Prefix), PWrite5m: e.PWrite5m, PRead: e.PRead, PingCost: mult * (float64(e.Prefix)*e.PRead + 7*e.PIn + e.POut)}
}

// Given the SAME 72 inputs, the Go tree evaluator reproduces LightGBM's survival curve to 1e-9 and the decisions exactly (two cost regimes).
func TestLGBModelParityWithLightGBM(t *testing.T) {
	fx, m := loadLGBFixture(t)
	var maxd float64
	hist := map[int]int{}
	for i := range fx.Events {
		e := &fx.Events[i]
		row := NewRow()
		fillFromFixture(row, fx, e)
		got := make([]float64, len(fx.Times))
		m.Survival(row, fx.Times, got)
		for j := range got {
			maxd = math.Max(maxd, math.Abs(got[j]-e.Probs[j]))
		}
		if d := Decide(m, row, e.econ(1)); d.K != e.K {
			t.Fatalf("event %d: go K=%d lightgbm K=%d", i, d.K, e.K)
		}
		d := Decide(m, row, e.econ(fx.HiMult))
		hist[d.K]++
		if d.K != e.KHi {
			t.Fatalf("event %d: go K=%d lightgbm K=%d at x%.1f ping cost", i, d.K, e.KHi, fx.HiMult)
		}
	}
	t.Logf("%d decision points, max |dS| = %.3g; k histogram at x%.1f ping cost: %v", len(fx.Events), maxd, fx.HiMult, hist)
	if maxd >= 1e-9 {
		t.Fatalf("max abs survival difference %.3g", maxd)
	}
	if len(hist) < 2 {
		t.Fatal("the fixture exercises a single k")
	}
}

// fillFromFixture builds the Row a model sees from the fixture's expected feature values (numeric in training order, categoricals by CatNames).
func fillFromFixture(row *Row, fx *lgbFixture, e *lgbEvent) {
	for i, n := range fx.Numeric {
		v := math.NaN()
		if e.Feat[i] != nil {
			v = *e.Feat[i]
		}
		row.F[GBSBase+indexOfGBS(n)] = v
	}
	for i, c := range fx.CatOrd {
		for k, n := range CatNames {
			if n == c {
				row.Cat[k] = e.Cats[i]
			}
		}
	}
	row.Ctx = (func() float64 {
		for i, n := range fx.Numeric {
			if n == "m_ctx_tokens" {
				return *e.Feat[i]
			}
		}
		return math.NaN()
	})()
	row.ModelName = e.Model
}

// The live tracker, fed only start / end / response events, reproduces the Python feature vectors and the decisions.
func TestLGBFeatureParityWithPython(t *testing.T) {
	fx, m := loadLGBFixture(t)
	type ev struct {
		at   int64
		kind int
		i    int
	}
	var evs []ev
	for i := range fx.Events {
		e := &fx.Events[i]
		evs = append(evs, ev{e.Ts, 1, i}, ev{e.End, 0, i}, ev{e.Dec, 2, i})
	}
	sort.SliceStable(evs, func(a, b int) bool {
		if evs[a].at != evs[b].at {
			return evs[a].at < evs[b].at
		}
		return evs[a].kind < evs[b].kind
	})
	tr := NewTracker()
	arr := make([]*Arrival, len(fx.Events))
	bad := map[string]int{}
	var checked, badRows, kdiff, skipped, skippedKdiff int
	skippedBy, rowsBy := map[int]int{}, map[int]int{}
	for _, v := range evs {
		e := &fx.Events[v.i]
		resp := e.response()
		switch v.kind {
		case 1:
			arr[v.i] = tr.Arrive(resp.Request)
		case 0:
			tr.Ended(arr[v.i], resp)
		case 2:
			row := NewRow()
			tr.Features(arr[v.i], resp, row)
			if !e.Check {
				continue
			}
			checked++
			rowsBy[e.Tenant]++
			// the two inputs a model derives from its own table are checked through the model; compare the rest here
			rowBad := false
			for i, n := range fx.Numeric {
				if n == "m_ctx_window" || n == "m_ctx_fill" {
					continue
				}
				want := math.NaN()
				if e.Feat[i] != nil {
					want = *e.Feat[i]
				}
				got := row.F[GBSBase+indexOfGBS(n)]
				if n == "ping_over_stake" || n == "stake" {
					if math.Abs(got-want) <= 1e-6*math.Max(1, math.Abs(want)) {
						continue
					}
				}
				if math.IsNaN(got) && math.IsNaN(want) {
					continue
				}
				if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) || math.IsNaN(got) != math.IsNaN(want) {
					rowBad = true
					bad[n]++
				}
			}
			for i, c := range fx.CatOrd {
				for k, n := range CatNames {
					if n == c && row.Cat[k] != e.Cats[i] {
						rowBad = true
						bad[c]++
					}
				}
			}
			if row.Skipped {
				skipped++
				if Decide(m, row, e.econ(1)).K != e.K || Decide(m, row, e.econ(fx.HiMult)).K != e.KHi { // both cost regimes
					skippedKdiff++
				}
				skippedBy[e.Tenant]++
				continue
			}
			if rowBad {
				badRows++
			}
			if d := Decide(m, row, e.econ(1)); d.K != e.K && !row.Skipped {
				kdiff++
			}
		}
	}
	t.Logf("parallel-request rows by tenant (flagged/rows): %v / %v", skippedBy, rowsBy)
	t.Logf("%d rows checked; %d had a thread predecessor still running (their thread-history inputs come from the last finished request; %d of those %d decisions differ from the offline definition); of the other %d: %d differ %v; %d decisions differ", checked, skipped, skippedKdiff, skipped, checked-skipped, badRows, bad, kdiff)
	// allowed: at most 0.25% of rows, for a session-cost sum whose order of two requests finishing in the same millisecond differs
	// the parallel-request rows are where live and offline definitions differ by construction: bound the decision disagreement there too
	if skipped > 0 && skippedKdiff*2 > skipped {
		t.Fatalf("%d of %d parallel-request rows get a different ping count", skippedKdiff, skipped)
	}
	if badRows*400 > checked || kdiff > 0 {
		t.Fatalf("%d of %d rows differ, %d decisions differ", badRows, checked, kdiff)
	}
}

// LightGBM sends a value equal to the threshold left (decision type "<="). The parity fixtures do not hit an exact tie, so pin it here.
func TestLGBTreeSendsATieLeft(t *testing.T) {
	tr := &lgbTree{F: []int32{0, -1, -1}, T: []float64{0.5, 0, 0}, L: []int32{1, -1, -1}, R: []int32{2, -1, -1}, D: []bool{false, false, false}, M: []uint8{0, 0, 0}, V: []float64{0, 1, 2}}
	var x [NGBS + 2]float64
	for _, c := range []struct{ v, want float64 }{{0.5, 1}, {math.Nextafter(0.5, 1), 2}, {math.Nextafter(0.5, 0), 1}} {
		x[0] = c.v
		if got := tr.eval(&x); got != c.want {
			t.Errorf("x=%v: leaf %v, want %v", c.v, got, c.want)
		}
	}
}

func BenchmarkDecideLGB(b *testing.B) {
	fx, m := loadLGBFixture(b)
	e := &fx.Events[100]
	row := NewRow()
	fillFromFixture(row, fx, e)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Decide(m, row, e.econ(1))
	}
}
