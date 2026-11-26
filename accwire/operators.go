package accwire

// OperatorPrefix is the host prefix of every operator name context-guru books. The accountant's
// metrics sink cuts it off, so the `component` label is the bare name.
const OperatorPrefix = "context-guru."

// The operators that book savings but are not registered components (components.Names()). Each
// emit site uses its constant, so OperatorsOutsideRegistry cannot miss a name the code books.
const (
	// OpKeeper is accountant.PingOps's operator: the keep-alive ping's spend. It has no host prefix.
	// The library exports no constant for it; TestOpKeeperIsThePingOperator reads it from PingOps.
	OpKeeper = "keeper"
	// OpHeadTTL is the 1h head TTL, a host option (apply/headttl.go, envelopeOps).
	OpHeadTTL = "headttl"
	// OpTailBreakpoint is the trailing cache mark apply adds (apply/ops.go).
	OpTailBreakpoint = "tailbreakpoint"
	// OpToolDefer is tool deferral (accrec's billed-body pass, the proxy's MessageOps).
	OpToolDefer = "tooldefer"
	// OpExpand and OpAdjudicate are the tool declarations expand and adjudicate inject (envelopeOwner).
	OpExpand     = "expand"
	OpAdjudicate = "adjudicate"
	// OpExpandRestore is the copy fixed_restore inserts (apply/restore.go, #481).
	OpExpandRestore = "expand_restore"
)

// Operator returns the booked operator name of name: the host prefix and the bare name.
func Operator(name string) string { return OperatorPrefix + name }

// OperatorsOutsideRegistry lists every operator that books savings and is not a registered
// component. A metrics sink built with WithComponents(components.Names()...) pushes these as
// component="other"; accrec.MetricComponents adds them.
func OperatorsOutsideRegistry() []string {
	return []string{OpKeeper, OpHeadTTL, OpTailBreakpoint, OpToolDefer, OpExpand, OpAdjudicate, OpExpandRestore}
}
