package counterfactual

// ReasonNotImplemented marks an operation the library has not priced because Compute is a stub.
const ReasonNotImplemented = "not_implemented"

// Compute prices the request and then replaces the thread's ThreadState (§6.1). It is
// deterministic and does no I/O (R26). If it fails or panics, the ThreadState is unchanged.
//
// NOT IMPLEMENTED. Until the tier rule (§6.2), the comparison (§5) and the checks (§6.5) are
// written, Compute prices nothing: it reports every operation as unpriced and leaves the
// ThreadState alone. The spec's open questions 11 to 13 decide what the state holds and how a
// change ends, and they block the real calculation.
func (r *Request) Compute(usage Usage) Result { return r.stub() }

// Preview does the same calculation as Compute and stores nothing (§6.1).
//
// NOT IMPLEMENTED: see Compute.
func (r *Request) Preview(usage Usage) Result { return r.stub() }

// ComputeStandalone prices a RequestOps that holds only spend operations (a keep-alive ping or a
// detached call). It reads and changes no ThreadState (§6.1).
//
// NOT IMPLEMENTED: see Compute.
func ComputeStandalone(ops RequestOps) Result {
	res := Result{Version: 1, Phase: PhaseUnknown}
	for _, op := range ops.Ops {
		res.Ops = append(res.Ops, PricedOp{OpID: op.ID, Operator: op.Operator, ChangeID: op.ChangeID, DryRun: op.DryRun, Call: op.Call})
		res.Unpriced = append(res.Unpriced, Unpriced{OpID: op.ID, Reason: ReasonNotImplemented})
	}
	return res
}

func (r *Request) stub() Result {
	res := Result{Version: 1, Phase: PhaseUnknown}
	if !r.ended {
		res.Unpriced = append(res.Unpriced, Unpriced{Reason: "end_request_missing"})
	}
	for _, op := range r.Ops {
		res.Ops = append(res.Ops, PricedOp{OpID: op.ID, Operator: op.Operator, ChangeID: op.ChangeID, DryRun: op.DryRun, Call: op.Call})
		res.Unpriced = append(res.Unpriced, Unpriced{OpID: op.ID, Reason: ReasonNotImplemented})
	}
	return res
}
