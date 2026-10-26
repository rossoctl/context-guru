// Package opscases is the exhaustive list of test cases the implementation must meet
// (design.md §10). A case holds only what the spec decides: the input and the expected result.
// It never holds the current code's figure; the agreement test computes that each time it runs.
//
// The JSON files in cases/ are written by gen_cases.py, which computes the body hashes.
package opscases

//go:generate python3 gen_cases.py

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

//go:embed cases/*.json differences.json
var casesFS embed.FS

// Case is one test case (§10.2): a sequence of steps on a fresh Trackers, because the result of a
// request depends on the ones before it (D14).
type Case struct {
	ID string `json:"id"`
	// Description says what the case tests and where its figures come from, in plain words.
	Description string            `json:"description"`
	Source      string            `json:"source,omitempty"` // the document the figures come from
	Dims        map[string]string `json:"dims"`
	// Open names the spec's open questions (§11) that the expected figures depend on. Such a case
	// is not frozen: the question's answer may change it.
	Open  []string `json:"open,omitempty"`
	Rates string   `json:"rates"` // names a rate table in Rates
	Steps []Step   `json:"steps"`
}

// Step is the calls of one request and the Result its Compute must return, or one standalone
// RequestOps (a keep-alive ping) priced with ComputeStandalone.
type Step struct {
	Standalone *cf.RequestOps `json:"standalone,omitempty"`
	Begin      *Begin         `json:"begin,omitempty"`
	Add        []cf.Operation `json:"add,omitempty"` // as the operators reported them; replays are not repeated (§4.6)
	End        *End           `json:"end,omitempty"`
	Usage      cf.Usage       `json:"usage"`
	Expected   Expected       `json:"expected"`
}

// IsStandalone reports whether the step is a RequestOps with no request record.
func (s Step) IsStandalone() bool { return s.Standalone != nil }

// Begin is the argument of Trackers.BeginRequest.
type Begin struct {
	Meta              cf.RequestMeta  `json:"meta"`
	Client            cf.Body         `json:"client"`
	ClientBreakpoints []cf.Breakpoint `json:"clientBreakpoints"`
}

// End is the argument of Request.EndRequest.
type End struct {
	Thread      cf.Thread       `json:"thread"`
	Forwarded   cf.Body         `json:"forwarded"`
	Breakpoints []cf.Breakpoint `json:"breakpoints"`
	Facts       Facts           `json:"facts"`
}

// Facts is cf.EntryFacts with a readable TTL ("5m").
type Facts struct {
	TTL        string          `json:"ttl"`
	Lifetime   cf.LifetimeKind `json:"lifetime"`
	LastPing   time.Time       `json:"lastPing,omitzero"`
	PingRead   *cf.Position    `json:"pingRead,omitempty"`
	PingWindow string          `json:"pingWindow,omitempty"`
}

// EntryFacts converts the facts to the library's type. It panics on a malformed TTL, which is a
// bug in the case file.
func (f Facts) EntryFacts() cf.EntryFacts {
	ttl, err := time.ParseDuration(f.TTL)
	if err != nil {
		panic(fmt.Sprintf("opscases: facts ttl %q: %v", f.TTL, err))
	}
	return cf.EntryFacts{TTL: ttl, Lifetime: f.Lifetime, LastPing: f.LastPing, PingRead: f.PingRead, PingWindow: f.PingWindow}
}

