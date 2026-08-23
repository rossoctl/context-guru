// Package kvcache is the KV-cache TTL domain: the per-request analysis dataset the
// dashboard's KV-cache page reads, the pricing model for the six things a prompt-cache
// policy can spend money on, the Strategy interface a TTL policy (or a future learned
// predictor) implements, and the historical replay simulator that scores one strategy
// against another.
//
// It is a PLAIN domain package, importable from anywhere, and it is deliberately not
// inside `dash`:
//
//   - A strategy is a decision the PROXY will eventually have to make on the request
//     path, not a thing the dashboard owns. Defining it inside the observability layer
//     would mean the request path had to import the dashboard to ask what to do.
//   - Nothing here touches SQL, HTTP or the DOM. `dash` reads the rows and hands them
//     over; this package does the arithmetic. That is the same split the rest of this
//     repo keeps — see the "NO ARITHMETIC HERE" rule at the top of the keep-alive tab's
//     renderer, which this package is the server-side other half of.
//
// # Units, and they are stated once
//
// Every timestamp is epoch MILLISECONDS and every instant is UTC. Every duration in a
// struct field is milliseconds and says so in its name; every duration in an argument is
// a time.Duration. Every price is USD PER TOKEN (the operator's price list is per million
// tokens and is converted before it reaches here — see internal/modelinfo.Table).
//
// There is no per-user timezone anywhere in the store, so "time of day" is UTC and every
// label that carries it says UTC. Inventing a local timezone from a tenant id would be a
// fabricated measurement, which is the failure this repo has hit repeatedly.
//
// # The one rule the simulator exists to hold shut
//
// A strategy sees an Observation, and an Observation carries NO future information. The
// next request's timestamp is not on it, its idle duration is not on it, and the
// historical statistics attached to it are accumulated from gaps that had ALREADY CLOSED
// at the decision instant. The real next-request time is used exactly once — to SCORE the
// decision after it has been made. TestStrategiesCannotSeeTheFuture is the assertion.
package kvcache

import (
	"sort"
	"time"
)

// TTL is a prompt-cache lifetime tier, spelled the way the provider's usage block spells
// it — the same vocabulary as the `cache_ttl` column, so a row's configured tier and a
// strategy's chosen tier are the same type and cannot be compared by accident.
type TTL string

// The three tiers, and ” is a THIRD answer rather than a synonym for 5 minutes: a request
// carrying no cache_control at all is not a 5-minute request, and folding the two together
// is how a page comes to report that everything is cached.
const (
	TTLNone TTL = ""
	TTL5m   TTL = "ephemeral_5m"
	TTL1h   TTL = "ephemeral_1h"
)

// Lifetime is how long an entry at this tier survives without being touched.
func (t TTL) Lifetime() time.Duration {
	switch t {
	case TTL5m:
		return 5 * time.Minute
	case TTL1h:
		return time.Hour
	}
	return 0
}

// Label is the tier in the words a person reads.
func (t TTL) Label() string {
	switch t {
	case TTL5m:
		return "5m"
	case TTL1h:
		return "1h"
	}
	return "none"
}

// Valid reports whether this is a tier this package knows. An unrecognised value from the
// store is carried through as-is by the dataset and treated as TTLNone by the simulator,
// which is the fail-open direction: an unknown tier must not be silently priced as 1h.
func (t TTL) Valid() bool { return t == TTLNone || t == TTL5m || t == TTL1h }

// The two horizons the whole page is about: the provider's default lifetime, and the long
// tier. Named constants because they appear in a histogram edge, a summary card, a
// strategy threshold and a cost formula, and four literals would eventually disagree.
const (
	Horizon5m = 5 * time.Minute
	Horizon1h = time.Hour
)

// Bucket is a coarse time-of-day label, in UTC.
type Bucket string

// The four buckets. Six-hour bands rather than 24 hours: the per-user statistics a
// strategy leans on need enough requests per cell to mean anything, and on the production
// corpus 24 cells per user leaves most of them empty. The hour itself is still on every
// row (Request.HourUTC) for anyone who wants the finer view.
const (
	BucketNight     Bucket = "night"     // 00:00–05:59 UTC
	BucketMorning   Bucket = "morning"   // 06:00–11:59 UTC
	BucketAfternoon Bucket = "afternoon" // 12:00–17:59 UTC
	BucketEvening   Bucket = "evening"   // 18:00–23:59 UTC
)

