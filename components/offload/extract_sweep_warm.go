package offload

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/schema"
)

// THE WARM PATH (#491).
//
// The cold apply (extract_sweep_apply.go) removes old outputs only when the cache is gone anyway. A
// session that never goes idle never has such a request. The warm path gives it a way, behind an
// economic gate, and it is OFF by default and in no preset.
//
//	WHEN to ask   a rule: on a warm request, at most once every warm_every_n requests of a thread, and
//	              only when old outputs could pay back a rewrite even if every one were dropped.
//	WHAT to drop  the model: the same question as the ping's, with a file clause for file reads.
//	FROM WHERE   the ping registry's existing copy of the conversation. The warm path keeps no copy of
//	              its own, so it asks only on threads the keep-alive can ping (the 80k floor applies).
//	WHERE it runs AFTER the request is forwarded, in the background. The agent sees no added delay.
//	WHERE it waits IN PROCESS MEMORY ONLY (warmThreads). The owner's rule: the waiting answer is never
//	              written to the Store. A restart loses it, and that fails open: nothing is dropped.
//	WHETHER to apply   on a later warm request: recheck each verdict as the cold apply does, then the
//	              gate: saving > rewrite, in BILLED sizes. A refusal DROPS the answer.
//
// An answer that passes is applied by the cold apply's own code (applyHeldFrom): the same markers, stash
// and drop records, so `expand` gives each output back byte for byte and later requests replay the same
// bytes. Those records are metadata, and the stash is the same content stash every offloader writes.
// Only the WAITING answer is memory-only.

const (
	defaultWarmEveryN      = 30
	defaultWarmMinAgeTurns = 30
	// defaultWarmMaxTurns is twice defaultWarmEveryN. A thread's warm run is the best guide to how many
	// warm turns are left, but a long run is weak evidence past about two question intervals, and a
	// larger cap would let one long run excuse a very large rewrite. It is a setting.
	defaultWarmMaxTurns = 60
	// defaultWarmMinMassFrac: ask only when the removable candidate mass is at least this share of the
	// conversation. On recorded sessions one sweep paid only when (removed x turns left) >= 11.5 x
	// (conversation - removed), and the best single sweep fell near 40% of the session. A sweep of under
	// a fifth of the conversation almost never covers the rewrite it forces.
	defaultWarmMinMassFrac = 0.20

	// warmLookbackBlocks is how far back the provider looks for an earlier cache entry: about 20 content
	// blocks before a cache breakpoint. A drop FARTHER back than that from the previous request's last
	// breakpoint cannot reach any entry written after the fixed part (system prompt and tools), so the
	// provider reads back only that fixed part and writes the whole conversation again. In 6,968 recorded
	// Opus calls every large cache drop read back exactly the fixed size and nothing partial. A drop
	// WITHIN the look-back still finds the entry just before it, so only the tail from the drop is
	// written again.
	warmLookbackBlocks = 20

	// warmHeldMaxAge is how long a waiting answer lives. Past it the answer is dropped, not applied: the
	// conversation has moved on. It matches the ping registry's bound.
	warmHeldMaxAge = 30 * time.Minute

	// maxCarriedDrops caps how many drop verdicts a thread carries forward between questions (#491). The
	// questions cover at most 12 x max_ask_batches = 96 outputs each, and a carried set is merged by tool id, so
	// it cannot grow past the number of old outputs the conversation holds; the cap is a bound on memory, not a
	// policy. The OLDEST verdicts are the ones that go (they are the likeliest to be stale).
	maxCarriedDrops = 256
	maxWarmThreads  = 256
	// maxBilledRatio bounds the billed-over-local ratio the gate trusts. A figure outside [1, max] came
	// from another thread's turn or a bad sample, and the gate refuses instead of acting on it.
	maxBilledRatio = 8.0
)

// Wait-cause weights on the expected warm turns (#453). A thread whose last wake was its own tool's
// result is in a tight loop and comes back warm soon. One that was woken by a human message may pause.
const (
	warmWeightOwnTool = 1.0
	warmWeightHuman   = 0.5
	warmWeightUnknown = 0.75
)

// warmPendingMax is how long a trigger waits for the host's hook before it is forgotten, so a host that
// never calls ResolveWarmQuestion cannot leave a thread waiting for ever. It holds a time, not a copy of
// the conversation. A var so a test can shrink it.
var warmPendingMax = 5 * time.Minute

// warmThread is one thread's warm state. All in memory.
type warmThread struct {
	// count is warm requests since the last check; run is warm requests since the last cold one.
	count, run int
	// lastLocal is the local token count of the previous request, the other half of the billed ratio.
	lastLocal int
	// Growth of the billed prompt per warm turn, as METADATA (three numbers): the billed size at the start
	// of the current growth run, the turns since then, and the last billed size seen. A shrink (a client
	// compaction, our own sweep, a rewind) restarts the run, so the average is over the turns since.
	growthFrom, growthTurns, growthLast int
	touched                             time.Time
	// asking is set from the registration until the question ends.
	asking bool
	// pendingAt is when the trigger fired; zero = nothing pending. NO conversation copy is kept for the
	// warm path: the question runs from the ping registry's existing copy (option B, #491).
	pendingAt time.Time
	held      *heldAnswer
	heldAt    time.Time
	// heldTried is set when the gate REFUSED the held answer and its survivors were carried forward. A carried
	// set is not retried on every warm request (it would be refused again on nearly the same numbers); the
	// trigger runs instead, asks a new question, and the new answer is merged in (warmHold clears this).
	heldTried bool
}

var (
	warmMu      sync.Mutex
	warmThreads = map[string]*warmThread{}
)

func warmFor(key string) *warmThread {
	t, ok := warmThreads[key]
	if ok {
		t.touched = time.Now()
		return t
	}
	if len(warmThreads) >= maxWarmThreads {
		// Evict the least recently touched thread that has NOTHING to lose: no waiting answer, no
		// pending trigger, no running question. If every thread has something, refuse the new one and
		// count it. The map never passes maxWarmThreads.
		oldestKey, oldest := "", time.Time{}
		for k, v := range warmThreads {
			if v.warmPending() || v.asking || v.held != nil {
				continue
			}
			if oldestKey == "" || v.touched.Before(oldest) {
				oldestKey, oldest = k, v.touched
			}
		}
		if oldestKey == "" {
			sweepWarmThreadsFull.Add(1)
			return nil
		}
		delete(warmThreads, oldestKey)
	}
	t = &warmThread{touched: time.Now()}
	warmThreads[key] = t
	return t
}

// warmTriggerHook, set only by tests, runs at the top of the trigger so a test can make it panic and
// prove the request still goes out unchanged.
var warmTriggerHook func()

// warmShrinkHook, set only by tests, runs after the shrink check has read the held answer and before it takes
// the lock to void it, as a question landing in between would.
var warmShrinkHook func()

// warmAskSpy, set only by tests, sees the candidate a warm question is about to use.
var warmAskSpy func(*sweepPingCandidate)

// Counters.
var (
	sweepWarmAsked, sweepWarmHeld, sweepWarmFailed, sweepWarmFired           atomic.Int64
	sweepWarmNotWorth, sweepWarmNoBilled, sweepWarmThreadBusy                atomic.Int64
	sweepWarmModelDeclined, sweepWarmHeldExpired, sweepWarmPanics            atomic.Int64
	sweepWarmHeldShrunk                                                      atomic.Int64
	sweepWarmCarriedMerged, sweepWarmCarriedOverridden                       atomic.Int64
	sweepWarmCarriedRefused, sweepWarmCarriedExpired, sweepWarmCarriedCapped atomic.Int64
	sweepWarmNotRegistered, sweepWarmThreadsFull                             atomic.Int64
	sweepWarmAlreadyAsked, sweepWarmNoInventory                              atomic.Int64
	sweepWarmPriceModelUnknown                                               atomic.Int64
	sweepWarmGateAccept, sweepWarmGateRefuse, sweepWarmConcurrency           atomic.Int64
	sweepWarmGateMu                                                          sync.Mutex
	sweepWarmTriggerReasons                                                  = map[string]int64{}
	sweepWarmGateReasons                                                     = map[string]int64{}
)

