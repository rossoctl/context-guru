package keepalive

// MayHoldCredentials answers who may keep a caller's credential in memory for pings.
//
// The "no audit sink, no retention" rule applies only to a hosted, multi-tenant host, where the
// process holds other people's credentials: there, a deployment with no audit sink holds nothing.
// A single-tenant host holds only its own user's credential, in the user's own process, and needs
// no sink. A library host (Cortex, bifrost) never asks: its dispatcher resolves the credential.
//
// A credential held this way lives in process memory only. No caller of this function may write
// it to a store, a file, a log or a metric.
//
// hosted is a function so that a failure while reading the signal can be caught here. The rule
// fails closed to the old behavior: when hosted panics, the answer is hasAuditSink, which is what
// every host answered before this rule was narrowed.
func MayHoldCredentials(hosted func() bool, hasAuditSink bool) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = hasAuditSink
		}
	}()
	if hasAuditSink {
		return true
	}
	return !hosted()
}
