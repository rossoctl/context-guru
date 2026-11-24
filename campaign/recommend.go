package campaign

import (
	"fmt"
	"math"
	"strings"
)

// Option is one strategy of the adapt report, scored on the held-out rows against the default.
type Option struct {
	ID      string
	Adapted bool       // an adapted plan: the builder's gate applies to it
	Net     float64    // its held-out net, gateway dollars
	Diff    float64    // its held-out net minus the default's
	CI      [2]float64 // the paired session interval of Diff
	// GatePass and GateWhy are the builder's ship gate (ShipGate) on the same rows, for an adapted option.
	GatePass bool
	GateWhy  []string
}

// Picker picks the recommendation of an adapt report from its options, in the order they are added.
type Picker struct {
	bestID  string
	bestNet float64
	bestCI  [2]float64
	bestD   float64

	gateID  string
	gateNet float64
	gateD   float64
	gateCI  [2]float64

	failID  string
	failNet float64
	failWhy string

	disID    string
	disNet   float64
	disagree []string
}

// NewPicker is a Picker with no option yet.
func NewPicker() *Picker { return &Picker{bestNet: math.Inf(-1)} }

// Add scores one option. "never" is a baseline, never the best. Among the adapted options, the one that leads in gateway dollars while the builder's
// gate disagrees (either way) is named in Disagreement.
func (p *Picker) Add(o Option) {
	if o.Adapted {
		if realBeats := o.CI[0] > 0; realBeats != o.GatePass && (p.disID == "" || o.Net > p.disNet) {
			p.disID, p.disNet, p.disagree = o.ID, o.Net, nil
			if realBeats {
				p.disagree = append(p.disagree, fmt.Sprintf("%s leads the default in gateway dollars (%+.2f [%.2f, %.2f]) but fails the builder's calibrated gate; the gate decides, so it would run in shadow unless applied anyway.", o.ID, o.Diff, o.CI[0], o.CI[1]))
			} else {
				p.disagree = append(p.disagree, fmt.Sprintf("%s passes the builder's calibrated gate, but in gateway dollars its lead over the default (%+.2f [%.2f, %.2f]) is not clear of zero.", o.ID, o.Diff, o.CI[0], o.CI[1]))
			}
		}
		if o.GatePass && (p.gateID == "" || o.Net > p.gateNet) {
			p.gateID, p.gateNet, p.gateD, p.gateCI = o.ID, o.Net, o.Diff, o.CI
		}
		if !o.GatePass && (p.failID == "" || o.Net > p.failNet) {
			p.failID, p.failNet, p.failWhy = o.ID, o.Net, strings.Join(o.GateWhy, "; ")
		}
	}
	if o.ID != "never" && o.Net > p.bestNet {
		p.bestID, p.bestNet, p.bestCI, p.bestD = o.ID, o.Net, o.CI, o.Diff
	}
}

// Pick is the outcome of a Picker.
type Pick struct {
	BestID     string
	BestCI     [2]float64
	BestDiff   float64
	GateID     string // the adapted option with the best net that passes the builder's gate; "" when none does
	GateDiff   float64
	GateCI     [2]float64
	Disagree   []string
	summary    string
	failID     string
	failWhy    string
	beats      bool
	recommends bool
}

// Result is the pick, with the summary sentence of the report.
func (p *Picker) Result() Pick {
	r := Pick{BestID: p.bestID, BestCI: p.bestCI, BestDiff: p.bestD, GateID: p.gateID, GateDiff: p.gateD, GateCI: p.gateCI, Disagree: p.disagree, failID: p.failID, failWhy: p.failWhy}
	r.beats = r.BestID != "default" && r.BestCI[0] > 0
	switch {
	case r.BestID == "":
		r.summary = "There is too little history to compare strategies; keep the default."
	case r.GateID != "":
		r.summary = fmt.Sprintf("%s beats the default by $%.2f [%.2f, %.2f] on your last 30%% of decisions (plan tuned only on what came before) and passes the campaign builder's gate (it also beats blanket K3 and the fleet plan).",
			r.GateID, r.GateDiff, r.GateCI[0], r.GateCI[1])
		r.recommends = true
	case r.beats && r.failID == r.BestID:
		r.summary = fmt.Sprintf("%s is $%.2f [%.2f, %.2f] ahead of the default on your last 30%% of decisions, but it does not pass the campaign builder's gate (in the builder's calibrated dollars: %s), so applying it would only run it in shadow. Keep the default.",
			r.BestID, r.BestDiff, r.BestCI[0], r.BestCI[1], r.failWhy)
	case r.beats:
		r.summary = fmt.Sprintf("%s is $%.2f [%.2f, %.2f] ahead of the default on your last 30%% of decisions, but it is a fixed plan, shown for comparison, and no adapted plan passes the campaign builder's gate. Keep the default.",
			r.BestID, r.BestDiff, r.BestCI[0], r.BestCI[1])
	case r.BestID == "default":
		r.summary = "The default keep-alive is the best strategy on your held-out data; keep it."
	default:
		r.summary = fmt.Sprintf("No strategy beats the default on held-out data: the best, %s, is $%.2f ahead on your last 30%% of decisions, but the 95%% interval [%.2f, %.2f] includes zero. Keep the default.",
			r.BestID, r.BestDiff, r.BestCI[0], r.BestCI[1])
	}
	return r
}

// Summary is the one-paragraph answer of the report.
func (r Pick) Summary() string { return r.summary }

// BeatsDefault reports whether the best option leads the default with a 95% interval above zero.
func (r Pick) BeatsDefault() bool { return r.beats }

// Recommends reports whether the report recommends a change: an adapted option passed the builder's gate.
func (r Pick) Recommends() bool { return r.recommends }