// SweepWarmStats is the warm path's ledger for /stats.
type SweepWarmStats struct {
	// Fired: questions the trigger registered. Asked: of those, questions that reached the provider.
	// Held: answers held since start (cumulative, not "now waiting"). Failed: questions with no usable answer.
	Fired, Asked, Held, Failed int64
	// NotWorth: checks where the candidates could not pay back. NoBilled: checks skipped because the
	// provider's billed size was not known yet. ThreadBusy, ModelDeclined, ConcurrencyFull: questions
	// not sent, for the same reasons as at the ping. HeldExpired: waiting answers dropped for age.
	NotWorth, NoBilled, ThreadBusy, ModelDeclined, ConcurrencyFull, HeldExpired, HeldShrunk, CarriedMerged, CarriedOverridden, CarriedRefused, CarriedExpired, CarriedCapped, NotRegistered, ThreadsFull, AlreadyAsked, NoInventory, PriceModelUnknown int64
	// GatePriceModel is the diagnostic warm_gate_price_model, "" in a normal run.
	GatePriceModel string
	// GateAccept and GateRefuse count the economic gate's verdicts; Reasons splits the refusals.
	GateAccept, GateRefuse int64
	Reasons                map[string]int64
	// TriggerReasons counts the refusals at the trigger (before a question), by reason.
	TriggerReasons map[string]int64
	// Recent lists the last warm sweeps applied, with the gate's numbers.
	Recent []WarmSweep
	// RecentDecisions lists the last trigger and apply-gate decisions, accepted or refused, with the
	// numbers they were made on.
	RecentDecisions []WarmDecision
	Panics          int64
	// Gates counts the question's own gate and event names (sweep_kept, sweep_unparseable, ...) for warm
	// questions, apart from the ping's.
	Gates map[string]int64
}

// Any reports whether the warm path did or counted anything, so /stats shows nothing when it never ran.
func (w SweepWarmStats) Any() bool {
	if len(w.TriggerReasons) > 0 || len(w.Recent) > 0 || len(w.RecentDecisions) > 0 || w.GatePriceModel != "" || w.PriceModelUnknown > 0 {
		return true
	}
	return w.Fired+w.Asked+w.Held+w.Failed+w.NotWorth+w.NoBilled+w.ThreadBusy+w.ModelDeclined+
		w.ConcurrencyFull+w.HeldExpired+w.HeldShrunk+w.CarriedMerged+w.CarriedOverridden+w.CarriedRefused+w.CarriedExpired+w.CarriedCapped+w.NotRegistered+w.ThreadsFull+w.AlreadyAsked+w.NoInventory+
		w.GateAccept+w.GateRefuse+w.Panics > 0 || len(w.Gates) > 0
}

var (
	sweepWarmGatesMu sync.Mutex
	sweepWarmGates   = map[string]int64{}
)

func bumpSweepWarmGates(counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	sweepWarmGatesMu.Lock()
	for n, c := range counts {
		sweepWarmGates[n] += int64(c)
	}
	sweepWarmGatesMu.Unlock()
}

func copyWarmGates() map[string]int64 {
	sweepWarmGatesMu.Lock()
	defer sweepWarmGatesMu.Unlock()
	out := make(map[string]int64, len(sweepWarmGates))
	for k, v := range sweepWarmGates {
		out[k] = v
	}
	return out
}

// SweepWarmCounters snapshots the warm path's ledger.
func SweepWarmCounters() SweepWarmStats {
	sweepWarmGateMu.Lock()
	rs := make(map[string]int64, len(sweepWarmGateReasons))
	for k, v := range sweepWarmGateReasons {
		rs[k] = v
	}
	sweepWarmGateMu.Unlock()
	sweepWarmGateMu.Lock()
	tr := make(map[string]int64, len(sweepWarmTriggerReasons))
	for k, v := range sweepWarmTriggerReasons {
		tr[k] = v
	}
	sweepWarmGateMu.Unlock()
	return SweepWarmStats{TriggerReasons: tr, Recent: warmRecentCopy(), RecentDecisions: warmDecisionsCopy(), Fired: sweepWarmFired.Load(), Asked: sweepWarmAsked.Load(), Held: sweepWarmHeld.Load(),
		Failed: sweepWarmFailed.Load(), NotWorth: sweepWarmNotWorth.Load(), NoBilled: sweepWarmNoBilled.Load(),
		ThreadBusy: sweepWarmThreadBusy.Load(), ModelDeclined: sweepWarmModelDeclined.Load(),
		ConcurrencyFull: sweepWarmConcurrency.Load(), HeldExpired: sweepWarmHeldExpired.Load(), HeldShrunk: sweepWarmHeldShrunk.Load(), CarriedMerged: sweepWarmCarriedMerged.Load(), CarriedOverridden: sweepWarmCarriedOverridden.Load(), CarriedRefused: sweepWarmCarriedRefused.Load(), CarriedExpired: sweepWarmCarriedExpired.Load(), CarriedCapped: sweepWarmCarriedCapped.Load(), NotRegistered: sweepWarmNotRegistered.Load(), ThreadsFull: sweepWarmThreadsFull.Load(),
		AlreadyAsked: sweepWarmAlreadyAsked.Load(), NoInventory: sweepWarmNoInventory.Load(), Gates: copyWarmGates(),
		PriceModelUnknown: sweepWarmPriceModelUnknown.Load(), GatePriceModel: gatePriceModel(),
		GateAccept: sweepWarmGateAccept.Load(), GateRefuse: sweepWarmGateRefuse.Load(), Reasons: rs,
		Panics: sweepWarmPanics.Load()}
}

func noteWarmGate(accept bool, reason string) {
	if accept {
		sweepWarmGateAccept.Add(1)
		return
	}
	sweepWarmGateRefuse.Add(1)
	sweepWarmGateMu.Lock()
	sweepWarmGateReasons[reason]++
	sweepWarmGateMu.Unlock()
}

// warmHold keeps an answer in memory for the thread's next warm request. It never touches the Store.
func warmHold(key string, h heldAnswer) {
	warmMu.Lock()
	defer warmMu.Unlock()
	t := warmFor(key)
	if t == nil {
		return
	}
	h.V = heldVersion
	// ACCUMULATION (#491). A gate refusal keeps the drops that survived the recheck, in memory, and the NEXT
	// question's answer is merged with them, so refused drops add up across questions until the gate accepts.
	// The merge is by tool-use id, each output once, and the NEWER verdict wins, a newer "keep" included.
	if t.held != nil && (time.Since(t.heldAt) > warmHeldMaxAge || heldAge(*t.held, time.Now()) > warmHeldMaxAge) {
		// carried_expired counts a CARRIED set that aged out. An answer that was never refused (never tried) and
		// simply went stale is not a carried verdict, and memHeld.get counts it as held_expired.
		if t.heldTried {
			sweepWarmCarriedExpired.Add(int64(countDrops(*t.held)))
		} else {
			sweepWarmHeldExpired.Add(1)
		}
		t.held = nil
	}
	if t.held != nil {
		merged, stats := mergeHeld(*t.held, h)
		sweepWarmCarriedMerged.Add(int64(stats.carriedKept))
		sweepWarmCarriedOverridden.Add(int64(stats.overridden))
		sweepWarmCarriedCapped.Add(int64(stats.evicted))
		h = merged
	}
	t.held, t.heldAt, t.heldTried = &h, time.Now(), false
}

// heldAge is how old the oldest question that still contributes a verdict to this answer is. A carried
// answer keeps the AskedAtMs of the OLDEST question that fed it (mergeHeld takes the minimum), so a thread
// cannot keep a verdict alive for ever by being refused again and again.
func heldAge(h heldAnswer, now time.Time) time.Duration {
	if h.AskedAtMs <= 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(h.AskedAtMs))
}

