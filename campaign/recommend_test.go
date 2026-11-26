package campaign

import (
	"strings"
	"testing"
)

func pick(opts ...Option) Pick {
	p := NewPicker()
	for _, o := range opts {
		p.Add(o)
	}
	return p.Result()
}

var (
	never = Option{ID: "never", Net: -5, Diff: -5, CI: [2]float64{-6, -4}}
	def   = Option{ID: "default", Net: 0}
)

func TestPickerRecommendsTheAdaptedOptionThatPassesTheGate(t *testing.T) {
	r := pick(never, def, Option{ID: "K3", Net: 4, Diff: 4, CI: [2]float64{1, 7}},
		Option{ID: "auto", Adapted: true, Net: 3, Diff: 3, CI: [2]float64{1, 5}, GatePass: true},
		Option{ID: "by_dow", Adapted: true, Net: 2, Diff: 2, CI: [2]float64{0.5, 4}, GatePass: true})
	if !r.Recommends() || r.GateID != "auto" || r.GateDiff != 3 {
		t.Fatalf("gate pick %q diff %v recommends %v", r.GateID, r.GateDiff, r.Recommends())
	}
	if r.BestID != "K3" || !r.BeatsDefault() { // the best by net is a fixed plan; the recommendation is the best that passes the gate
		t.Errorf("best %q beats %v", r.BestID, r.BeatsDefault())
	}
	if !strings.Contains(r.Summary(), "auto beats the default by $3.00 [1.00, 5.00]") {
		t.Errorf("summary %q", r.Summary())
	}
}

func TestPickerSummaryWhenTheLeaderFailsTheGate(t *testing.T) {
	r := pick(never, def, Option{ID: "auto", Adapted: true, Net: 3, Diff: 3, CI: [2]float64{1, 5}, GateWhy: []string{"vs blanket K3 +1.00 [-1.00, 2.00]"}})
	if r.Recommends() || !r.BeatsDefault() {
		t.Fatalf("recommends %v beats %v", r.Recommends(), r.BeatsDefault())
	}
	if s := r.Summary(); !strings.Contains(s, "does not pass the campaign builder's gate") || !strings.Contains(s, "vs blanket K3 +1.00") {
		t.Errorf("summary %q", s)
	}
	// the gateway-dollar lead and the failed gate disagree, and the report says so in one line
	if len(r.Disagree) != 1 || !strings.Contains(r.Disagree[0], "fails the builder's calibrated gate") {
		t.Errorf("disagreement %v", r.Disagree)
	}
}

func TestPickerSummaryForAFixedLeaderAndForNoLeader(t *testing.T) {
	if s := pick(never, def, Option{ID: "K2", Net: 2, Diff: 2, CI: [2]float64{1, 3}}).Summary(); !strings.Contains(s, "it is a fixed plan") {
		t.Errorf("a fixed leader: %q", s)
	}
	if s := pick(never, def, Option{ID: "K1", Net: 1, Diff: 1, CI: [2]float64{-1, 3}}).Summary(); !strings.Contains(s, "includes zero") {
		t.Errorf("a lead inside the interval: %q", s)
	}
	if s := pick(never, Option{ID: "default", Net: 9}).Summary(); !strings.Contains(s, "default keep-alive is the best") {
		t.Errorf("the default leads: %q", s)
	}
	if r := pick(never); r.BestID != "" || !strings.Contains(r.Summary(), "too little history") {
		t.Errorf("no option: best %q summary %q", r.BestID, r.Summary())
	}
}

// An adapted option that passes the gate with a gateway-dollar interval that includes zero is named too.
func TestPickerNamesAGatePassThatGatewayDollarsDoNotSupport(t *testing.T) {
	r := pick(def, Option{ID: "auto", Adapted: true, Net: 2, Diff: 2, CI: [2]float64{-1, 5}, GatePass: true})
	if len(r.Disagree) != 1 || !strings.Contains(r.Disagree[0], "passes the builder's calibrated gate") {
		t.Errorf("disagreement %v", r.Disagree)
	}
}