// Buckets is every bucket in clock order, for a filter control or a group-by.
var Buckets = []Bucket{BucketNight, BucketMorning, BucketAfternoon, BucketEvening}

// BucketOf places one UTC hour in its bucket. An hour OUTSIDE 0..23 lands in night rather
// than in whichever band its arithmetic happens to fall into: the input comes from strftime
// and from a JSON query parameter, so an out-of-range value is a caller error, and quietly
// filing hour 99 under "afternoon" would be a fabricated label.
func BucketOf(hourUTC int) Bucket {
	if hourUTC < 0 || hourUTC > 23 {
		return BucketNight
	}
	switch {
	case hourUTC >= 18:
		return BucketEvening
	case hourUTC >= 12:
		return BucketAfternoon
	case hourUTC >= 6:
		return BucketMorning
	}
	return BucketNight
}

// BucketAt places an instant in its bucket, in UTC.
func BucketAt(tsMs int64) Bucket {
	return BucketOf(time.UnixMilli(tsMs).UTC().Hour())
}

// Conversation identifies one trajectory, and it is a PAIR.
//
// A session id is client-supplied, so two accounts can present the same one — by accident
// or on purpose. Keying a conversation on the session id alone would splice two accounts'
// requests into one interleaved trajectory and derive idle gaps across the join, which is
// both a wrong measurement and a cross-account read. The store's own indexes and its
// tool-inventory GC trigger are tenant-leading for exactly this reason.
type Conversation struct {
	User         string
	Conversation string
}

