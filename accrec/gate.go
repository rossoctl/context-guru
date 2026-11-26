package accrec

import (
	"strings"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/components"
)

// The reasons a request books no accountant figure (Gate). The dashboard stores them as requests.acc_fallback, and
// the /stats ledger counts the same requests as unpriced (#1441). One decision, so a request the row refuses is
// unpriced on /stats too.
const (
	GateOpsDropped         = "ops_dropped"          // a change could not be placed as an operation
	GateObserve            = "observe"              // the run was observe mode
	GateUnpriced           = "unpriced"             // an operation or effect is unpriced
	GateCheckSum           = "check_sum"            // the library's sum check failed
	GateCheckUniqueChange  = "check_unique_change"  // two operations share a ChangeID
	GateCheckSpans         = "check_spans"          // an operation's spans do not add up to its sizes
	GateCheckOther         = "check_other"          // a fatal library check this host has no name for
	GateSizeUnknown        = "size_unknown"         // TokensBefore is 0: the plausibility bound cannot be checked
	GateImplausibleSize    = "implausible_size"     // the library saved more tokens than the request had
	GateFigureWithoutRow   = "figure_without_row"   // a library figure for a component with no row
	GateDuplicateName      = "duplicate_name"       // two rows share a component name
	GateActedWithoutFigure = "acted_without_figure" // a component acted and the library has no figure for it
)

// The rows a host adds for a figure that no pipeline component owns.
const (
	EndedChangeComponent = "ended_change"
	InteractionComponent = "interaction"
)

// EnvelopeComponents are the operators that change the request envelope (tools, system, cache marks) and may have no
// pipeline row: the proxy reports them from a body diff (accwire/envelope.go). expand, adjudicate and expand_restore
// are never pipeline components; headttl is a host option; toolschema, toolfilter and cachesplit are components whose
// row can be absent when the rewrite ran from an equivalent gate (cacheinject runs the split too).
var EnvelopeComponents = map[string]bool{
	accwire.OpExpand: true, accwire.OpAdjudicate: true, accwire.OpHeadTTL: true, accwire.OpExpandRestore: true,
	"toolschema": true, "toolfilter": true, "cachesplit": true,
	accwire.OpTailBreakpoint: true, accwire.OpToolDefer: true,
}

// toolSectionOperators are the operators whose operations act on the tools section, not on messages. Their tokens are
// not in TokensBefore, which counts message text only. Deferral's names list is a growth in messages and saves
// nothing, so it adds no saved tokens to the bound.
var toolSectionOperators = map[string]bool{
	accwire.Operator("toolfilter"): true, accwire.Operator("toolschema"): true,
	accwire.Operator(accwire.OpExpand): true, accwire.Operator(accwire.OpAdjudicate): true,
	accwire.Operator(accwire.OpToolDefer): true,
}

// CountedSavedTokens is the Saved tokens, all four tiers, of the operations that feed a component figure: not DryRun,
// not a spend, and not an operation on the tools section. TokensBefore is the size of the messages. A tool declaration
// is not in it, so 2,188 removed declaration tokens would look larger than a 121-token conversation and drop the whole
// result (#907). The bound stays for every operation on messages.
func CountedSavedTokens(res accountant.Result) int {
	n := 0
	for _, o := range res.Ops {
		if o.DryRun || o.Call != nil || toolSectionOperators[o.Operator] {
			continue
		}
		n += o.Saved.Input + o.Saved.CacheRead + o.Saved.CacheWrite5m + o.Saved.CacheWrite1h
	}
	return n
}

// GateRow is one component row of the request, as the gate needs it.
type GateRow struct {
	Name  string
	Acted bool
}

// GateRowsOf is the component rows of a run, as the dashboard row builds them: a component acted when it was neither
// reverted nor skipped and saved tokens.
func GateRowsOf(run *components.RunReport) []GateRow {
	if run == nil {
		return nil
	}
	rows := make([]GateRow, 0, len(run.Components))
	for _, r := range run.Components {
		rows = append(rows, GateRow{Name: r.Component, Acted: !r.Reverted && !r.Skipped && r.Saved() > 0})
	}
	return rows
}

// GateIn is what the gate needs besides the Result and the library's figure.
type GateIn struct {
	Observe    bool // the run was observe mode
	OpsDropped int  // changes that could not be placed as operations
	// TokensBefore is the size of the request's messages, in tokens: the plausibility bound. 0 or less: unknown.
	TokensBefore int
	Rows         []GateRow
}