// mergeStats says what a merge did.
type mergeStats struct {
	// carriedKept is carried DROP verdicts that survived into the merged answer; overridden is carried
	// verdicts a newer verdict replaced (including a newer keep); evicted is carried drops cut by the cap.
	carriedKept, overridden, evicted int
}

// mergeHeld merges a carried answer (older) with a new one (newer), by tool-use id. Each output appears
// once. If both judged the same output, the NEWER verdict wins, including a newer "keep". Outputs only the
// carried answer judged stay as they were. The merged answer takes the NEWER answer's coordinates (Judged and
// Prints: the conversation as of the newest question), because the recheck compares the whole set against the
// conversation as it is now, output by output (hash, references), not against the older question's length.
func mergeHeld(carried, newer heldAnswer) (heldAnswer, mergeStats) {
	var st mergeStats
	seen := make(map[string]bool, len(newer.Outputs))
	for _, o := range newer.Outputs {
		seen[o.ToolUseID] = true
	}
	merged := newer
	merged.Outputs = nil
	// Only DROP verdicts are carried. A carried keep says nothing the next recheck needs, and an output that
	// was kept is simply asked about again.
	var keep []heldOutput
	for _, o := range carried.Outputs {
		if !o.Drop {
			continue
		}
		if seen[o.ToolUseID] {
			st.overridden++
			continue
		}
		keep = append(keep, o)
	}
	if over := len(keep) + len(newer.Outputs) - maxCarriedDrops; over > 0 && len(keep) > 0 {
		if over > len(keep) {
			over = len(keep)
		}
		keep = keep[over:] // the oldest go first: carried drops are kept in the order they were judged
		st.evicted = over
	}
	st.carriedKept = len(keep)
	for i := range keep {
		keep[i].Carried = true
	}
	merged.Outputs = append(append(merged.Outputs, keep...), newer.Outputs...)
	merged.CostUSD = carried.CostUSD + newer.CostUSD
	// The age limit counts from the OLDEST question that still contributes a verdict. If every carried
	// verdict was replaced, only the newer question contributes.
	if st.carriedKept > 0 && carried.AskedAtMs > 0 && (newer.AskedAtMs <= 0 || carried.AskedAtMs < newer.AskedAtMs) {
		merged.AskedAtMs = carried.AskedAtMs
	}
	return merged, st
}

// memHeld is the warm source for applyHeldFrom: the thread's in-memory answer. It has no Store.
type memHeld struct{ key string }

func (m memHeld) get(string) (heldAnswer, bool) {
	warmMu.Lock()
	defer warmMu.Unlock()
	t, ok := warmThreads[m.key]
	if !ok || t.held == nil {
		return heldAnswer{}, false
	}
	if time.Since(t.heldAt) > warmHeldMaxAge || heldAge(*t.held, time.Now()) > warmHeldMaxAge {
		// Same split as warmHold: a CARRIED (refused) set that ages out is carried_expired, an answer never
		// refused that goes stale is held_expired.
		if t.heldTried {
			sweepWarmCarriedExpired.Add(int64(countDrops(*t.held)))
		} else {
			sweepWarmHeldExpired.Add(1)
		}
		t.held = nil
		return heldAnswer{}, false
	}
	return *t.held, true
}

// claim takes the answer out of memory for exactly one caller.
func (m memHeld) claim(string) (heldAnswer, bool) {
	warmMu.Lock()
	defer warmMu.Unlock()
	t, ok := warmThreads[m.key]
	if !ok || t.held == nil {
		return heldAnswer{}, false
	}
	h := *t.held
	t.held = nil
	return h, true
}

// put gives an answer back (a panicked apply, or the drops that stay held before a change point).
func (m memHeld) put(h heldAnswer) {
	if h.Spent {
		return
	}
	warmHold(m.key, h)
}

// resetWarmForTest clears every thread's warm state, as a new process would have it.
func resetWarmForTest() {
	resetSweepBatchForTest() // the batching ledger the warm ask feeds (#747)
	warmMu.Lock()
	warmThreads = map[string]*warmThread{}
	warmMu.Unlock()
	sweepWarmGateMu.Lock()
	sweepWarmGateReasons = map[string]int64{}
	sweepWarmTriggerReasons = map[string]int64{}
	warmRecent = nil
	warmDecisions = nil
	sweepWarmGateMu.Unlock()
	sweepWarmGatesMu.Lock()
	sweepWarmGates = map[string]int64{}
	sweepWarmGatesMu.Unlock()
	warmGatePriceModelSet = atomic.Value{}
	for _, c := range []*atomic.Int64{&sweepWarmAsked, &sweepWarmHeld, &sweepWarmFailed, &sweepWarmFired,
		&sweepWarmNotWorth, &sweepWarmNoBilled, &sweepWarmThreadBusy, &sweepWarmModelDeclined,
		&sweepWarmHeldExpired, &sweepWarmHeldShrunk, &sweepWarmCarriedMerged, &sweepWarmCarriedOverridden, &sweepWarmCarriedRefused, &sweepWarmCarriedExpired, &sweepWarmCarriedCapped, &sweepWarmPanics, &sweepWarmNotRegistered, &sweepWarmThreadsFull, &sweepWarmAlreadyAsked, &sweepWarmNoInventory, &sweepWarmPriceModelUnknown, &sweepWarmGateAccept, &sweepWarmGateRefuse, &sweepWarmConcurrency} {
		c.Store(0)
	}
}

// ---------------------------------------------------------------------------------------------------
// The economic gate.

// warmEconIn is everything the gate needs, in tokens. Sizes are LOCAL (this package's count); the two
// ratios turn them into BILLED sizes.
type warmEconIn struct {
	// Removed is what the drops remove. Rewritten is what stays after the first removed output in the
	// cached prefix: the part the provider must write again.
	Removed, Rewritten int
	// RatioContent scales removed content, RatioFull scales the whole prompt. See warmRatios.
	RatioContent, RatioFull float64
	// Read and Write are the per-token rates.
	Read, Write float64
	// Turns is the expected number of warm turns the saving is collected over.
	Turns float64
}

// warmEcon is the gate's arithmetic:
//
//	saving  = removed tokens x read rate x expected warm turns
//	rewrite = rewritten tokens x (write rate - read rate)
//
// Apply only if saving > rewrite.
func warmEcon(in warmEconIn) (saving, rewrite float64) {
	saving = float64(in.Removed) * in.RatioContent * in.Read * in.Turns
	rewrite = float64(in.Rewritten) * in.RatioFull * (in.Write - in.Read)
	return saving, rewrite
}

// warmRatios returns the two billed-over-local ratios, or ok=false when the billed size is not known.
//
// RatioFull is the provider's billed input of the previous turn over this package's count of that same
// turn. It includes the system prompt and the tool declarations, which the local count never sees, so it
// is large (often 3 or more). It scales the REWRITE, which is the whole cached suffix.
//
// RatioContent scales the REMOVED content, which is tool output only. Dividing by the full ratio would
// credit a removed output with the system prompt's overhead. It is the smaller of the family's measured
// content factor (tokens.BilledDeltaFactor, #240) and the full ratio.
//
// WHY TWO RATIOS (#696). One shared ratio cancels out of saving > rewrite, so billed sizes would change
// nothing. The two sides really do differ: a removed output is plain tool text, while the rewritten tail
// also holds what the local count misses (thinking blocks, tool calls, images). The family factor is the
// per-family average of #240 and can be off for a single request (#696). The weights, the cap and the
// ratios are first guesses, to be tuned in the owner's live test.
//
// So a large fixed prompt makes the rewrite look dearer and not the saving: the gate refuses more, never
// less. The direction of every doubt is "refuse".
func warmRatios(model string, prevBilled, prevLocal int) (content, full float64, ok bool) {
	if prevBilled <= 0 || prevLocal <= 0 {
		return 0, 0, false
	}
	full = float64(prevBilled) / float64(prevLocal)
	if full < 1 || full > maxBilledRatio {
		return 0, 0, false
	}
	content, _ = tokens.BilledDeltaFactor(model)
	if full < content {
		content = full
	}
	return content, full, true
}

