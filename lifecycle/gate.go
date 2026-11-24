package lifecycle

import "github.com/rossoctl/context-guru/components"

// Gate values CompactionGate reports.
const (
	GateDetected       = "detected"
	GateOversizeBypass = "oversize_bypass"
)

// CompactionGate decides what the agent's own compaction request does to the bypass flag.
//
// Only the bypass header (headerBypass) bypasses. The agent's compaction request goes through the
// pipeline like any other request, so the provider cache stays warm. The one exception is size: a
// compaction body longer than limit bytes is bypassed.
//
// gate is "" when the request is not a compaction, or when headerBypass already bypassed it.
// Otherwise it is GateDetected or GateOversizeBypass, and the host reports it.
func CompactionGate(headerBypass, agentCompaction bool, bodyLen int, limit int64) (bypassed bool, gate string) {
	bypassed = headerBypass
	if bypassed || !agentCompaction {
		return bypassed, ""
	}
	if int64(bodyLen) > limit {
		return true, GateOversizeBypass
	}
	return false, GateDetected
}

// Observing reports whether this request is in observe mode: the pipeline runs off-path on a copy,
// and the forwarded body is the original. A bypassed request is never observed.
func Observing(mode components.Mode, bypassed bool) bool {
	return mode == components.ModeObserve && !bypassed
}

// Enforcing reports whether the host rewrites this request at all. It is the gate of expand
// repair, the fixed-restore anchor and every tool injection.
func Enforcing(mode components.Mode, bypassed bool) bool {
	return mode != components.ModeObserve && !bypassed
}
