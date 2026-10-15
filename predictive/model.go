// Package predictive is the learned keep-alive policy: from the facts known when a response ends it
// estimates P(this conversation returns inside each ping window) and picks how many keep-alive pings
// are worth buying. It is a small MLP (~6K parameters, a few seeds averaged) that outputs a censored
// mixture-of-Weibulls survival curve of the time-to-return, plus the economic rule on top of it:
//
//	k* = argmax_k  lam*stake*(S(Tc) - S(k*x + Tc)) - pingCost * sum_{j<=k} S(j*x)
//
// stake = prefix * (write5m - read) is what a rescued cache entry is worth. Training and the held-out
// comparison live in scripts/campaign; this package is inference only and costs microseconds.
package predictive

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// Layer sizes of the exported network.
const (
	NIn, H1, H2, NOut = 37, 48, 64, 10
)

// Rule is the economic decision rule's constants, exported with the model.
type Rule struct {
	Lam  float64 `json:"lam"`
	XS   float64 `json:"x_s"`  // seconds between pings
	KMax int     `json:"kmax"` // most pings ever bought
	TCS  float64 `json:"tc_s"` // the provider's cache lifetime, seconds
}

type netJSON struct {
	W1 [][]float64 `json:"W1"`
	B1 []float64   `json:"b1"`
	W2 [][]float64 `json:"W2"`
	B2 []float64   `json:"b2"`
	W3 [][]float64 `json:"W3"`
	B3 []float64   `json:"b3"`
}

type modelJSON struct {
	Schema string    `json:"schema"`
	Kind   string    `json:"kind"`
	Names  []string  `json:"names"`
	Mu     []float64 `json:"mu"`
	Sd     []float64 `json:"sd"`
	Rule   Rule      `json:"rule"`
	Nets   []netJSON `json:"nets"`
}

// net holds the weights flat and row-major (in x out), so a forward pass is three tight loops.
type net struct{ w1, b1, w2, b2, w3, b3 []float64 }

// MLP is the Weibull-mixture network. Safe for concurrent use.
type MLP struct {
	rule     Rule
	Features []string
	mu, sd   []float64
	nets     []net
}

func flat(m [][]float64, rows, cols int, what string) ([]float64, error) {
	if len(m) != rows {
		return nil, fmt.Errorf("predictive: %s has %d rows, want %d", what, len(m), rows)
	}
	out := make([]float64, 0, rows*cols)
	for _, r := range m {
		if len(r) != cols {
			return nil, fmt.Errorf("predictive: %s row has %d cols, want %d", what, len(r), cols)
		}
		out = append(out, r...)
	}
	return out, nil
}

// parseMLP validates and loads a Weibull-mixture MLP document (schema cg.cache.predictor/1).
func parseMLP(b []byte) (*MLP, error) {
	var j modelJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("predictive: model json: %w", err)
	}
	if j.Schema != "cg.cache.predictor/1" || j.Kind != "weibull3-mixture-mlp" {
		return nil, fmt.Errorf("predictive: unsupported model %q/%q", j.Schema, j.Kind)
	}
	if len(j.Names) != NIn || len(j.Mu) != NIn || len(j.Sd) != NIn {
		return nil, fmt.Errorf("predictive: model has %d features, runtime computes %d", len(j.Names), NIn)
	}
	for i, n := range FeatureNames {
		if j.Names[i] != n {
			return nil, fmt.Errorf("predictive: feature %d is %q in the model, %q in the runtime", i, j.Names[i], n)
		}
	}
	for _, s := range j.Sd {
		if s <= 0 || math.IsNaN(s) {
			return nil, fmt.Errorf("predictive: non-positive feature scale")
		}
	}
	if len(j.Nets) == 0 {
		return nil, fmt.Errorf("predictive: model has no networks")
	}
	if j.Rule.KMax < 1 || j.Rule.KMax > 12 || j.Rule.XS <= 0 || j.Rule.TCS <= 0 || j.Rule.Lam <= 0 {
		return nil, fmt.Errorf("predictive: implausible rule %+v", j.Rule)
	}
	m := &MLP{rule: j.Rule, Features: j.Names, mu: j.Mu, sd: j.Sd}
	for i, n := range j.Nets {
		var x net
		var err error
		if x.w1, err = flat(n.W1, NIn, H1, "W1"); err != nil {
			return nil, fmt.Errorf("net %d: %w", i, err)
		}
		if x.w2, err = flat(n.W2, H1, H2, "W2"); err != nil {
			return nil, fmt.Errorf("net %d: %w", i, err)
		}
		if x.w3, err = flat(n.W3, H2, NOut, "W3"); err != nil {
			return nil, fmt.Errorf("net %d: %w", i, err)
		}
		if len(n.B1) != H1 || len(n.B2) != H2 || len(n.B3) != NOut {
			return nil, fmt.Errorf("net %d: bias sizes", i)
		}
		x.b1, x.b2, x.b3 = n.B1, n.B2, n.B3
		m.nets = append(m.nets, x)
	}
	return m, nil
}

// NumNets is how many seeds are averaged.
func (m *MLP) NumNets() int { return len(m.nets) }

func (n *net) forward(x *[NIn]float64) (o [NOut]float64) {
	var h1 [H1]float64
	var h2 [H2]float64
	copy(h1[:], n.b1)
	for i := 0; i < NIn; i++ {
		xi, row := x[i], n.w1[i*H1:(i+1)*H1]
		for j := range h1 {
			h1[j] += xi * row[j]
		}
	}
	for j := range h1 {
		h1[j] = math.Tanh(h1[j])
	}
	copy(h2[:], n.b2)
	for i := 0; i < H1; i++ {
		row := n.w2[i*H2 : (i+1)*H2]
		for j := range h2 {
			h2[j] += h1[i] * row[j]
		}
	}
	for j := range h2 {
		h2[j] = math.Tanh(h2[j])
	}
	copy(o[:], n.b3)
	for i := 0; i < H2; i++ {
		row := n.w3[i*NOut : (i+1)*NOut]
		for j := range o {
			o[j] += h2[i] * row[j]
		}
	}
	return o
}