// ComponentFigure maps the library's ByComponent, ByComponentFirst and ByComponentReplay to the host's component names
// (the operator prefix off). An operator with no row and 0 tokens in all of its operations is left out: it would get
// an envelope row of $0. A name with a row keeps its key at $0: a component that acted can have counted operations of
// 0 tokens (the byte ratio truncates), and without its key the whole request's saving would be dropped. The mark
// premium no operation owns goes to the tail breakpoint row, which adds the mark in every default preset.
func ComponentFigure(res accountant.Result, fig accountant.USDFigure, rows []GateRow) (by, first, replay map[string]float64) {
	hasRow := map[string]bool{}
	for _, r := range rows {
		hasRow[r.Name] = true
	}
	zero := map[string]bool{}
	for _, o := range res.Ops {
		if !o.DryRun && o.Call == nil {
			if n := strings.TrimPrefix(o.Operator, accwire.OperatorPrefix); !hasRow[n] {
				zero[n] = true
			}
		}
	}
	for _, o := range res.Ops {
		if !o.DryRun && o.Call == nil && !(o.Saved.IsZero() && o.Caused.IsZero()) {
			delete(zero, strings.TrimPrefix(o.Operator, accwire.OperatorPrefix))
		}
	}
	cut := func(m map[string]float64) map[string]float64 {
		to := map[string]float64{}
		for k, v := range m {
			if zero[strings.TrimPrefix(k, accwire.OperatorPrefix)] && v == 0 {
				continue
			}
			to[strings.TrimPrefix(k, accwire.OperatorPrefix)] += v
		}
		return to
	}
	by, first, replay = cut(fig.ByComponent), cut(fig.ByComponentFirst), cut(fig.ByComponentReplay)
	if fig.UnownedMarkUSD != 0 {
		by[accwire.OpTailBreakpoint] += fig.UnownedMarkUSD
	}
	return by, first, replay
}

// Gate says why a request books no accountant figure, or "" when it does. detail is the library's "opID:reason" for
// GateUnpriced. fig is the library's own figure for res. The gates, in order: a change dropped, an observe run, a fatal
// library check, an unpriced operation or effect (penalty_unknown does not gate: see Held), the size bound
// (TokensBefore unknown, or more tokens saved than the request had), then the rows: a figure for a component with no
// row, a row that the host adds for the ended change or the interaction when a component has that name already, two
// rows with one name, and a component that acted with no figure.
//
// It is the one decision the dashboard row and the /stats ledger share. It reads no price.
func Gate(res accountant.Result, fig accountant.USDFigure, in GateIn) (reason, detail string) {
	switch {
	case in.OpsDropped != 0:
		return GateOpsDropped, ""
	case in.Observe:
		return GateObserve, ""
	}
	for _, c := range res.Checks {
		if c.Fatal() {
			return fatalCheckReason(c.Check), ""
		}
	}
	for _, u := range fig.Unpriced {
		if !strings.HasSuffix(u, ":"+accountant.ReasonPenaltyUnknown) {
			// "opID:reason". A request-level refusal has no operation, so its id is empty: drop the colon.
			return GateUnpriced, strings.TrimPrefix(u, ":")
		}
	}
	if in.TokensBefore <= 0 {
		return GateSizeUnknown, ""
	}
	if CountedSavedTokens(res) > in.TokensBefore {
		return GateImplausibleSize, ""
	}
	rows := map[string]bool{}
	for _, r := range in.Rows {
		rows[r.Name] = true
	}
	by, _, _ := ComponentFigure(res, fig, in.Rows)
	for name := range by {
		if !rows[name] && !EnvelopeComponents[name] {
			return GateFigureWithoutRow, ""
		}
	}
	if fig.TTLUSD != 0 {
		by[accwire.OpHeadTTL] += fig.TTLUSD
	}
	if fig.EndedBreakUSD+fig.UnownedReRateUSD != 0 {
		if rows[EndedChangeComponent] {
			return GateDuplicateName, ""
		}
		by[EndedChangeComponent] = 1
	}
	if fig.InteractionUSD != 0 && (res.Interaction != (accountant.TierTokens{}) || res.InteractionCaused != (accountant.TierTokens{})) {
		if rows[InteractionComponent] {
			return GateDuplicateName, ""
		}
		by[InteractionComponent] = 1
	}
	seen := map[string]bool{}
	for _, r := range in.Rows {
		if _, ok := by[r.Name]; ok {
			if seen[r.Name] {
				return GateDuplicateName, ""
			}
			seen[r.Name] = true
		}
	}
	for _, r := range in.Rows {
		if _, ok := by[r.Name]; !ok && r.Acted {
			return GateActedWithoutFigure, ""
		}
	}
	return "", ""
}

// fatalCheckReason names a fatal library check. The library decides which checks are fatal. A fatal check this switch
// does not know gates all the same, as GateCheckOther, so a check the library adds later never passes silently.
func fatalCheckReason(check string) string {
	switch check {
	case accountant.CheckSum:
		return GateCheckSum
	case accountant.CheckUniqueChange:
		return GateCheckUniqueChange
	case accountant.CheckSpans:
		return GateCheckSpans
	}
	return GateCheckOther
}
