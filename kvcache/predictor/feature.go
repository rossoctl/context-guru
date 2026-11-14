package predictor

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rossoctl/context-guru/kvcache"
)

// AvailabilityClass says how reachable a feature is for a LIVE decision on the request
// path — a claim about production, not about whether the historical store happens to have
// the column.
type AvailabilityClass string

const (
	// Live features are read inline on the request path today, the same footing
	// kvcache.Observation's own fields stand on (StopReason, CachedTokens, ...).
	Live AvailabilityClass = "live"
	// OfflineDerivable features are computable from stored columns but are not wired into
	// the live path — reverts_lag1 and expands_lag1 are here until dash's row scan is
	// wired to populate kvcache.Request.Reverts/Expands (see that field's own doc comment).
	OfflineDerivable AvailabilityClass = "offline_derivable"
	// NeedsInstrumentation features name a column or a timestamp that does not exist in the
	// store yet at all. Cataloging one here is how a future instrumentation request gets
	// written down instead of re-discovered.
	NeedsInstrumentation AvailabilityClass = "needs_instrumentation"
	// Impossible features read the future by definition (idle_ms, next_ts). A feature with
	// this class carries NO Extractor — see Features.Register — so it exists purely as a
	// named ceiling: the fact Optimal alone is allowed to use, never a policy input.
	Impossible AvailabilityClass = "impossible"
)

// PrivacyClass is how sensitive a feature's value is, for a report or a log line that
// might carry it.
type PrivacyClass string

const (
	// PrivacyAggregate features carry no tenant-identifying value (a stop-reason cluster
	// name, an hour-of-day bucket).
	PrivacyAggregate PrivacyClass = "aggregate"
	// PrivacyTenant features are one tenant's own number (their reuse rate, their revert
	// count) — fine to report behind a pseudonym (T01..T18), never behind a raw tenant id.
	PrivacyTenant PrivacyClass = "tenant"
	// PrivacyContent features touch transcript or session content and must never leave the
	// box, pseudonymised or not.
	PrivacyContent PrivacyClass = "content"
)

// MissingBehavior says what a caller should do when a feature has no answer for a given
// decision point.
type MissingBehavior string

const (
	// MissingZero means an absent value is legitimately zero — e.g. a Stats-backed feature
	// before any gap has closed, which kvcache.History itself returns as (0, 0, LevelNone).
	MissingZero MissingBehavior = "zero"
	// MissingSkip means the caller must skip this decision point rather than guess — e.g. a
	// lag-1 feature on a conversation's very first request, which has no prior row at all.
	MissingSkip MissingBehavior = "skip"
	// MissingError means this feature should never be absent for a well-formed row; an
	// absence is a bug upstream, not a value to fall back from.
	MissingError MissingBehavior = "error"
)

// Cost is a coarse computational-cost label — enough to tell a caller whether a feature is
// free to compute for every row in a large replay or worth gating.
type Cost string

const (
	CostFieldRead Cost = "field_read" // one struct field, no lookup at all
	CostStatCell  Cost = "stat_cell"  // one History/ClusteredHistory lookup, amortised O(1)
	CostScan      Cost = "scan"       // a scan over the conversation-so-far
)

// Value is one feature's answer at one decision point. Present is false where Missing says
// the caller must treat this as absent rather than as the zero value of Num/Str.
type Value struct {
	Num     float64
	Str     string
	Present bool
}

// StatsContext is the accumulated, leak-free statistics an Extractor may lean on — never a
// table precomputed over the whole replay window, which is exactly the leak both
// accumulators exist to prevent (see kvcache.History's own doc comment). Replay owns one of
// each per run and feeds them gap-by-gap, in wall-clock order, as gaps close.
type StatsContext struct {
	// Hist is kvcache.History's own fallback ladder (user+model+bucket -> ... -> global),
	// the same accumulator kvcache.Simulate feeds.
	Hist *kvcache.History
	// Clustered additionally pins the request's own StopReasonCluster into every rung — see
	// ClusteredHistory's doc comment for why cluster is never dropped the way user/model/
	// bucket are.
	Clustered *ClusteredHistory
}

// Extractor computes one feature's value at a decision point from exactly the two things
// that decision point is allowed to see: o, the present-tense projection every strategy in
// this codebase already reads (kvcache.Observation — a type that cannot express a future
// field because it has none), and past, the SAME conversation's rows strictly before it.
//
// past is capped so its CAPACITY cannot be grown into the future either — Features.Extract
// hands it over as group[:i:i], the three-index slice form, so even past[:cap(past)] cannot
// reach group[i] or later. That is the actual enforcement mechanism, not a comment asking
// an implementer to behave: an Extractor that indexes past[len(past)] panics, and one that
// reslices past to a wider capacity gets nothing wider, because there is nothing wider to
// get.
//
// stats is the same StatsContext for every Extractor in one replay, advanced as the replay
// walks forward — see Replay's doc comment for why the walk must be in GLOBAL wall-clock
// order across every conversation, not conversation-by-conversation.
type Extractor func(o kvcache.Observation, past []*kvcache.Request, stats StatsContext) Value