// warmWeight is the wait-cause weight, read off the request in hand (#453). The thread's last message
// says what woke it: a tool result means it waited on its own tool.
func warmWeight(raw []bschemas.ChatMessage) float64 {
	for i := len(raw) - 1; i >= 0; i-- {
		switch raw[i].Role {
		case bschemas.ChatMessageRoleTool:
			return warmWeightOwnTool
		case bschemas.ChatMessageRoleUser:
			if strings.TrimSpace(schema.MessageText(raw[i])) == "" {
				continue
			}
			return warmWeightHuman
		default:
			return warmWeightUnknown
		}
	}
	return warmWeightUnknown
}

// turnsModel names how the expected warm turns were produced (WarmDecision.TurnsModel).
const (
	turnsModelExpected     = "expected"
	turnsModelCut          = "cut"
	turnsModelSoFar        = "so_far_fallback"
	turnsWhyWindowNotExact = "window_not_exact"
	turnsWhyNoGrowth       = "growth_unknown"
	turnsWhyNoBilled       = "no_billed_size"
	turnsWhyBeyondCut      = "already_at_cut"
)

// turnsEstimate is the expected number of warm turns N and how it was produced.
type turnsEstimate struct {
	N, Raw        float64
	Model, Why    string
	Cut           int
	GrowthPerTurn float64
}

// noteBilledGrowth folds the billed prompt size the host recorded for the previous turn into the growth
// run. A size that did not grow (a client compaction, our own sweep, a rewind) restarts the run from
// there, so the average is the growth per turn SINCE the last shrink. Metadata only: three numbers.
func (t *warmThread) noteBilledGrowth(billed int) {
	if billed <= 0 {
		return
	}
	if t.growthFrom == 0 || billed < t.growthLast {
		t.growthFrom, t.growthTurns = billed, 0
	} else {
		t.growthTurns++
	}
	t.growthLast = billed
}

// warmTurnsLeft is N, the number of warm turns the saving is collected over.
//
//	(a) warm_expected_turns, when set: N = max(expected - turns so far, 0).
//	(b) otherwise, the session is assumed to run until its CUT: the cache_aware_summarizer / summarize
//	    trigger's fraction of the window when one is in the pipeline (Ctx.CutFrac), else the full window.
//	    N = (cut - the current billed prompt) / (the average billed growth per turn on this thread).
//	(c) the turns so far, when (b) cannot be computed: the growth is unknown or not positive, the billed
//	    size is unknown, or the window is only a family guess (CtxWindowExact false), which the summarizer's
//	    own trigger refuses to act on (Trigger.FracResolvable).
//
// warm_max_turns caps N in every case. The wait-cause weight is applied by the caller, at the apply.
func (e *ExtractSweep) warmTurnsLeft(c *components.Ctx, run, growthFrom, growthTurns int) turnsEstimate {
	capN := func(raw float64) float64 {
		if raw > float64(e.warmMaxTurns) {
			return float64(e.warmMaxTurns)
		}
		if raw < 0 {
			return 0
		}
		return raw
	}
	if e.warmExpectedTurns > 0 {
		raw := float64(e.warmExpectedTurns - run)
		return turnsEstimate{N: capN(raw), Raw: raw, Model: turnsModelExpected}
	}
	soFar := func(why string) turnsEstimate {
		return turnsEstimate{N: capN(float64(run)), Raw: float64(run), Model: turnsModelSoFar, Why: why}
	}
	if c == nil || c.PrevBilledInput <= 0 {
		return soFar(turnsWhyNoBilled)
	}
	if !c.CtxWindowExact || c.CtxWindow <= 0 {
		return soFar(turnsWhyWindowNotExact)
	}
	if growthTurns <= 0 || c.PrevBilledInput <= growthFrom {
		return soFar(turnsWhyNoGrowth)
	}
	growth := float64(c.PrevBilledInput-growthFrom) / float64(growthTurns)
	if growth <= 0 {
		return soFar(turnsWhyNoGrowth)
	}
	denom := c.FillDenominator()
	cut := denom
	if c.CutFrac > 0 {
		cut = int(c.CutFrac * float64(denom))
	}
	if cut <= c.PrevBilledInput {
		est := soFar(turnsWhyBeyondCut)
		est.Cut, est.GrowthPerTurn = cut, growth
		return est
	}
	raw := float64(cut-c.PrevBilledInput) / growth
	return turnsEstimate{N: capN(raw), Raw: raw, Model: turnsModelCut, Cut: cut, GrowthPerTurn: growth}
}

func localTokens(msgs []bschemas.ChatMessage) int {
	n := 0
	for i := range msgs {
		n += schema.TextTokens(schema.MessageText(msgs[i]))
	}
	return n
}

// cachedEnd is the last index of req.Input that is inside the cached prefix.
func cachedEnd(c *components.Ctx, req *bschemas.BifrostChatRequest) int {
	if c != nil && c.CacheAware && c.MaxCachedIdx >= 0 && c.MaxCachedIdx < len(req.Input) {
		return c.MaxCachedIdx
	}
	return len(req.Input) - 1
}

// contentBlocks counts the content items of one message, one block per item: each content block, or one
// for a plain text, plus one per tool call. A message with nothing counts zero.
func contentBlocks(m bschemas.ChatMessage) int {
	n := 0
	if m.Content != nil {
		if len(m.Content.ContentBlocks) > 0 {
			n += len(m.Content.ContentBlocks)
		} else if m.Content.ContentStr != nil && *m.Content.ContentStr != "" {
			n++
		}
	}
	if m.ChatAssistantMessage != nil {
		n += len(m.ChatAssistantMessage.ToolCalls)
	}
	return n
}