// Request is one historical request as the analysis dataset sees it: what was billed, what
// tier it asked for, and — derived, never stored — when the same conversation came back.
//
// The JSON names are the wire contract of /api/kvcache/rows. Idle is a POINTER: a request
// with no successor has no idle time, and 0 would read as "it came back instantly", which
// is the single most misleading value this dataset could carry. `null` is the only honest
// encoding, and HasNext is the flag a filter uses so nobody has to test a pointer.
type Request struct {
	// Identity. User is the owning tenant and ConversationID the session/trajectory.
	ID             int64  `json:"id"`
	User           string `json:"user"`
	ConversationID string `json:"conversation_id"`
	TS             int64  `json:"ts"` // epoch ms, UTC
	HourUTC        int    `json:"hour_utc"`
	Bucket         Bucket `json:"bucket"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	Agent          string `json:"agent"`

	// The billed token tiers, exactly as the provider reported them.
	InputTokens  int64 `json:"input_tokens"` // fresh, uncached input
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
	// CacheWrite1h is the part of CacheWrite the provider billed at the one-hour tier. It
	// is the ONLY evidence that a requested 1h was honoured rather than silently
	// downgraded, which on this gateway depends on the model.
	CacheWrite1h int64 `json:"cache_write_1h_tokens"`
	// CachedContext is the billed prefix: read + write. The size of the entry that exists
	// after this request, and the number every ping and every re-creation is priced on.
	// NOT tokens_before, which is message text only and runs a median 3.4x low.
	CachedContext int64 `json:"cached_context_tokens"`

	// TTL is the tier this request asked for, and TTLSource says how that was known:
	// "configured" when the row recorded its own cache_ttl, "observed" when the tier was
	// reconstructed from the provider's 1h write counter on a row written before that
	// column existed, "unknown" when neither could answer. A page that cannot tell those
	// apart reports "everything is 5m" over history that recorded no tier at all.
	TTL       TTL    `json:"ttl"`
	TTLSource string `json:"ttl_source"`

	// Hit is whether the provider served this request's prefix from cache, and MissReason
	// its own classification (cold_start|ttl_expiry|prefix_change|unknown|hit).
	Hit        bool   `json:"hit"`
	MissReason string `json:"miss_reason"`

	// The derived half. NextTS is the next request IN THE SAME CONVERSATION, chronologically;
	// HasNext is false on the last request of a conversation and IdleMs is then nil.
	NextTS   int64  `json:"next_ts,omitempty"`
	NextID   int64  `json:"next_id,omitempty"`
	HasNext  bool   `json:"has_next"`
	IdleMs   *int64 `json:"idle_ms"`
	Within5m bool   `json:"within_5m"`
	Within1h bool   `json:"within_1h"`

	// UpstreamMs is how long the provider took, in milliseconds. 0 means NOT RECORDED, and
	// the latency differential below skips such rows rather than averaging a zero in. It is
	// the only field here that is used for anything but money: a cache miss re-reads a whole
	// prefix, and the window's own hit/miss means are what turns an avoided miss into an
	// avoided delay.
	UpstreamMs float64 `json:"upstream_ms"`

	// CostUSD is what this request was billed, priced at write time from the rates that
	// were in force. CostKnown is false where the row's token accounting was incomplete —
	// then the cost is UNKNOWN, never zero.
	CostUSD   float64 `json:"cost_usd"`
	CostKnown bool    `json:"cost_known"`

	// KeepAlive marks a row that IS a ping context-guru sent on its own initiative. Such
	// rows are excluded from the dataset by default (they are not the user's traffic and
	// they would break every idle gap they sit inside); the field is here so a caller that
	// asks for them can see which ones they are.
	KeepAlive bool `json:"keepalive,omitempty"`
}

// TTLSource values.
const (
	TTLSourceConfigured = "configured"
	TTLSourceObserved   = "observed"
	TTLSourceUnknown    = "unknown"
)

// Key is this request's conversation.
func (r *Request) Key() Conversation {
	return Conversation{User: r.User, Conversation: r.ConversationID}
}

// Idle is the idle duration and whether there is one at all. The only way to read it: a
// caller that dereferences IdleMs without checking HasNext is the bug this returns two
// values to prevent.
func (r *Request) Idle() (time.Duration, bool) {
	if !r.HasNext || r.IdleMs == nil {
		return 0, false
	}
	return time.Duration(*r.IdleMs) * time.Millisecond, true
}

// Derive fills the chronological half of the dataset in place: NextTS, NextID, HasNext,
// IdleMs, Within5m and Within1h.
//
// Three properties, each of which is a way this has been got wrong before:
//
//   - Grouped by CONVERSATION, which is (user, session). A later request from another
//     conversation is never the successor of this one, whatever its timestamp, because the
//     provider's entry it would reuse is a different entry. The one exception the data
//     model would allow — a shared, byte-identical prefix across sessions — is not
//     recorded anywhere in the store, so it is not assumed here.
//   - Sorted by (ts, id). Timestamps tie: on the production corpus 9 of 12,635 consecutive
//     pairs share a millisecond. The id breaks the tie deterministically, so the same
//     window derives the same dataset twice, and a zero-length gap stays zero rather than
//     becoming negative.
//   - The LAST request of a conversation gets HasNext=false and a nil IdleMs. It is not a
//     request with an idle time of zero, and it is not a request that was never followed —
//     it is a request whose successor is outside the window (or has not happened yet), and
//     every aggregate below counts it in its own bucket.
//
// Requests is sorted in place.
func Derive(reqs []*Request) {
	sort.SliceStable(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.User != b.User {
			return a.User < b.User
		}
		if a.ConversationID != b.ConversationID {
			return a.ConversationID < b.ConversationID
		}
		if a.TS != b.TS {
			return a.TS < b.TS
		}
		return a.ID < b.ID
	})
	for i, r := range reqs {
		r.HasNext, r.IdleMs, r.NextTS, r.NextID = false, nil, 0, 0
		r.Within5m, r.Within1h = false, false
		if i+1 >= len(reqs) {
			continue
		}
		n := reqs[i+1]
		if n.Key() != r.Key() {
			continue
		}
		idle := n.TS - r.TS
		if idle < 0 {
			idle = 0 // cannot happen after the sort above; clamped rather than trusted
		}
		r.HasNext, r.NextTS, r.NextID = true, n.TS, n.ID
		v := idle
		r.IdleMs = &v
		r.Within5m = time.Duration(idle)*time.Millisecond <= Horizon5m
		r.Within1h = time.Duration(idle)*time.Millisecond <= Horizon1h
	}
	// Restore chronological order across the whole set: the simulator replays in wall-clock
	// order, and leaving the slice grouped by conversation would replay one conversation to
	// its end before starting the next — which is exactly the leak the Observation is
	// designed to prevent, since the statistics would then carry one user's whole future.
	sort.SliceStable(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.TS != b.TS {
			return a.TS < b.TS
		}
		return a.ID < b.ID
	})
}
