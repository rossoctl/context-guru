package predictive

import (
	"encoding/json"
	"fmt"
	"math"
)

// LGB is a LightGBM discrete-time hazard model: the time to return is cut into bins at Edges; for every bin a gradient-boosted classifier gives the
// hazard h_b = P(return in bin b | not returned before it) from the 72 request inputs plus the bin and its right edge. S(edge_b) = prod_{i<=b} (1 - h_i).
// Trees are exported from LightGBM as flat node arrays; evaluation is the same arithmetic LightGBM's predict does (split on <=, NaN / zero routed by
// the stored default direction).
type LGB struct {
	rule    Rule
	edges   []float64
	enc     [NCat]map[string]float64
	windows map[string]float64
	trees   []lgbTree
}

type lgbTree struct {
	F    []int32
	T    []float64
	L, R []int32
	D    []bool
	M    []uint8
	V    []float64
}

type lgbJSON struct {
	Schema     string              `json:"schema"`
	Kind       string              `json:"kind"`
	Names      []string            `json:"names"`
	Categories map[string][]string `json:"categories"`
	Windows    map[string]float64  `json:"windows"`
	Edges      []float64           `json:"edges"`
	Rule       Rule                `json:"rule"`
	Trees      []struct {
		F []int32   `json:"f"`
		T []float64 `json:"t"`
		L []int32   `json:"l"`
		R []int32   `json:"r"`
		D []int     `json:"d"`
		M []int     `json:"m"`
		V []float64 `json:"v"`
	} `json:"trees"`
}

func parseLGB(b []byte) (*LGB, error) {
	var j lgbJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("predictive: model json: %w", err)
	}
	if j.Schema != "cg.cache.predictor/1" || j.Kind != "lgb-discrete-hazard" {
		return nil, fmt.Errorf("predictive: unsupported model %q/%q", j.Schema, j.Kind)
	}
	if len(j.Names) != NGBS {
		return nil, fmt.Errorf("predictive: model has %d features, runtime computes %d", len(j.Names), NGBS)
	}
	for i, n := range GBSNames {
		if j.Names[i] != n {
			return nil, fmt.Errorf("predictive: feature %d is %q in the model, %q in the runtime", i, j.Names[i], n)
		}
	}
	if len(j.Edges) < 2 || j.Edges[0] != 0 || len(j.Trees) == 0 {
		return nil, fmt.Errorf("predictive: bad bin edges or no trees")
	}
	for i := 1; i < len(j.Edges); i++ {
		if j.Edges[i] <= j.Edges[i-1] {
			return nil, fmt.Errorf("predictive: bin edges must increase")
		}
	}
	if j.Rule.KMax < 1 || j.Rule.KMax > 12 || j.Rule.XS <= 0 || j.Rule.TCS <= 0 || j.Rule.Lam <= 0 {
		return nil, fmt.Errorf("predictive: implausible rule %+v", j.Rule)
	}
	m := &LGB{rule: j.Rule, edges: j.Edges, windows: j.Windows}
	for i, c := range CatNames {
		m.enc[i] = map[string]float64{}
		for k, v := range j.Categories[c] {
			m.enc[i][v] = float64(k)
		}
	}
	for ti, t := range j.Trees {
		n := len(t.F)
		if n == 0 || len(t.T) != n || len(t.L) != n || len(t.R) != n || len(t.D) != n || len(t.M) != n || len(t.V) != n {
			return nil, fmt.Errorf("predictive: tree %d has inconsistent node arrays", ti)
		}
		lt := lgbTree{F: t.F, T: t.T, L: t.L, R: t.R, V: t.V, D: make([]bool, n), M: make([]uint8, n)}
		for i := 0; i < n; i++ {
			lt.D[i], lt.M[i] = t.D[i] != 0, uint8(t.M[i])
			if t.F[i] >= 0 && (int(t.F[i]) >= NGBS+2 || t.L[i] <= int32(i) || t.R[i] <= int32(i) || int(t.L[i]) >= n || int(t.R[i]) >= n) {
				return nil, fmt.Errorf("predictive: tree %d node %d is malformed", ti, i)
			}
		}
		m.trees = append(m.trees, lt)
	}
	return m, nil
}