// mix is one network's distribution of the time to return, reduced to what S(t) needs.
type mix struct {
	pi [4]float64 // pi[0] is the never-returns atom
	la [3]float64 // log scales
	b  [3]float64 // shapes
}

func newMix(o *[NOut]float64) (m mix) {
	mx := math.Max(math.Max(o[0], o[1]), math.Max(o[2], o[3]))
	var s float64
	for i := 0; i < 4; i++ {
		m.pi[i] = math.Exp(o[i] - mx)
		s += m.pi[i]
	}
	for i := range m.pi {
		m.pi[i] /= s
	}
	for i := 0; i < 3; i++ {
		m.la[i] = o[4+i]
		m.b[i] = 0.3 + 3.7/(1+math.Exp(-o[7+i]))
	}
	return m
}

// surv is P(no return by t seconds).
func (m *mix) surv(t float64) float64 {
	lt := math.Log(math.Max(t, 0.05))
	s := m.pi[0]
	for i := 0; i < 3; i++ {
		u := math.Exp(math.Max(-50, math.Min(50, m.b[i]*(lt-m.la[i]))))
		s += m.pi[i+1] * math.Exp(-u)
	}
	return s
}

// Econ is the money side of one decision: what the entry is worth and what a ping costs.
type Econ struct {
	PrefixTok       float64
	PWrite5m, PRead float64 // $ per token
	PingCost        float64 // $ per ping
}

// Decision is the model's answer for one decision point.
type Decision struct {
	K       int     // pings to buy, 0 = let it lapse
	PWindow float64 // P(return inside the window the K pings cover), 0 when K == 0
	S295    float64 // P(still no return when the cache lifetime ends)
}

// Standardize writes (raw - mu) / sd into x.
func (m *MLP) Standardize(raw []float64, x *[NIn]float64) {
	for i := 0; i < NIn; i++ {
		x[i] = (raw[i] - m.mu[i]) / m.sd[i]
	}
}

// Kind implements Model.
func (m *MLP) Kind() string { return "weibull3-mixture-mlp" }

// Rule implements Model.
func (m *MLP) Rule() Rule { return m.rule }

// Survival implements Model: the seed-averaged S(t) at each of ts. The input is the first NIn values of row.F (FeatureNames order).
func (m *MLP) Survival(row *Row, ts []float64, out []float64) {
	var x [NIn]float64
	m.Standardize(row.F[:NIn], &x)
	for i := range out[:len(ts)] {
		out[i] = 0
	}
	for i := range m.nets {
		o := m.nets[i].forward(&x)
		mx := newMix(&o)
		for k, t := range ts {
			out[k] += mx.surv(t)
		}
	}
	inv := 1 / float64(len(m.nets))
	for k := range ts {
		out[k] *= inv
	}
}

// Model is a pluggable time-to-return predictor: it turns one Row of live features into the survival curve S(t) = P(no return by t seconds).
// The economic rule (Decide) is model independent. To add a model: implement this, register its "kind" in ParseModel, and have it read the inputs it
// needs from Row (the research feature blocks are documented in features.go and gbsfeatures.go).
type Model interface {
	Kind() string
	Rule() Rule
	// Survival writes S(t) for every t in ts into out (len(out) >= len(ts)).
	Survival(row *Row, ts []float64, out []float64)
}

// ParseModel loads a model document, dispatching on its "kind".
func ParseModel(b []byte) (Model, error) {
	var h struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("predictive: model json: %w", err)
	}
	switch h.Kind {
	case "weibull3-mixture-mlp":
		return parseMLP(b)
	case "lgb-discrete-hazard":
		return parseLGB(b)
	}
	return nil, fmt.Errorf("predictive: unknown model kind %q", h.Kind)
}

// LoadModel reads a model file.
func LoadModel(path string) (Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseModel(b)
}

// Decide applies the economic rule to one decision point: the number of pings k in 0..kmax that maximises
//
//	sum_{j<=k}  stake*(S(x(j-1)+Tc) - S(xj+Tc))  -  lam*pingCost*S(xj)
//
// (the j-th ping is paid only if the conversation has not returned by xj and rescues a return in the window it opens). Ties keep fewer pings.
func Decide(m Model, row *Row, e Econ) Decision {
	r := m.Rule()
	var ts, s [2*12 + 1]float64
	n := 1 + 2*r.KMax
	ts[0] = r.TCS
	for k := 1; k <= r.KMax; k++ {
		ts[k] = float64(k)*r.XS + r.TCS  // end of the window k pings cover
		ts[r.KMax+k] = float64(k) * r.XS // when ping k fires
	}
	m.Survival(row, ts[:n], s[:n])
	stake := e.PrefixTok * (e.PWrite5m - e.PRead)
	best, bestNet, net := 0, 0.0, 0.0
	for k := 1; k <= r.KMax; k++ {
		net += stake*(s[k-1]-s[k]) - r.Lam*e.PingCost*s[r.KMax+k]
		if net > bestNet+1e-12 {
			best, bestNet = k, net
		}
	}
	d := Decision{K: best, S295: s[0]}
	if best > 0 {
		d.PWindow = s[0] - s[best]
	}
	return d
}