// warmRewrite is what the provider writes again when the first drop sits at index first, and whether the
// drop was DEEP.
//
// The provider reads back only up to a cache entry an earlier request wrote at a breakpoint BEFORE the
// change. For a drop farther than warmLookbackBlocks content blocks from the previous request's last
// breakpoint (end), that entry is the fixed system prompt and tools, so the rewrite is the WHOLE
// conversation part of the prompt (index 0 on). For a drop within the look-back it is only the part from
// the drop on. Either way the removals themselves come off, and what replaces the first drop (its
// descriptor and marker) counts, since those are new bytes.
func warmRewrite(req *bschemas.BifrostChatRequest, first, end int, others []warmRemoval) (tokens int, deep bool) {
	if end >= len(req.Input) {
		end = len(req.Input) - 1
	}
	blocks := 0
	for j := first; j <= end && j >= 0; j++ {
		blocks += contentBlocks(req.Input[j])
	}
	deep = blocks > warmLookbackBlocks
	start := first
	if deep {
		start = 0
	}
	for j := start; j <= end && j >= 0 && j < len(req.Input); j++ {
		tokens += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	for _, o := range others {
		if o.idx >= start && o.idx <= end {
			tokens -= o.saves
		}
	}
	if tokens < 0 {
		tokens = 0
	}
	return tokens, deep
}

type warmRemoval struct{ idx, saves int }

// warmGate builds the apply-time gate for one request.
func (e *ExtractSweep) warmGate(c *components.Ctx, req *bschemas.BifrostChatRequest, run, growthFrom, growthTurns, prevLocal int, wa *warmApply) func(res recheckResult, raw []bschemas.ChatMessage) (bool, string) {
	return func(res recheckResult, raw []bschemas.ChatMessage) (ok bool, reason string) {
		tkey := thread.Key(c.Session, c.Thread)
		d := WarmDecision{Stage: "apply", Weight: warmWeight(raw)}
		// EVERY exit records its decision, accepted or refused, with the numbers it had by then.
		defer func() {
			d.Accept, d.Reason = ok, reason
			noteWarmDecision(tkey, d)
		}()
		// The recheck's own account, recorded whatever the gate decides below.
		d.Proposed, d.Dropped = res.proposed, res.reasonCounts()
		for _, o := range res.Valid {
			if o.Carried {
				d.Carried++
				d.CarriedTokens += o.Saves
			} else {
				d.New++
				d.NewTokens += o.Saves
			}
		}
		if len(res.Valid) == 0 {
			// Two different things were both called no_valid_drop. The judge proposing NO drop at all leaves
			// nothing to recheck (recheckHeld returns at once): that is no_drop_proposed. A proposal the recheck
			// then threw out in full is no_valid_drop, and d.Dropped says why.
			if res.proposed == 0 {
				return false, "no_drop_proposed"
			}
			return false, "no_valid_drop"
		}
		content, full, bok := warmRatios(c.ModelName, c.PrevBilledInput, prevLocal)
		if !bok {
			return false, "no_billed_size"
		}
		d.RatioContent, d.RatioFull = content, full
		var rm []warmRemoval
		removed, first := 0, -1
		for _, o := range res.Valid {
			idx, found := rawToolMessage(req.Input, o.ToolUseID)
			if !found {
				continue
			}
			rm = append(rm, warmRemoval{idx, o.Saves})
			removed += o.Saves
			if first < 0 || idx < first {
				first = idx
			}
		}
		if first < 0 {
			return false, "no_valid_drop"
		}
		read, write := e.warmGateRates(c)
		rewritten, deep := warmRewrite(req, first, cachedEnd(c, req), rm)
		te := e.warmTurnsLeft(c, run, growthFrom, growthTurns)
		turns := te.N * d.Weight
		in := warmEconIn{Removed: removed, Rewritten: rewritten,
			RatioContent: content, RatioFull: full, Read: read, Write: write, Turns: turns}
		saving, rewrite := warmEcon(in)
		d.TurnsModel, d.TurnsRaw, d.Cut, d.GrowthPerTurn, d.TurnsWhy = te.Model, te.Raw, te.Cut, te.GrowthPerTurn, te.Why
		d.RemovedLocal, d.RemovedBilled = removed, int(float64(removed)*content)
		d.RewrittenLocal, d.RewrittenBilled = rewritten, int(float64(rewritten)*full)
		d.Turns, d.Deep, d.Saving, d.Rewrite = in.Turns, deep, saving, rewrite
		if !(saving > rewrite) {
			if deep {
				return false, "does_not_pay_deep"
			}
			return false, "does_not_pay"
		}
		wa.rewriteBilled = int(float64(in.Rewritten) * in.RatioFull)
		wa.removedBilled = int(float64(removed) * in.RatioContent)
		wa.turns, wa.netUSD, wa.deep = in.Turns, saving-rewrite, deep
		return true, ""
	}
}

// ---------------------------------------------------------------------------------------------------
// The step, run on every request of the thread when warm is on.

func (e *ExtractSweep) warmStep(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) (keys []string, changed int) {
	if c == nil || c.Store == nil || c.Session == "" || req == nil || c.Bypass || c.Mode == components.ModeObserve {
		return nil, 0
	}
	tkey := thread.Key(c.Session, c.Thread)
	cold := sweepColdRequest(c)

	// The thread's counters. prevLocal is the previous request's local size, the other half of the billed
	// ratio, read BEFORE it is replaced by this request's.
	local := localTokens(req.Input)
	warmMu.Lock()
	t := warmFor(tkey)
	if t == nil {
		warmMu.Unlock()
		return nil, 0 // the table is full of threads with something to lose: this one is not tracked
	}
	prevLocal := t.lastLocal
	t.lastLocal = local
	if cold {
		t.run, t.count = 0, 0
		t.growthFrom, t.growthTurns, t.growthLast = 0, 0, 0
	} else {
		t.run++
		t.count++
		t.noteBilledGrowth(c.PrevBilledInput)
	}
	run, count := t.run, t.count
	growthFrom, growthTurns := t.growthFrom, t.growthTurns
	busy := t.asking || t.warmPending()
	hasHeld := t.held != nil
	heldTried := t.held != nil && t.heldTried
	var heldJudged, heldProposed int
	if hasHeld {
		heldJudged = t.held.Judged
		heldProposed, _ = t.held.proposed()
	}
	warmMu.Unlock()

	// FAIL OPEN. A panic anywhere below forwards the request as it stands. applyHeldFrom has its own
	// recover that restores what it touched; this one covers the trigger.
	defer func() {
		if r := recover(); r != nil {
			sweepWarmPanics.Add(1)
			keys, changed = nil, 0
			slog.Error("cg.sweep.warm_panic: recovered; the request is forwarded unchanged",
				"session", c.Session, "thread", c.Thread, "panic", fmt.Sprint(r))
		}
	}()

	// A SHRUNK CONVERSATION VOIDS THE HELD ANSWER (#491). The answer is a list of verdicts about the messages of
	// the conversation as it was when the question was asked (Judged of them). A request with FEWER messages is a
	// different conversation: the agent compacted, was rewound, or started over (a live run went from 331 to 6
	// messages while an answer for the old one was still held). The recheck would refuse every drop as
	// conversation_shorter anyway, but only on the next apply, after the answer had sat in memory and blocked a
	// new question. Dropped now, counted, and recorded as a decision.
	if hasHeld {
		rawNow := req.Input
		if c.RawInput != nil {
			rawNow = c.RawInput
		}
		if heldJudged > 0 && len(rawNow) < heldJudged {
			// The lock was released since heldJudged was read, so a question may have landed a FRESH answer. Void
			// only the answer this decision was made on: the same one (same Judged) is still held.
			voided := false
			if warmShrinkHook != nil {
				warmShrinkHook()
			}
			warmMu.Lock()
			if t := warmFor(tkey); t != nil && t.held != nil && t.held.Judged == heldJudged {
				t.held = nil
				voided = true
			}
			warmMu.Unlock()
			if voided {
				sweepWarmHeldShrunk.Add(1)
				noteWarmDecision(tkey, WarmDecision{Stage: "apply", Accept: false, Reason: "conversation_shrunk",
					Weight: 1, Proposed: heldProposed})
			}
			hasHeld = false
		}
	}
	// A CARRIED set the gate already refused is not retried on every warm request: the trigger runs instead and
	// asks a new question, whose drops are merged in, and THEN the gate decides on the merged set. A FRESH
	// answer (never tried) is applied on the next warm request, as before.
	if hasHeld && !heldTried {
		warmMu.Lock()
		if t := warmFor(tkey); t != nil {
			t.count = 0 // a refused or spent answer must not earn a new question at once
		}
		warmMu.Unlock()
		wa := &warmApply{}
		wa.gate = e.warmGate(c, req, run, growthFrom, growthTurns, prevLocal, wa)
		wk, wn := e.applyHeldFrom(memHeld{tkey}, wa, req, rep, c, tkey)
		if wn > 0 {
			return wk, wn
		}
		// The answer was spent (applied nothing, or refused by the gate). Do not ask again at once.
		return nil, 0
	}
	if warmTriggerHook != nil {
		warmTriggerHook()
	}
	if cold || busy || count < e.warmEveryN {
		return nil, 0
	}

	// The trigger: a rule, then the worth test. With a CARRIED set (refused drops waiting for more) it still
	// runs: that is how the set grows. The count restarts here whether or not a question follows.
	warmMu.Lock()
	if t = warmFor(tkey); t != nil {
		t.count = 0
	}
	warmMu.Unlock()
	if !e.warmWorthAsking(c, req, prevLocal, run, growthFrom, growthTurns) {
		return nil, 0
	}
	e.registerWarm(c, tkey)
	return nil, 0
}

// warmWorthAsking is the cheap test before a question: are there outputs old enough and big enough that
// dropping ALL of them could pay back the rewrite? It reads sizes and positions only. prevLocal is the
// local size of the previous request, the other half of the billed ratio.
func (e *ExtractSweep) warmWorthAsking(c *components.Ctx, req *bschemas.BifrostChatRequest, prevLocal, run, growthFrom, growthTurns int) (worth bool) {
	d := WarmDecision{Stage: "trigger", Weight: 1}
	// EVERY exit records its decision, accepted or refused (a refusal carries the reason noteWarmTrigger
	// counted, read back from d.Reason).
	defer func() {
		d.Accept = worth
		noteWarmDecision(thread.Key(c.Session, c.Thread), d)
	}()
	content, full, ok := warmRatios(c.ModelName, c.PrevBilledInput, prevLocal)
	if !ok {
		sweepWarmNoBilled.Add(1)
		noteWarmTrigger("no_billed_size")
		d.Reason = "no_billed_size"
		return false
	}
	// Model turns after each message, one pass.
	later := make([]int, len(req.Input)+1)
	for i := len(req.Input) - 1; i >= 0; i-- {
		later[i] = later[i+1]
		if req.Input[i].Role == bschemas.ChatMessageRoleAssistant {
			later[i]++
		}
	}
	d.RatioContent, d.RatioFull = content, full
	var rm []warmRemoval
	removed, first := 0, -1
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) || later[i] < e.warmMinAge {
			continue
		}
		text := schema.MessageText(*msg)
		// The same rule as the question's candidate scan (carriesRealMarker), so the mass test counts what
		// the question would offer: text that only QUOTES marker syntax is still a candidate.
		if text == "" || carriesRealMarker(c, text) {
			continue
		}
		tk := schema.TextTokens(text)
		if tk < e.minTokens {
			continue
		}
		saves := tk - schema.TextTokens(sweepDescriptor(text))
		if saves <= 0 {
			continue
		}
		rm = append(rm, warmRemoval{i, saves})
		removed += saves
		if first < 0 {
			first = i
		}
	}
	if first < 0 {
		sweepWarmNotWorth.Add(1)
		noteWarmTrigger("no_candidate")
		d.Reason = "no_candidate"
		return false
	}
	d.RemovedLocal, d.RemovedBilled = removed, int(float64(removed)*content)
	// THE MASS GATE: the removable mass must be a real share of the conversation.
	if conv := localTokens(req.Input); conv > 0 && float64(removed) < e.warmMinMassFrac*float64(conv) {
		sweepWarmNotWorth.Add(1)
		noteWarmTrigger("below_mass")
		d.Reason = "below_mass"
		return false
	}
	read, write := e.warmGateRates(c)
	// Optimistic: every candidate dropped. The expected turns come from the SAME model the apply gate uses
	// (warmTurnsLeft), so a thread is not asked about while the gate would refuse it for want of turns.
	// No wait-cause weight is applied here: the weight (warmWeight) is read off the request at the apply,
	// where the gate uses it.
	te := e.warmTurnsLeft(c, run, growthFrom, growthTurns)
	rewritten, deep := warmRewrite(req, first, cachedEnd(c, req), rm)
	d.RewrittenLocal, d.RewrittenBilled, d.Turns, d.Deep = rewritten, int(float64(rewritten)*full), te.N, deep
	d.TurnsModel, d.TurnsRaw, d.Cut, d.GrowthPerTurn, d.TurnsWhy = te.Model, te.Raw, te.Cut, te.GrowthPerTurn, te.Why
	in := warmEconIn{Removed: removed, Rewritten: rewritten,
		RatioContent: content, RatioFull: full, Read: read, Write: write, Turns: te.N}
	saving, rewrite := warmEcon(in)
	d.Saving, d.Rewrite = saving, rewrite
	if !(saving > rewrite) {
		sweepWarmNotWorth.Add(1)
		noteWarmTrigger("does_not_pay")
		d.Reason = "does_not_pay"
		return false
	}
	return true
}

