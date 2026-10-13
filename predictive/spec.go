package predictive

import (
	"encoding/json"
	"fmt"
)

// SpecSchema is the campaign document this package reads. v2 extends the v1 decision-tree campaign with a model reference:
// a campaign is an ordered list of segments, each naming the rule that decides for the requests it matches.
const SpecSchema = "cg.cache.campaign/2"

// Rule kinds a segment may name.
const (
	RuleModel = "model" // the predictor picks the number of pings per request
	RulePlan  = "plan"  // a fixed plan from the menu (uniform: Plan.GapsS[0] seconds apart, len(GapsS) pings)
	RuleNone  = "none"  // never ping
)

// Plan is a menu entry.
type Plan struct {
	GapsS    []float64 `json:"gaps_s"`
	MaxPings int       `json:"max_pings"`
	Tier     string    `json:"tier"`
}

// SegRule is the rule one segment applies.
type SegRule struct {
	Kind string `json:"kind"`
	Plan string `json:"plan,omitempty"`
	// Lam optionally rescales the model's value of a rescue for this segment (default 1).
	Lam float64 `json:"lam,omitempty"`
}

// Segment applies Rule to the requests where every predicate in When holds. When is a list of [fact, op, value] triples.
type Segment struct {
	ID   string         `json:"id"`
	When [][3]any       `json:"when"`
	Rule SegRule        `json:"rule"`
	Raw  map[string]any `json:"-"`
}

// ModelRef points at the predictor file next to the spec.
type ModelRef struct {
	Ref      string `json:"ref"`
	SHA256   string `json:"sha256"`
	Schema   string `json:"schema"`
	Kind     string `json:"kind"`
	Features int    `json:"n_features"`
	Rule     Rule   `json:"rule"`
}

// Policy is the executable part of a campaign.
type Policy struct {
	Kind     string    `json:"kind"` // "rules" (no model) or "model+segments"
	Model    *ModelRef `json:"model,omitempty"`
	Segments []Segment `json:"segments"`
	Fallback SegRule   `json:"fallback"`
}

// Spec is the campaign document. Only the fields a runtime needs are typed; the rest (evidence, caveats, ...) is kept verbatim in Extra for the API to return.
type Spec struct {
	Schema  string          `json:"schema"`
	Version string          `json:"version"`
	Policy  Policy          `json:"policy"`
	Menu    map[string]Plan `json:"menu"`
}

// ParseSpec validates a campaign document.
func ParseSpec(b []byte) (*Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("campaign spec: %w", err)
	}
	if s.Schema != SpecSchema {
		return nil, fmt.Errorf("campaign spec: schema %q, want %q", s.Schema, SpecSchema)
	}
	switch s.Policy.Kind {
	case "rules":
		if s.Policy.Model != nil {
			return nil, fmt.Errorf("campaign spec: kind rules cannot carry a model")
		}
	case "model+segments":
		if s.Policy.Model == nil || s.Policy.Model.Ref == "" || s.Policy.Model.SHA256 == "" {
			return nil, fmt.Errorf("campaign spec: kind model+segments needs a model with ref and sha256")
		}
	default:
		return nil, fmt.Errorf("campaign spec: unknown policy kind %q", s.Policy.Kind)
	}
	check := func(r SegRule, where string) error {
		switch r.Kind {
		case RuleNone:
		case RuleModel:
			if s.Policy.Model == nil {
				return fmt.Errorf("campaign spec: %s uses the model but the policy has none", where)
			}
		case RulePlan:
			p, ok := s.Menu[r.Plan]
			if !ok || len(p.GapsS) == 0 {
				return fmt.Errorf("campaign spec: %s names missing or empty plan %q", where, r.Plan)
			}
		default:
			return fmt.Errorf("campaign spec: %s has unknown rule kind %q", where, r.Kind)
		}
		return nil
	}
	for _, sg := range s.Policy.Segments {
		if err := check(sg.Rule, "segment "+sg.ID); err != nil {
			return nil, err
		}
		for _, w := range sg.When {
			if _, ok := w[0].(string); !ok {
				return nil, fmt.Errorf("campaign spec: segment %s has a predicate without a fact name", sg.ID)
			}
			if op, _ := w[1].(string); op == "" {
				return nil, fmt.Errorf("campaign spec: segment %s has a predicate without an operator", sg.ID)
			}
		}
	}
	if s.Policy.Fallback.Kind == "" {
		s.Policy.Fallback = SegRule{Kind: RuleNone}
	}
	if err := check(s.Policy.Fallback, "fallback"); err != nil {
		return nil, err
	}
	return &s, nil
}

// Facts are the per-request values a segment predicate may test.
type Facts struct {
	Family, StopReason, Model, Agent string
	PrefixTok, PingCost              float64
	SessionTurn                      float64
}

func (f *Facts) get(name string) (str string, num float64, isNum bool, ok bool) {
	switch name {
	case "family":
		return f.Family, 0, false, true
	case "stop_reason":
		return f.StopReason, 0, false, true
	case "model":
		return f.Model, 0, false, true
	case "agent":
		return f.Agent, 0, false, true
	case "prefix_tok":
		return "", f.PrefixTok, true, true
	case "ping_cost":
		return "", f.PingCost, true, true
	}
	return "", 0, false, false
}

// match reports whether one predicate holds. An unknown fact or a type mismatch never matches (the segment is skipped, not guessed).
func (f *Facts) match(w [3]any) bool {
	name, _ := w[0].(string)
	op, _ := w[1].(string)
	s, n, isNum, ok := f.get(name)
	if !ok {
		return false
	}
	if isNum {
		v, ok := w[2].(float64)
		if !ok {
			return false
		}
		switch op {
		case "==":
			return n == v
		case "!=":
			return n != v
		case "<":
			return n < v
		case "<=":
			return n <= v
		case ">":
			return n > v
		case ">=":
			return n >= v
		}
		return false
	}
	v, ok := w[2].(string)
	if !ok {
		return false
	}
	switch op {
	case "==":
		return s == v
	case "!=":
		return s != v
	}
	return false
}

// RuleFor returns the first matching segment's rule, or the fallback.
func (s *Spec) RuleFor(f *Facts) SegRule {
	for i := range s.Policy.Segments {
		sg := &s.Policy.Segments[i]
		ok := true
		for _, w := range sg.When {
			if !f.match(w) {
				ok = false
				break
			}
		}
		if ok {
			return sg.Rule
		}
	}
	return s.Policy.Fallback
}

// PlanOf turns a fixed-plan rule into (count, seconds between pings).
func (s *Spec) PlanOf(r SegRule) (k int, idleS float64) {
	p := s.Menu[r.Plan]
	if len(p.GapsS) == 0 {
		return 0, 0
	}
	return len(p.GapsS), p.GapsS[0]
}
