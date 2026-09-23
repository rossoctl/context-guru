package offload

import (
	"encoding/json"
	"strconv"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// THE WASTE THIS EXISTS TO STOP, measured rather than supposed: on one probe pass, seven of ten
// adjudications were the SAME five candidates at the same 41,453 tokens, offered once per turn, kept
// every time. The component paid sonnet to answer a question it had already had answered, seven times,
// and nothing in the design noticed — drops are frozen and replayed through cg:res:, and keeps were
// simply forgotten.
//
// A KEEP MUST EXPIRE, and this is the part that makes the cache safe rather than merely cheap. "Still
// needed" is a judgement about the CURRENT state of the conversation's obligations, not a property of
// the content. Cache it permanently and a candidate judged needed once is never offered again — which
// silently disables the component for exactly the outputs it most wants to reach, and would read in
// the counters as an inventory that quietly shrank. The failure mode of over-caching here is worse than
// the cost of re-asking, so expiry is not optional.
//
// WHAT IT EXPIRES ON is model turns, not seconds. What changes whether an output is spent is the agent
// doing more work: taking steps, completing obligations, superseding results. Wall-clock says nothing
// about that — a session idle for an hour has the same obligations it had at the start. So a keep is
// recorded with the candidate's `later_turns` at the time, and the question becomes askable again once
// that number has grown by `recheck` turns. On the measured pass, a recheck of 4 turns would have taken
// those seven asks down to two while preserving every opportunity to change the verdict.
type sweepKeep struct {
	// LaterTurns is how many model turns followed the output when the keep was recorded. Compared
	// against the same measure on a later turn, so the two are the same coordinate by construction.
	LaterTurns int `json:"lt"`
}

func sweepKeepKey(session, id string) string {
	return store.SweepKeepPrefix + session + ":" + id
}

// recordSweepKeep notes that the adjudicator judged this content still needed at this point in the
// conversation. Best effort: a store that refuses simply means the question gets asked again, which is
// the pre-existing behaviour and never wrong, only dearer.
func recordSweepKeep(c *components.Ctx, id string, laterTurns int) {
	if c == nil || c.Store == nil || id == "" {
		return
	}
	if b, err := json.Marshal(sweepKeep{LaterTurns: laterTurns}); err == nil {
		c.Store.Put(sweepKeepKey(c.Session, id), b)
	}
}

// sweepKeepStillHolds reports whether a previous keep should suppress re-offering this candidate, and
// how many turns have passed since that keep — returned so a counter can say how stale the suppressed
// judgement was rather than merely that one existed.
//
// recheck <= 0 disables the cache entirely: every candidate is offered every turn, which is the
// behaviour every iteration up to 027 had.
func sweepKeepStillHolds(c *components.Ctx, id string, laterTurns, recheck int) (held bool, since int) {
	// The recheck <= 0 test is an OPTIMISATION, not the guard: with recheck 0 the arithmetic below
	// already answers "not held" for any growing transcript, so removing this line changes no outcome —
	// only whether a store read happens on every candidate of every request. Stated because a mutation
	// run showed it to be behaviour-equivalent, which is exactly the kind of line someone later deletes
	// as dead. The contract is pinned by TestKeepCacheIsInertWhenUnconfigured either way.
	if recheck <= 0 || c == nil || c.Store == nil || id == "" {
		return false, 0
	}
	b, ok := c.Store.Get(sweepKeepKey(c.Session, id))
	if !ok {
		return false, 0
	}
	var k sweepKeep
	if json.Unmarshal(b, &k) != nil {
		return false, 0 // unreadable => ask again; never suppress on a record we cannot read
	}
	grown := laterTurns - k.LaterTurns
	if grown < 0 {
		// The transcript got SHORTER behind this output — an agent-side compaction or a rewind. The
		// coordinate is no longer comparable, so the stored judgement says nothing about now. Ask.
		return false, 0
	}
	if grown >= recheck {
		return false, grown
	}
	return true, grown
}

// sweepKeepRecheckLabel renders the recheck interval for a gate name's detail, kept here so the two
// places that mention it cannot drift.
func sweepKeepRecheckLabel(recheck int) string { return strconv.Itoa(recheck) }