// warmGateRates are the read and write rates the gate decides with. Normally the request's own. With
// warm_gate_price_model set (a DIAGNOSTIC), those of that model, so a run on a model priced at $0 still
// gets real decisions. A model the rate card does not know falls back to the built-in rates and is
// counted (price_model_unknown); the dashboard and /stats are never affected, they price the real model.
func (e *ExtractSweep) warmGateRates(c *components.Ctx) (read, write float64) {
	if e.warmGatePriceModel != "" && c != nil && c.RatesFor != nil {
		if r := c.RatesFor(e.warmGatePriceModel); !r.Zero() {
			cc := *c
			cc.SelfRates = r
			_, read, write = agentRates(&cc)
			return read, write
		}
		sweepWarmPriceModelUnknown.Add(1)
		_, read, write = agentRates(nil)
		return read, write
	}
	if e.warmGatePriceModel != "" {
		sweepWarmPriceModelUnknown.Add(1)
		_, read, write = agentRates(nil)
		return read, write
	}
	_, read, write = agentRates(c)
	return read, write
}

var warmGatePriceModelSet atomic.Value // string

// noteWarmGatePriceModel logs the diagnostic setting once per distinct model and keeps it for /stats.
func noteWarmGatePriceModel(m string) {
	if prev, _ := warmGatePriceModelSet.Load().(string); prev == m {
		return
	}
	warmGatePriceModelSet.Store(m)
	slog.Warn("cg.sweep.warm_gate_price_model: DIAGNOSTIC setting on. The warm gate prices with this model, "+
		"not the request's. This is not a normal run.", "model", m, "gate", "sweep_warm_gate_price_model")
}

// noteWarmTrigger counts a refusal at the trigger by reason.
func noteWarmTrigger(reason string) {
	sweepWarmGateMu.Lock()
	sweepWarmTriggerReasons[reason]++
	sweepWarmGateMu.Unlock()
}

// ---------------------------------------------------------------------------------------------------
// The question, after the forward.

// warmPending reports whether a trigger is waiting for its request to be forwarded.
func (t *warmThread) warmPending() bool {
	return !t.pendingAt.IsZero() && time.Since(t.pendingAt) < warmPendingMax
}

// registerWarm notes that a question should run once this request has been forwarded. It keeps no copy
// of the conversation: a thread the keep-alive cannot ping has no registry copy to ask from, so it gets
// no warm question, and that is counted (sweep_warm_not_registered). The host calls ResolveWarmQuestion
// when the response is in; the question then reads the registry's copy, which this request's own
// registerPing has just refreshed.
func (e *ExtractSweep) registerWarm(c *components.Ctx, tkey string) {
	if c.PrefixAsk == nil || c.CanKeepAlive == nil || !c.CanKeepAlive() {
		sweepWarmNotRegistered.Add(1)
		return
	}
	warmMu.Lock()
	defer warmMu.Unlock()
	t := warmFor(tkey)
	if t == nil || t.warmPending() || t.asking {
		return
	}
	t.pendingAt = time.Now()
	sweepWarmFired.Add(1)
}

// ResolveWarmQuestion is called by the host after the thread's request was forwarded and its response
// read. forwarded=false (the request failed) drops the registration: the stashed body is not the one the
// snapshot was taken from. The question runs on its own goroutine, so the caller waits for nothing.
func ResolveWarmQuestion(session, threadID string, forwarded bool, turnRead int) {
	runWarmQuestion(thread.Key(session, threadID), forwarded, turnRead)
}