// Kind implements Model.
func (m *LGB) Kind() string { return "lgb-discrete-hazard" }

// Rule implements Model.
func (m *LGB) Rule() Rule { return m.rule }

func (t *lgbTree) eval(x *[NGBS + 2]float64) float64 {
	k := int32(0)
	for t.F[k] >= 0 {
		v := x[t.F[k]]
		mt := t.M[k]
		if math.IsNaN(v) && mt != 2 {
			v = 0
		}
		if (mt == 2 && math.IsNaN(v)) || (mt == 1 && math.Abs(v) <= 1e-35) {
			if t.D[k] {
				k = t.L[k]
			} else {
				k = t.R[k]
			}
		} else if v <= t.T[k] {
			k = t.L[k]
		} else {
			k = t.R[k]
		}
	}
	return t.V[k]
}

// input assembles the 72 model inputs from a row: numerics as computed, categoricals by this model's code table, the two window-derived inputs from its window table.
func (m *LGB) input(row *Row, x *[NGBS + 2]float64) {
	copy(x[:NGBS], row.F[GBSBase:])
	for i := range CatNames {
		code, ok := m.enc[i][row.Cat[i]]
		if !ok {
			code = nan
		}
		x[catIdx[i]] = code
	}
	w, ok := m.windows[row.ModelName]
	if !ok {
		w = nan
	}
	x[idxCtxWindow], x[idxCtxFill] = w, ratio(row.Ctx, w)
	// The model was trained, and its reference predicts, on float32 inputs (gbsfeat.encode), so its thresholds sit on float32 values: compare
	// at that precision, or a float64 input just above a float32 threshold goes the other way.
	for i := range x[:NGBS] {
		x[i] = float64(float32(x[i]))
	}
}

var (
	idxCtxWindow = indexOfGBS("m_ctx_window")
	idxCtxFill   = indexOfGBS("m_ctx_fill")
	catIdx       = func() (o [NCat]int) {
		for i, c := range CatNames {
			o[i] = indexOfGBS(c)
		}
		return
	}()
)

func indexOfGBS(name string) int {
	for i, n := range GBSNames {
		if n == name {
			return i
		}
	}
	panic("predictive: unknown GBS feature " + name)
}

// Hazards returns the hazard of every bin whose right edge is <= upTo.
func (m *LGB) Hazards(row *Row, upTo float64, out []float64) []float64 {
	var x [NGBS + 2]float64
	m.input(row, &x)
	out = out[:0]
	for b := 0; b+1 < len(m.edges) && m.edges[b+1] <= upTo+1e-9; b++ {
		x[NGBS], x[NGBS+1] = float64(b), m.edges[b]
		var s float64
		for i := range m.trees {
			s += m.trees[i].eval(&x)
		}
		out = append(out, 1/(1+math.Exp(-s)))
	}
	return out
}

// Survival implements Model. S at a time that is not a bin edge is interpolated linearly between the two edges around it.
func (m *LGB) Survival(row *Row, ts []float64, out []float64) {
	upTo := 0.0
	for _, t := range ts {
		upTo = math.Max(upTo, t)
	}
	var hb [64]float64
	h := m.Hazards(row, upTo, hb[:0])
	var se [65]float64
	se[0] = 1
	for i, v := range h {
		se[i+1] = se[i] * (1 - v)
	}
	for k, t := range ts {
		i := 0
		for i+1 < len(h)+1 && i+1 < len(m.edges) && m.edges[i+1] < t-1e-9 {
			i++
		}
		// edges[i] < t <= edges[i+1] (or t == 0)
		switch {
		case t <= 0:
			out[k] = 1
		case i+1 > len(h):
			out[k] = se[len(h)]
		case math.Abs(m.edges[i+1]-t) < 1e-9:
			out[k] = se[i+1]
		default:
			f := (t - m.edges[i]) / (m.edges[i+1] - m.edges[i])
			out[k] = se[i] + f*(se[i+1]-se[i])
		}
	}
}
