package keepalive

// WindowsInput is what ResolveWithWindows reads. The host fills the callbacks from its own state
// (the tracker, the request's usage and body), so each is called lazily and only where the
// resolver needs it.
type WindowsInput struct {
	// Account is the account's own policy.
	Account Policy
	// Tracked is true when the response is one the keeper would consider (2xx, a body, a start).
	Tracked bool
	// OpenAI is true for an OpenAI-family request.
	OpenAI bool
	// Subagent is true for a subagent thread.
	Subagent bool
	// Open is the thread's open background tasks and teammate waits, or nil.
	Open *TaskCounts
	// Prefix is the billed prefix of the response: cache read plus cache write.
	Prefix int64
	// CauseOf is the wait cause of the response, given the open counts (nil for the tracker-only
	// cause).
	CauseOf func(*TaskCounts) string
	// OneHourTier reports whether this response is on the 1-hour tier (Keeper.OneHourTier).
	OneHourTier func() bool
	// Resolve is the policy chain for one reading of the windows (Resolve with the host's
	// strategies, campaign and override). ignoreWindows ignores a strategy's time windows.
	Resolve func(pol Policy, ignoreWindows bool) (Policy, string)
}

// ResolveWithWindows resolves the policy of one finished response with the wait cause in hand.
//
// Time windows gate only a wait for a person (#858), so the strategy is resolved with the wait
// cause of the response that ended this turn. The body reading joins only where the per-wait plan
// changes the count (the 5-minute tier, not OpenAI, Idle under 300 s: Record's own guard), for the
// windows as for the plan (#1227). Elsewhere the tracker-only cause decides.
//
// Ignoring the windows must never refuse a hold today's decision makes, nor plan fewer pings.
// Today's policy is the one the windows give; the wide one is the policy with them ignored. When
// the two readings differ, the one that holds the wait, or plans more pings, wins.
//
// It returns the policy, the applied strategy tag, and the open counts Record must use (nil when
// the reading did not apply).
func ResolveWithWindows(in WindowsInput) (Policy, string, *TaskCounts) {
	open := in.Open
	planEligible := func(p Policy) bool { return p.WaitPlan && p.On() && p.Idle < OneHourIdleFloor }
	ignoreWindows, ignoreToday := false, false
	if in.Account.WaitPlan && in.Tracked {
		ignoreToday = !WindowsApply(in.CauseOf(nil), in.Subagent)
		ignoreWindows = ignoreToday
		if open != nil && !in.OpenAI && !in.OneHourTier() {
			ignoreWindows = !WindowsApply(in.CauseOf(open), in.Subagent)
		}
	}
	pol, applied := in.Resolve(in.Account, ignoreWindows)
	if ignoreWindows == ignoreToday {
		return pol, applied, open
	}
	todayPol, todayApplied := in.Resolve(in.Account, ignoreToday)
	// The wait cause a policy is judged by is the one Record will use for it: the merged cause where
	// the policy takes the per-wait plan, the tracker-only cause where it does not (a strategy on
	// the 1-hour band keeps its own count and the general floor). Whether a policy HOLDS the wait
	// does not depend on its plan eligibility.
	causeFor := func(p Policy) string {
		if planEligible(p) {
			return in.CauseOf(open)
		}
		return in.CauseOf(nil)
	}
	holds := func(p Policy) bool {
		return p.On() && in.Prefix >= int64(p.WaitMinPrefix(causeFor(p), in.Subagent))
	}
	pingsOf := func(p Policy) int {
		if planEligible(p) {
			return p.WaitPings(causeFor(p), in.Subagent)
		}
		return p.MaxPings
	}
	switch {
	case !planEligible(pol):
		// The policy that ignoring the windows chose does not take the plan (a 1-hour Idle, or
		// off): the reading does not apply, and today's windows decision stands.
		return todayPol, todayApplied, nil
	case holds(todayPol) && (!holds(pol) || pingsOf(todayPol) > pingsOf(pol)):
		// Today's policy holds this wait and the strategy's would refuse it or plan fewer pings.
		return todayPol, todayApplied, open
	}
	return pol, applied, open
}