func runWarmQuestion(tkey string, forwarded bool, turnRead int) {
	warmMu.Lock()
	t, ok := warmThreads[tkey]
	if !ok || !t.warmPending() {
		warmMu.Unlock()
		return
	}
	t.pendingAt = time.Time{}
	if !forwarded {
		warmMu.Unlock()
		return
	}
	t.asking = true
	warmMu.Unlock()
	// release undoes `asking` on every early exit below.
	release := func() {
		warmMu.Lock()
		if t2, ok := warmThreads[tkey]; ok {
			t2.asking = false
		}
		warmMu.Unlock()
	}
	// The ping registry's copy of the conversation, shared, never copied again. THE LATCH IS SET HERE,
	// before the goroutine starts: the registry candidate's `asked` flag is the one SweepPingSubstitute
	// reads, so a ping that falls due after this question sends a plain ping instead of paying for the
	// same conversation again. If the ping already asked about it (latch set), or its held answer is in
	// the Store, the warm path does not ask.
	sweepPingMu.Lock()
	reg := sweepPingCand[tkey]
	latched := reg != nil && !reg.asked.CompareAndSwap(false, true)
	sweepPingMu.Unlock()
	if reg == nil {
		release()
		sweepWarmNotRegistered.Add(1) // under the 80k floor, or no entry: no copy to ask from
		return
	}
	if latched {
		release()
		sweepWarmAlreadyAsked.Add(1)
		return
	}
	if _, ok := getHeld(reg.ctx.Store, tkey); ok {
		reg.asked.Store(false) // no question was paid for; the registry copy stays askable at the ping
		release()
		sweepWarmAlreadyAsked.Add(1)
		return
	}
	cand := &sweepPingCandidate{e: reg.e, ctx: reg.ctx, input: reg.input, raw: reg.raw,
		registeredAt: reg.registeredAt, warm: true, minAge: reg.e.warmMinAge}
	go func() {
		defer release()
		// FAIL OPEN: a panic in collection, the ask or the hold ends this goroutine and nothing else.
		defer func() {
			if r := recover(); r != nil {
				sweepWarmPanics.Add(1)
				slog.Error("cg.sweep.warm_panic: recovered; no warm answer is held", "thread", tkey, "panic", fmt.Sprint(r))
			}
		}()
		// A question that never reached the provider (thread busy, slots full, no inventory) gives the
		// registry latch back, so the ping may still ask. One that did keeps it set.
		if !askWarm(tkey, cand, turnRead) {
			reg.asked.Store(false)
		}
	}()
}

// askWarm is the ping's SweepPingSubstitute plus its dispatch, for a thread that has no ping due. It
// takes the same guards: the thread's ping claim, so no ping question runs on the thread at once and
// none starts while this one runs; no summary in flight; and the shared question slots.
func askWarm(tkey string, cand *sweepPingCandidate, turnRead int) (asked bool) {
	if warmAskSpy != nil {
		warmAskSpy(cand)
	}
	release, ok := ClaimThread(tkey)
	if !ok {
		sweepWarmThreadBusy.Add(1)
		return false
	}
	defer release()
	if _, running := inFlight.peek(tkey); running {
		sweepWarmThreadBusy.Add(1)
		return false
	}
	select {
	case sweepSlots <- struct{}{}:
	default:
		sweepWarmConcurrency.Add(1)
		return false
	}
	var cands []sweepCand
	rep := &components.Report{Component: "extract_llm_sweep", Kind: "offload"}
	declined := false
	func() {
		defer func() { <-sweepSlots }()
		if sweepModelDeclines(cand.ctx.ModelName, cand.input) {
			sweepWarmModelDeclined.Add(1)
			declined = true
			return
		}
		cands = cand.collect(rep)
	}()
	if declined {
		return false
	}
	if len(cands) == 0 {
		// The collection's gates say why nothing was offered, as at the ping.
		bumpSweepWarmGates(rep.Gates)
		sweepWarmNoInventory.Add(1)
		return false
	}
	if len(cands) < cand.e.minInventory {
		sweepWarmNoInventory.Add(1)
		return false
	}
	// The cache read is judged by prefixAskCacheOK against this turn's own read (#725), the one rule the
	// ping uses. The warm path has no write check of its own.
	//
	// The OUTER bound for the whole sweep is the keeper's, as at the ping (CacheAwareSummarizerCallTimeout,
	// 300 s): the sweep is k questions in sequence (askInBatches), and each keeps its own 90 s. A timeout of 0
	// would fall back to the 90 s of ONE question and end a sweep of several after the first.
	cand.ask(cands, rep, CacheAwareSummarizerCallTimeout(), SweepPingWait{LastTurnRead: turnRead})
	return cand.asked.Load()
}

// ---------------------------------------------------------------------------------------------------
// File-read evidence (#491, point 2).

// fileEvidence says, for each file read in a conversation, whether the agent later edited that file or
// read it again. It comes from tool NAMES and tool INPUT PATHS only, which the request already carries.
// No tool output is read, and no path leaves this function: the clause holds counts.
type fileEvidence struct {
	// read maps a tool output's message index to its path.
	read map[int]string
	// later maps a path to the message indexes of the calls that edit or re-read it.
	edits, reads map[string][]int
}

var pathKeys = []string{"file_path", "path", "notebook_path", "filename"}

func toolKind(name string) (read, edit bool) {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "edit"), strings.Contains(n, "write"), strings.Contains(n, "patch"), strings.Contains(n, "replace"):
		return false, true
	case strings.Contains(n, "read"), strings.Contains(n, "view"), n == "cat", strings.Contains(n, "open_file"):
		return true, false
	}
	return false, false
}

func callPath(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	for _, k := range pathKeys {
		if v, ok := m[k].(string); ok && v != "" {
			return filepath.Clean(v)
		}
	}
	return ""
}

func newFileEvidence(req *bschemas.BifrostChatRequest) *fileEvidence {
	fe := &fileEvidence{read: map[int]string{}, edits: map[string][]int{}, reads: map[string][]int{}}
	for i := range req.Input {
		a := req.Input[i].ChatAssistantMessage
		if a == nil {
			continue
		}
		for _, tc := range a.ToolCalls {
			name := ""
			if tc.Function.Name != nil {
				name = *tc.Function.Name
			}
			rd, ed := toolKind(name)
			if !rd && !ed {
				continue
			}
			path := callPath(tc.Function.Arguments)
			if path == "" {
				continue
			}
			if ed {
				fe.edits[path] = append(fe.edits[path], i)
			} else {
				fe.reads[path] = append(fe.reads[path], i)
				// The output of this read: the tool message with this call's id.
				if tc.ID != nil {
					if j, ok := rawToolMessage(req.Input, *tc.ID); ok {
						fe.read[j] = path
					}
				}
			}
		}
	}
	return fe
}

// clause renders the evidence for the tool output at message index i, or "" when it is not a file read.
func (fe *fileEvidence) clause(i int) string {
	path, ok := fe.read[i]
	if !ok {
		return ""
	}
	edits, rereads := 0, 0
	for _, j := range fe.edits[path] {
		if j > i {
			edits++
		}
	}
	for _, j := range fe.reads[path] {
		if j > i {
			rereads++
		}
	}
	return fmt.Sprintf("file_read=yes later_edits=%d later_rereads=%d", edits, rereads)
}

// fileEvidenceFor is the one-candidate form, for a test.
func fileEvidenceFor(req *bschemas.BifrostChatRequest, i int) string {
	return newFileEvidence(req).clause(i)
}

// ---------------------------------------------------------------------------------------------------
// The record of each warm sweep, for the live test (#491).

// WarmSweep is one applied warm sweep: what it removed, the conversation before and after, the gate's
// numbers and the net it expected. Numbers only: the thread is a hash label, never a session id.
type WarmSweep struct {
	Thread string `json:"thread"`
	AtMs   int64  `json:"at_ms"`
	Drops  int    `json:"drops"`
	// RemovedLocal is the tokens removed by this package's count; RemovedBilled is the same scaled by the
	// content ratio the gate used.
	RemovedLocal  int `json:"removed_tokens_local"`
	RemovedBilled int `json:"removed_tokens_billed"`
	// RewriteBilled is the rewrite the gate counted, billed-scaled.
	RewriteBilled int `json:"rewrite_tokens_billed"`
	// ConvBefore and ConvAfter are the conversation's local token count before and after the sweep.
	ConvBefore int `json:"conversation_before"`
	ConvAfter  int `json:"conversation_after"`
	// ExpectedTurns is the warm turns the gate expected; NetUSD is saving - rewrite over them.
	ExpectedTurns float64 `json:"expected_turns"`
	NetUSD        float64 `json:"expected_net_usd"`
	// Deep: the drop was deeper than the provider's look-back, so the rewrite was the whole conversation.
	Deep bool `json:"deep"`
}

const maxWarmRecent = 50