// Expected is what Compute must return for one step, in tokens for each tier.
type Expected struct {
	Phase cf.CachePhase `json:"phase,omitempty"`
	// Boundary is asserted only when present. A cold request has B* = 0 and no position to name.
	Boundary *cf.Position `json:"boundary,omitempty"`
	// BaselineUsage is what the provider would have billed for the client body with no context
	// operation at all (§6.3 conservation). The step's Usage is what it billed with them. Their
	// price difference is the sum of Saved minus Caused over Ops. Absent on a standalone step.
	BaselineUsage *cf.Usage `json:"baselineUsage,omitempty"`
	// Ops are the priced operations that save or cost something, or carry a Call, in the order
	// Compute returns them: reported, then active ones applied again. Operations that price to
	// nothing are not listed.
	Ops []ExpectedOp `json:"ops"`
	// Effects are the cache effects Compute found (breaks, rescues, breakpoint changes), in the
	// order it returns them. Only the library writes these, so no step has an input form of them.
	Effects  []ExpectedEffect   `json:"effects,omitempty"`
	Unpriced []ExpectedUnpriced `json:"unpriced,omitempty"`
}

// ExpectedOp is one priced operation.
type ExpectedOp struct {
	Note     string        `json:"note,omitempty"` // for the reader: replay, break, rescue; never asserted
	ChangeID string        `json:"changeId"`
	Saved    cf.TierTokens `json:"saved"`
	Caused   cf.TierTokens `json:"caused"`
	Call     *cf.ModelCall `json:"call,omitempty"` // a spend: the host prices it
}

// ExpectedEffect is one cache effect. Its range is asserted only when present.
type ExpectedEffect struct {
	Note     string             `json:"note,omitempty"` // for the reader; never asserted
	Kind     cf.CacheEffectKind `json:"kind"`
	ChangeID string             `json:"changeId"`
	From     *cf.Position       `json:"from,omitempty"`
	To       *cf.Position       `json:"to,omitempty"`
	TTL      string             `json:"ttl,omitempty"`
	Saved    cf.TierTokens      `json:"saved"`
	Caused   cf.TierTokens      `json:"caused"`
}

// ExpectedUnpriced is an operation the library must report as not priced, and why.
type ExpectedUnpriced struct {
	OpID   string `json:"opId"`
	Reason string `json:"reason"`
}

// Rate is a price per million tokens, in micro-dollars.
type Rate struct{ Input, CacheWrite5m, CacheWrite1h, CacheRead, Output int64 }

// Rates are the fixed rate tables a case may name (§10.2). With one table, the expected figure and
// the current code's figure can be compared in dollars.
var Rates = map[string]Rate{
	"fixture-v1":     {Input: 3_000_000, CacheWrite5m: 3_750_000, CacheWrite1h: 6_000_000, CacheRead: 300_000, Output: 15_000_000},
	"summarizer-v1":  {Input: 1_000_000, CacheWrite5m: 1_250_000, CacheWrite1h: 2_000_000, CacheRead: 100_000, Output: 5_000_000},
	"small-model-v1": {Input: 800_000, CacheWrite5m: 1_000_000, CacheWrite1h: 1_600_000, CacheRead: 80_000, Output: 4_000_000},
}

// ModelRate says which table prices a model named on a request or on a ModelCall. The agent model
// and the large model are priced by the case's own table.
func (c Case) ModelRate(model string) Rate {
	switch model {
	case "summarizer-model":
		return Rates["summarizer-v1"]
	case "small-model":
		return Rates["small-model-v1"]
	}
	return Rates[c.Rates]
}

// StepRate is the rate table that prices a request step: the model that served the request. A break
// that a model change caused is priced at the rates of the model after the change (design §11
// question 10, proposed rule), so both sides of the break use one table and the figure is the cache
// effect alone. The price difference between the two models is the model change's own saving,
// which the library does not compute (R50).
func (c Case) StepRate(s Step) Rate {
	if s.Begin == nil {
		return c.RateTable()
	}
	return c.ModelRate(s.Begin.Meta.Model)
}

// Picos prices tokens in pico-dollars (1e-12 USD): tokens × (µ$ per million tokens) is exact, so no
// figure is rounded until it is shown (§10.2, "Rounding").
func (r Rate) Picos(t cf.TierTokens) int64 {
	return int64(t.Input)*r.Input + int64(t.CacheWrite5m)*r.CacheWrite5m +
		int64(t.CacheWrite1h)*r.CacheWrite1h + int64(t.CacheRead)*r.CacheRead
}