// Feature is one entry in the feature registry: everything a reader needs to know before
// trusting a number this package produced.
type Feature struct {
	ID          string
	Description string
	// SourceTable and SourceColumn name where the raw value lives, e.g. "requests" /
	// "cache_miss_reason". Blank for a feature that is purely derived (stop_reason_cluster).
	SourceTable, SourceColumn string
	// AvailableAt is a human-readable timing note — WHEN this feature's value for a row is
	// actually knowable, relative to that row's own timestamp. "immediately" for a field the
	// provider's own response carries; a longer note for the three lagged fields, naming the
	// delay this package works around by lagging rather than by trying to time it exactly.
	AvailableAt  string
	Availability AvailabilityClass
	Missing      MissingBehavior
	Privacy      PrivacyClass
	Cost         Cost
	// Extract computes the feature. Required unless Availability is Impossible, in which
	// case it must be nil — see Features.Register.
	Extract Extractor
}

// Features is the feature registry.
type Features struct {
	mu    sync.RWMutex
	byID  map[string]Feature
	order []string // registration order, kept only so List()'s zero-op path is deterministic without a sort when nothing has raced ahead of it — List sorts anyway; see its doc comment.
}

// NewFeatures builds an empty feature registry.
func NewFeatures() *Features { return &Features{byID: map[string]Feature{}} }

// Register adds one feature. An Impossible feature must carry no Extractor — it exists to
// be named, never to be computed — and every other feature must carry one; a Feature that
// fails this check is a policy input pretending to be a documented ceiling, or an
// undocumented one pretending to be a fact, and either is refused rather than silently
// half-registered.
func (f *Features) Register(feat Feature) error {
	if feat.ID == "" {
		return fmt.Errorf("predictor: feature ID is required")
	}
	if feat.Availability == "" {
		return fmt.Errorf("predictor: feature %q: Availability is required", feat.ID)
	}
	switch {
	case feat.Availability == Impossible && feat.Extract != nil:
		return fmt.Errorf("predictor: feature %q is marked Impossible but supplies an "+
			"Extractor — an impossible feature must have none", feat.ID)
	case feat.Availability != Impossible && feat.Extract == nil:
		return fmt.Errorf("predictor: feature %q is not Impossible but has no Extractor", feat.ID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.byID[feat.ID]; dup {
		return fmt.Errorf("predictor: feature %q already registered", feat.ID)
	}
	f.byID[feat.ID] = feat
	f.order = append(f.order, feat.ID)
	return nil
}

// Get looks up one feature by ID.
func (f *Features) Get(id string) (Feature, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	feat, ok := f.byID[id]
	return feat, ok
}

// List returns every registered feature, sorted by ID — deterministic regardless of
// registration order or map iteration.
func (f *Features) List() []Feature {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Feature, 0, len(f.byID))
	for _, feat := range f.byID {
		out = append(out, feat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Extract builds the present-tense projection AND every registered feature's value at
// decision point i in group.
//
// group MUST be one conversation's rows — a single (user, session, model) key — in
// chronological (ts, id) order, with group[i] the row being decided about. Replay
// maintains exactly this invariant as it walks (see replay.go); a caller building group by
// hand should use kvcache.Derive followed by grouping on Request.Key(), sorted the same way
// Derive itself sorts.
//
// group[i] — "the request just served" — is Observation's own present tense, the same
// footing StopReason already stands on throughout kvcache. group[i+1:] is provably
// unreachable from here: it is never referenced by this function, and the past slice it
// hands to every Extractor is capped so even a reslice cannot widen it (see Extractor's own
// doc comment).
func (f *Features) Extract(group []*kvcache.Request, i int, stats StatsContext) (kvcache.Observation, map[string]Value) {
	row := group[i]
	o := kvcache.Observation{
		User: row.User, Conversation: row.ConversationID, Model: row.Model, RequestID: row.ID,
		Now: row.TS, HourUTC: row.HourUTC, Bucket: row.Bucket,
		CachedTokens: row.CachedContext, TTL: row.TTL, Turn: i + 1, StopReason: row.StopReason,
		Stats: stats.Hist,
	}
	if i > 0 && group[i-1].Key() == row.Key() {
		o.SinceLastMs = row.TS - group[i-1].TS
	}
	past := group[:i:i] // len == cap == i: group[i] and beyond are unreachable, even via past[:cap(past)]

	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]Value, len(f.byID))
	for id, feat := range f.byID {
		if feat.Extract == nil {
			out[id] = Value{}
			continue
		}
		out[id] = feat.Extract(o, past, stats)
	}
	return o, out
}