// WarmDecision is one trigger or apply-gate decision, accepted OR refused, with the numbers it was made
// on. A refused sweep used to record nothing, so a live run could not say why. Numbers only: the thread is
// a hash label and no content is in it.
type WarmDecision struct {
	Thread string `json:"thread"`
	AtMs   int64  `json:"at_ms"`
	// Stage is "trigger" (before a question) or "apply" (the gate on a held answer).
	Stage  string `json:"stage"`
	Accept bool   `json:"accept"`
	// Reason is why a decision was a refusal ("" on accept).
	Reason string `json:"reason,omitempty"`
	// RemovedLocal and RemovedBilled are D: the tokens the drops would remove, by this package's count and
	// scaled by the content ratio. RewrittenLocal and RewrittenBilled are R: what the provider would write
	// again, scaled by the full ratio.
	RemovedLocal    int `json:"removed_local"`
	RemovedBilled   int `json:"removed_billed"`
	RewrittenLocal  int `json:"rewritten_local"`
	RewrittenBilled int `json:"rewritten_billed"`
	// Turns is N, the expected warm turns; Weight is the wait-cause weight at the apply (1 at the trigger,
	// which applies none).
	Turns  float64 `json:"turns"`
	Weight float64 `json:"weight"`
	// RatioFull and RatioContent are the billed-over-local ratios the gate used.
	RatioFull    float64 `json:"ratio_full"`
	RatioContent float64 `json:"ratio_content"`
	Deep         bool    `json:"deep"`
	// Proposed is how many drops the held answer carried into the recheck, and Dropped is what the recheck
	// removed from them, by reason (output_gone, content_changed, referenced_later, expanded, conversation_shorter).
	// Without it a refused apply read as "no valid drop" with no way to say why 59 to 87% of the judge's
	// drops were gone by the time the gate saw them. Apply stage only.
	Proposed int            `json:"proposed,omitempty"`
	Dropped  map[string]int `json:"dropped,omitempty"`
	// Carried and New split the merged answer the recheck saw: drops carried forward from earlier REFUSED
	// answers, and drops the newest question proposed (CarriedTokens and NewTokens are their Saves). Merged
	// D is RemovedLocal, the surviving set the gate priced. Apply stage only.
	Carried       int `json:"carried,omitempty"`
	New           int `json:"new,omitempty"`
	CarriedTokens int `json:"carried_tokens,omitempty"`
	NewTokens     int `json:"new_tokens,omitempty"`
	// TurnsModel says which model produced Turns: "expected" (warm_expected_turns), "cut" (turns left before
	// the conversation is cut, from the billed growth per turn) or "so_far_fallback" (the warm turns so
	// far). TurnsRaw is N before the warm_max_turns cap and the weight; Cut is the billed size the model
	// assumed the session runs to; GrowthPerTurn is the average billed growth per turn; TurnsWhy names why
	// the "cut" model was not used when it was not.
	TurnsModel    string  `json:"turns_model,omitempty"`
	TurnsRaw      float64 `json:"turns_raw"`
	Cut           int     `json:"cut,omitempty"`
	GrowthPerTurn float64 `json:"growth_per_turn,omitempty"`
	TurnsWhy      string  `json:"turns_why,omitempty"`
	// Saving and Rewrite are the gate's two sides in USD, so the refusal reads as numbers.
	Saving  float64 `json:"saving_usd"`
	Rewrite float64 `json:"rewrite_usd"`
}

var warmDecisions []WarmDecision

// noteWarmDecision keeps the last maxWarmRecent decisions, logs one INFO line (numbers only), and is
// called for every decision. It never fails a request.
func noteWarmDecision(tkey string, d WarmDecision) {
	d.Thread, d.AtMs = threadLabel(tkey), time.Now().UnixMilli()
	sweepWarmGateMu.Lock()
	warmDecisions = append(warmDecisions, d)
	if len(warmDecisions) > maxWarmRecent {
		warmDecisions = warmDecisions[len(warmDecisions)-maxWarmRecent:]
	}
	sweepWarmGateMu.Unlock()
	slog.Info("cg.sweep.warm_decision", "thread", d.Thread, "stage", d.Stage, "accept", d.Accept, "reason", d.Reason,
		"removed_local", d.RemovedLocal, "removed_billed", d.RemovedBilled,
		"rewritten_local", d.RewrittenLocal, "rewritten_billed", d.RewrittenBilled,
		"turns", d.Turns, "weight", d.Weight, "ratio_full", d.RatioFull, "ratio_content", d.RatioContent,
		"deep", d.Deep, "saving_usd", d.Saving, "rewrite_usd", d.Rewrite)
}

func warmDecisionsCopy() []WarmDecision {
	sweepWarmGateMu.Lock()
	defer sweepWarmGateMu.Unlock()
	return append([]WarmDecision(nil), warmDecisions...)
}

var warmRecent []WarmSweep

func removedNow(p []pendingDrop) int {
	n := 0
	for _, d := range p {
		n += d.rec.Tokens
	}
	return n
}

func noteWarmSweep(tkey string, w WarmSweep) {
	w.Thread, w.AtMs = threadLabel(tkey), time.Now().UnixMilli()
	sweepWarmGateMu.Lock()
	defer sweepWarmGateMu.Unlock()
	warmRecent = append(warmRecent, w)
	if len(warmRecent) > maxWarmRecent {
		warmRecent = warmRecent[len(warmRecent)-maxWarmRecent:]
	}
}

func warmRecentCopy() []WarmSweep {
	sweepWarmGateMu.Lock()
	defer sweepWarmGateMu.Unlock()
	return append([]WarmSweep(nil), warmRecent...)
}
func gatePriceModel() string { m, _ := warmGatePriceModelSet.Load().(string); return m }

// lastWarmDecision returns the newest recorded decision.
func lastWarmDecision() (WarmDecision, bool) {
	sweepWarmGateMu.Lock()
	defer sweepWarmGateMu.Unlock()
	if len(warmDecisions) == 0 {
		return WarmDecision{}, false
	}
	return warmDecisions[len(warmDecisions)-1], true
}

// carryRefused puts back, after a gate REFUSAL, the drops of a claimed answer that survived the recheck. The
// survivors keep their verdicts; everything else (the kept verdicts and the drops the recheck threw out) is
// left behind. The answer's age is NOT reset: a carried drop is as old as the question that judged it, so a
// thread cannot keep a verdict alive for ever by being refused (warmHeldMaxAge counts from the oldest
// question that still contributes). Returns how many drops were carried.
func carryRefused(src heldSource, tkey string, claimed heldAnswer, res recheckResult) int {
	m, ok := src.(memHeld)
	if !ok || len(res.Valid) == 0 {
		return 0
	}
	back := claimed
	back.Outputs = append([]heldOutput(nil), res.Valid...)
	warmMu.Lock()
	defer warmMu.Unlock()
	t := warmFor(m.key)
	if t == nil {
		return 0
	}
	if t.held != nil {
		// A newer answer landed while this one was being applied: merge, newer wins.
		merged, _ := mergeHeld(back, *t.held)
		back = merged
	}
	back.V = heldVersion
	t.held = &back
	// Marked tried even when that newer answer (never tried itself) is now part of the set. This is
	// conservative on purpose: the next warm request asks a new question instead of applying at once, so a
	// fresh answer waits one more question rather than risk an apply on a set the gate has just refused.
	t.heldTried = true
	// heldAt is NOT reset: it is the age the get() below checks, and a refused answer is no younger for having
	// been refused. (warmHold, for a NEW question, does reset it: that answer is fresh.)
	sweepWarmCarriedRefused.Add(int64(len(res.Valid)))
	return len(res.Valid)
}

// countDrops is how many drop verdicts a held answer carries.
func countDrops(h heldAnswer) int {
	n, _ := h.proposed()
	return n
}

// SeedCarriedCountersForTest sets each carried-set counter to a distinct non-zero value and returns a func
// that clears the warm state. It lets the keeper's test prove every counter reaches /stats.
func SeedCarriedCountersForTest() (reset func()) {
	resetWarmForTest()
	sweepWarmCarriedMerged.Store(1)
	sweepWarmCarriedOverridden.Store(2)
	sweepWarmCarriedRefused.Store(3)
	sweepWarmCarriedExpired.Store(4)
	sweepWarmCarriedCapped.Store(5)
	return resetWarmForTest
}
