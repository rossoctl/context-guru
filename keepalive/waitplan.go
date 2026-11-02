package keepalive

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/waitcause"
)

// The per-wait ping plan (#858). The ping count is chosen when a turn ends, from what the thread
// waits for. Metadata only: stop reason, tool names, the open-task counts and the shape of the
// thread id. "Base" is Policy.MaxPings.
//
//	own tool (main or subagent)               keep pinging until the result comes, at most 8
//	main, background work                     base + 3
//	main, a person                            base + 1
//	main, a teammate                          base
//	subagent, its main agent                  base
//	done, or no tool call yet on the thread   0
//
// The plan applies to the 5-minute tier only. A 1-hour thread keeps its own schedule
// (Policy.ForOneHour), whose credential hold is already bounded at (K+1) x 59 min.
const (
	// DefaultToolWaitMaxPings is the most pings a tool wait gets. A tool almost never runs longer
	// than 13 minutes (0 of 805 main waits locally, 5 of 5,202 on the hosted proxy), so in practice
	// the count stays near the base. Mirrored by config.DefaultKeepAliveToolWaitMaxPings.
	DefaultToolWaitMaxPings = 8
	// DefaultBackgroundExtraPings and DefaultHumanExtraPings are added to the base. Mirrored by
	// config.DefaultKeepAliveBackgroundExtraPings and DefaultKeepAliveHumanExtraPings.
	DefaultBackgroundExtraPings = 3
	DefaultHumanExtraPings      = 1
	// MaxWaitPings bounds any plan result unless the base itself is higher. It equals the highest
	// strategy ping count, so the credential hold stays at (11+1) x 299 s = 60 min or less.
	MaxWaitPings = 11
)

// IsSubagentThread reports whether a thread id names a subagent. The main thread is "".
func IsSubagentThread(threadID string) bool { return strings.HasPrefix(threadID, "a:") }

// WindowsApply reports whether a manager strategy's time windows apply to a thread that waits for
// cause. Only a wait for a person follows working hours. A tool, a background task, a teammate and
// a main agent answer at night as fast as in the day. A subagent's "human" wait (end_turn, or the
// SubagentHandback tool) is a wait for its main agent, not for a person.
func WindowsApply(cause string, subagent bool) bool {
	switch cause {
	case waitcause.WaitingOwnTool, waitcause.WaitingBackground, waitcause.WaitingTeammate:
		return false
	case waitcause.WaitingHuman:
		return !subagent
	}
	return true
}

