package predictive

import (
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"sort"
	"testing"
)

type fixture struct {
	Times  []float64 `json:"times"`
	Events []struct {
		ID         int64     `json:"id"`
		Tenant     int       `json:"tenant"`
		Session    int       `json:"session"`
		Thread     int       `json:"thread"`
		Model      string    `json:"model"`
		Ts         int64     `json:"ts"`
		End        int64     `json:"end"`
		Dec        int64     `json:"dec"`
		Prefix     int64     `json:"prefix"`
		CacheWrite int64     `json:"cache_write"`
		Fresh      int64     `json:"fresh"`
		Out        int64     `json:"out"`
		Messages   int       `json:"messages"`
		Tools      int       `json:"tools"`
		Stop       string    `json:"stop"`
		Agent      string    `json:"agent"`
		Thinking   string    `json:"thinking"`
		Stream     bool      `json:"stream"`
		PIn        float64   `json:"p_in"`
		POut       float64   `json:"p_out"`
		PRead      float64   `json:"p_read"`
		PWrite5m   float64   `json:"p_write5m"`
		Feat       []float64 `json:"feat"`
		Probs      []float64 `json:"probs"`
		K          int       `json:"k"`
		Check      bool      `json:"check"`
	} `json:"events"`
}

func loadFixture(t testing.TB) (*fixture, Model) {
	f, err := os.Open("testdata/parity.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var fx fixture
	if err := json.NewDecoder(zr).Decode(&fx); err != nil {
		t.Fatal(err)
	}
	m, err := LoadModel("testdata/parity.model.json")
	if err != nil {
		t.Fatal(err)
	}
	return &fx, m
}

// The Python model and the Go model, given the SAME input vector, agree on every survival probability to 1e-5 and on the number of pings.
func TestModelParityWithPython(t *testing.T) {
	fx, m := loadFixture(t)
	var maxd float64
	disagree := 0
	for i, e := range fx.Events {
		row := NewRow()
		copy(row.F, e.Feat)
		got := make([]float64, len(fx.Times))
		m.Survival(row, fx.Times, got)
		for j := range got {
			maxd = math.Max(maxd, math.Abs(got[j]-e.Probs[j]))
		}
		d := Decide(m, row, Econ{PrefixTok: float64(e.Prefix), PWrite5m: e.PWrite5m, PRead: e.PRead, PingCost: float64(e.Prefix)*e.PRead + 7*e.PIn + e.POut})
		if d.K != e.K {
			disagree++
			t.Logf("event %d: go K=%d python K=%d", i, d.K, e.K)
		}
	}
	t.Logf("%d decision points, max |dP| = %.3g", len(fx.Events), maxd)
	if maxd >= 1e-5 {
		t.Fatalf("max abs probability difference %.3g >= 1e-5", maxd)
	}
	if disagree > 0 {
		t.Fatalf("%d of %d decisions differ", disagree, len(fx.Events))
	}
}

// tol is 1e-9 everywhere except the features built from closed gaps: the research table stores response ends rounded to the
// millisecond but return gaps unrounded, so a gap differs by up to 0.5 ms (about 1e-3 after log1p of seconds).
func tol(j int) float64 {
	if FeatureNames[j] == "l_gq" {
		return 2e-3
	}
	return 1e-9
}

// Replaying the request stream through the live Tracker reproduces the Python feature vectors.
func TestFeatureParityWithPython(t *testing.T) {
	fx, m := loadFixture(t)
	type ev struct {
		at   int64
		kind int // 0 = response ended, 1 = next request arrives, 2 = proxy has returned the response
		i    int
	}
	var evs []ev
	for i, e := range fx.Events {
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
	bad := map[int]int{}
	var maxd float64
	var nbad, nrounding, kdiff int
	k := 0
	for _, v := range evs {
		e := fx.Events[v.i]
		rq := Request{Tenant: string(rune('A' + e.Tenant)), Session: string(rune('a'+e.Session%26)) + string(rune('A'+e.Session/26)), Thread: string(rune('a'+e.Thread%26)) + string(rune('A'+e.Thread/26)), Model: e.Model, TsMs: e.Ts}
		if v.kind == 1 {
			arr[v.i] = tr.Arrive(rq)
			continue
		}
		resp := &Response{Request: rq, EndMs: e.End, DecMs: e.Dec, Prefix: e.Prefix, CacheWrite: e.CacheWrite, Fresh: e.Fresh, Output: e.Out, Messages: e.Messages, Tools: e.Tools,
			StopReason: e.Stop, Agent: e.Agent, Thinking: e.Thinking, Stream: e.Stream, PIn: e.PIn, POut: e.POut, PRead: e.PRead, PWrite5m: e.PWrite5m}
		if v.kind == 0 {
			tr.Ended(arr[v.i], resp)
			continue
		}
		row := NewRow()
		tr.Features(arr[v.i], resp, row)
		got := row.F[:NIn]
		if !e.Check {
			continue
		}
		if d := Decide(m, row, Econ{PrefixTok: float64(e.Prefix), PWrite5m: e.PWrite5m, PRead: e.PRead, PingCost: resp.PingCost()}); d.K != e.K {
			kdiff++
		}
		rowBad := false
		for j := 0; j < NIn; j++ {
			d := math.Abs(got[j] - e.Feat[j])
			if d > tol(j) {
				rowBad = true
				bad[j]++
				maxd = math.Max(maxd, d)
			}
		}
		if rowBad {
			// The research table keeps response ends rounded to the ms but return gaps unrounded, so a gap closes up to 0.5 ms
			// away from where the table says. The only rows that can differ are those whose decision lands within 1 ms of a same-tenant arrival.
			near := false
			for _, o := range fx.Events {
				if o.Tenant == e.Tenant && o.Ts >= e.Dec-1 && o.Ts <= e.Dec+1 {
					near = true
				}
			}
			if near {
				nrounding++
				continue
			}
			nbad++
			t.Logf("row id=%d tenant=%d ts=%d end=%d dec=%d: kv_p5 go %.4f py %.4f kv_ln go %.3f py %.3f gq go %.4f py %.4f", e.ID, e.Tenant, e.Ts, e.End, e.Dec, got[18], e.Feat[18], got[20], e.Feat[20], got[22], e.Feat[22])
		}
		k++
	}
	if nrounding*200 > k { // at most 0.5% of rows may be skipped as sub-millisecond ties
		t.Fatalf("%d of %d rows skipped as sub-millisecond ties", nrounding, k)
	}
	t.Logf("%d rows: %d differ beyond tolerance; %d more are sub-millisecond ties of the research table (skipped); per feature: %v", k, nbad, nrounding, bad)
	t.Logf("end to end (live features -> decision): %d of %d decisions differ from Python", kdiff, k)
	if kdiff > 0 {
		t.Fatalf("%d decisions differ end to end", kdiff)
	}
	if nbad > 0 {
		t.Fatalf("%d of %d feature vectors differ from Python (max %.3g)", nbad, k, maxd)
	}
}

// One decision costs microseconds.
func BenchmarkDecideMLP(b *testing.B) {
	fx, m := loadFixture(b)
	e := fx.Events[100]
	econ := Econ{PrefixTok: float64(e.Prefix), PWrite5m: e.PWrite5m, PRead: e.PRead, PingCost: 0.1}
	row := NewRow()
	copy(row.F, e.Feat)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Decide(m, row, econ)
	}
}