// UsagePicos prices a billed prompt usage. CacheWrite1h is a part of CacheWrite.
func (r Rate) UsagePicos(u cf.Usage) int64 {
	return r.Picos(cf.TierTokens{Input: u.Input, CacheRead: u.CacheRead,
		CacheWrite5m: u.CacheWrite - u.CacheWrite1h, CacheWrite1h: u.CacheWrite1h})
}

// CallPicos prices everything a call's response bills, output included.
func (r Rate) CallPicos(u cf.CallUsage) int64 {
	return r.UsagePicos(u.Usage) + int64(u.Output)*r.Output
}

// RoundMicros rounds pico-dollars to micro-dollars, half up.
func RoundMicros(p int64) int64 {
	n := p + 500_000
	q := n / 1_000_000
	if n%1_000_000 < 0 {
		q--
	}
	return q
}

// StepPicos is the expected net of one step: Saved minus Caused over its ops, priced with the
// case's rates, minus the cost of every spend, priced with its own model's rates.
func (c Case) StepPicos(s Step) (net int64) {
	for _, op := range s.Expected.Ops {
		net += c.StepRate(s).Picos(op.Saved) - c.StepRate(s).Picos(op.Caused)
		if op.Call != nil {
			net -= c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
		}
	}
	for _, e := range s.Expected.Effects {
		net += c.StepRate(s).Picos(e.Saved) - c.StepRate(s).Picos(e.Caused)
	}
	return net
}

// RateTable is the case's own rate table.
func (c Case) RateTable() Rate { return Rates[c.Rates] }

// SectionTokens returns the tokens of each section of a body. The fixtures use 4 bytes a token.
func SectionTokens(b cf.Body) (tools, system, messages int) {
	for i, p := range b.Pos {
		t := int(p.Bytes) / 4
		switch {
		case i < b.Start[1]:
			tools += t
		case i < b.Start[2]:
			system += t
		default:
			messages += t
		}
	}
	return
}

// Tokens returns the tokens of a whole body.
func Tokens(b cf.Body) int { t, s, m := SectionTokens(b); return t + s + m }

// All returns every case, sorted by ID. It panics on a malformed file, because the list is data
// the package ships and a bad file is a bug in the package.
func All() []Case {
	files, err := fs.Glob(casesFS, "cases/*.json")
	if err != nil {
		panic(err)
	}
	var out []Case
	for _, f := range files {
		b, err := casesFS.ReadFile(f)
		if err != nil {
			panic(err)
		}
		var c Case
		if err := json.Unmarshal(b, &c); err != nil {
			panic(fmt.Sprintf("opscases: %s: %v", f, err))
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Difference marks a case where the current code is meant to give a different figure (§10.2).
type Difference struct {
	Case   string `json:"case"`
	Kind   string `json:"kind"`   // "differs" | "notCovered"
	Reason string `json:"reason"` // cites the spec section that causes it
	// Allow lists exactly where the current code may differ: any difference not listed here fails.
	Allow []Allowed `json:"allow"`
	// Total allows the thread total to differ. It follows from the steps, so it is listed once.
	Total bool `json:"total,omitempty"`
}

// Allowed is one place the current code may differ from the case.
type Allowed struct {
	Step int `json:"step"`
	// Change is the ChangeID whose figure differs. Empty means the step's own figures differ: its
	// net saving and its baseline bill.
	Change string `json:"change,omitempty"`
}

// Differences returns differences.json. A case that is not in it must agree with the current code.
func Differences() map[string]Difference {
	b, err := casesFS.ReadFile("differences.json")
	if err != nil {
		panic(err)
	}
	var list []Difference
	if err := json.Unmarshal(b, &list); err != nil {
		panic(fmt.Sprintf("opscases: differences.json: %v", err))
	}
	out := map[string]Difference{}
	for _, d := range list {
		out[d.Case] = d
	}
	return out
}