// WaitPings is the ping count for a thread that waits for cause. With WaitPlan off it is MaxPings.
func (p Policy) WaitPings(cause string, subagent bool) int {
	base := p.MaxPings
	if !p.WaitPlan {
		return base
	}
	n := base
	switch cause {
	case waitcause.WaitingOwnTool:
		n = max(base, orDefault(p.ToolWaitMaxPings, DefaultToolWaitMaxPings))
	case waitcause.WaitingBackground:
		if !subagent {
			n = base + orDefault(p.BackgroundExtraPings, DefaultBackgroundExtraPings)
		}
	case waitcause.WaitingHuman:
		if !subagent {
			n = base + orDefault(p.HumanExtraPings, DefaultHumanExtraPings)
		}
	}
	return min(n, max(base, MaxWaitPings))
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// WaitingFor is the wait cause of a response, from its tool names and stop reason and the thread's
// open-task counts (Options.OpenTasks). The open counts are as of the last request the host's
// tracker observed, which can lag by one request: a stale count reads as "a person", the cheaper plan.
func (k *Keeper) WaitingFor(key Key, names []string, incomplete bool, stopReason string, usageOK bool) string {
	if k == nil {
		return waitcause.WaitingUnknown
	}
	bg, tm := 0, 0
	if k.o.OpenTasks != nil {
		bg, tm = k.o.OpenTasks(key.Tenant, key.Session, key.Thread)
	}
	w, _ := waitcause.ClassifyWaitingForTerminal(terminalFor(k.o.TerminalTools, incomplete),
		names, stopReason, false, usageOK, bg, tm)
	// A response that stopped for a tool call waits for that tool, even when the name list missed
	// the call (a spliced sniff window): the classifier reads such a response as a wait for a person.
	// Whatever the classifier said (open background tasks read "background"), a tool stop gets the tool budget.
	return toolStopWait(w, len(names) == 0, stopReason)
}

// toolStopWait applies the tool-stop rule: a response that stopped for a tool call and has no tool
// name waits for that tool, whatever the classifier said, unless the thread is done or the call failed.
func toolStopWait(w string, noNames bool, stopReason string) string {
	if noNames && toolStop(stopReason) && w != waitcause.WaitingDone && w != waitcause.WaitingRetry {
		return waitcause.WaitingOwnTool
	}
	return w
}

// ToolStopReasons are the stop reasons that mean "the response called a tool". The dashboard reads
// the same list when it looks for the first tool call of a thread in its stored rows.
var ToolStopReasons = []string{"tool_use", "tool_calls", "function_call"}

func toolStop(stopReason string) bool {
	for _, s := range ToolStopReasons {
		if s == stopReason {
			return true
		}
	}
	return false
}

// SpanFacts is what a stored request row says about the idle span that follows it: the facts the
// plan needs. The dashboard replays the plan from them (Policy.SpanPings).
type SpanFacts struct {
	// Cause is the row's stored waiting_for ("" on an old row).
	Cause string
	// StopReason is the row's stored stop reason ("" on an old row).
	StopReason string
	// Subagent is true for a subagent thread (IsSubagentThread).
	Subagent bool
	// ToolSeen is true when this response or an earlier one of the thread called a tool.
	ToolSeen bool
	// OpenAI is true for an OpenAI request. The plan does not cover OpenAI: it keeps the base count.
	OpenAI bool
	// OneHour is true when the request asked for the 1-hour cache tier. That tier keeps its own
	// schedule: the base count.
	OneHour bool
}

// storedWait is the wait cause the keeper would plan for, from a stored row. A stored row has no
// tool names, so the keeper's "tool stop with no name" rule is read from the cause: a tool stop that
// the classifier read as background work or a teammate waits for its tool. A tool stop stored as
// "human" stays human, because a row cannot tell an ask-a-person tool from a name list that missed
// the call (the keeper reads the second as a tool wait). That case is rare.
func storedWait(cause, stopReason string) string {
	return toolStopWait(cause, cause != waitcause.WaitingHuman && cause != waitcause.WaitingOwnTool, stopReason)
}

// SpanPings is the ping count the plan gives the idle span after a response, from the facts a stored
// row holds. It is the keeper's decision in one place, for a replay:
//
//   - the count WaitPings gives for the wait cause;
//   - 0 for a thread with no tool call yet (the no-tool rule);
//   - 0 for a finished thread (waiting_for done) and for a failed call (retry): the keeper holds nothing for them;
//   - MaxPings with WaitPlan off, for OpenAI, for the 1-hour tier, and for a row that stored neither a
//     wait cause nor a stop reason (an old row: nothing to replay from).
//
// TestDashboardReplayMatchesTheKeeper (package dash) runs the keeper and this function on the same cases.
func (p Policy) SpanPings(s SpanFacts) int {
	if !p.WaitPlan || s.OpenAI || (s.Cause == "" && s.StopReason == "") {
		return p.MaxPings
	}
	w := storedWait(s.Cause, s.StopReason)
	if w == waitcause.WaitingDone || w == waitcause.WaitingRetry || !s.ToolSeen {
		return 0
	}
	if s.OneHour {
		return p.MaxPings
	}
	return p.WaitPings(w, s.Subagent)
}

// calledATool reports whether a response called a tool: a tool_use name or a tool stop reason. A
// name list that may be incomplete (a spliced sniff window) counts as a call: the thread is then pinged as
// before #858.
func calledATool(names []string, incomplete bool, stopReason string) bool {
	return len(names) > 0 || incomplete || toolStop(stopReason)
}

// toolWait reports whether this entry's pings were planned for a wait on the thread's own tool.
func (e *kaEntry) toolWait() bool { return e.wait == waitcause.WaitingOwnTool }

// ToolSeen reports whether a response of the thread has called a tool (#858). Cheap: one map read under
// the keeper lock. The host uses it to keep nothing for a thread that the plan will not ping.
func (k *Keeper) ToolSeen(key Key) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.toolSeen[key.id()]
}
