package kvcache

import (
	"reflect"
	"testing"
	"time"
)

// req is a terse builder for one row of the dataset.
func req(id int64, user, conv string, tsMs int64, model string) *Request {
	return &Request{ID: id, User: user, ConversationID: conv, TS: tsMs, Model: model,
		HourUTC: time.UnixMilli(tsMs).UTC().Hour(), Bucket: BucketAt(tsMs),
		CachedContext: 100_000, InputTokens: 100, OutputTokens: 50, TTL: TTL5m,
		TTLSource: TTLSourceConfigured, MissReason: "hit", Hit: true}
}

const base = int64(1_786_967_311_185) // a real production timestamp: 2026-08-17T11:48:31Z

// The successor of a request is the next request in the SAME conversation, and a conversation
// is (user, session) — never the session alone.
//
// Two accounts presenting the same session id is not hypothetical: the session id is
// client-supplied and the store's own indexes are tenant-leading for exactly this reason.
// Keyed on the session alone, this dataset derives a 1-second gap where the truth is that
// neither account came back at all.
func TestDeriveNeverCrossesAConversationBoundary(t *testing.T) {
	shared := "same-session-id"
	a := req(1, "tenant-a", shared, base, "m")
	b := req(2, "tenant-b", shared, base+1000, "m")
	c := req(3, "tenant-a", shared, base+60_000, "m")
	other := req(4, "tenant-a", "another", base+2000, "m")
	rows := []*Request{a, b, c, other}
	Derive(rows)

	if !a.HasNext || a.NextID != c.ID {
		t.Errorf("tenant-a's first request should be followed by its OWN next request (%d), got %d (has_next=%v)",
			c.ID, a.NextID, a.HasNext)
	}
	if got, _ := a.Idle(); got != 60*time.Second {
		t.Errorf("idle = %v, want 60s — tenant-b's request must not shorten it", got)
	}
	if b.HasNext {
		t.Errorf("tenant-b has one request; it must have no successor, got next=%d", b.NextID)
	}
	if other.HasNext {
		t.Error("a different conversation of the same user must not be spliced in")
	}
}

// The last request of a conversation has NO idle time. Not zero — zero reads as "it came
// back instantly", which is the opposite of what is known.
func TestFinalRequestHasNoIdleTimeRatherThanZero(t *testing.T) {
	a, b := req(1, "u", "s", base, "m"), req(2, "u", "s", base+30_000, "m")
	Derive([]*Request{a, b})
	if b.HasNext || b.IdleMs != nil {
		t.Errorf("final request carries has_next=%v idle=%v; want false/nil", b.HasNext, b.IdleMs)
	}
	if _, ok := b.Idle(); ok {
		t.Error("Idle() reported an idle time for a request with no successor")
	}
	if b.Within5m || b.Within1h {
		t.Error("a request with no successor was followed by nothing within any horizon")
	}
	if !a.HasNext || a.IdleMs == nil || *a.IdleMs != 30_000 {
		t.Errorf("the first request's idle = %v, want 30000 ms", a.IdleMs)
	}
}

// The two horizons are INCLUSIVE at their edge, and one millisecond past it is outside.
// Every percentage on the page rests on this comparison.
func TestFiveMinuteAndOneHourBoundariesAreExact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gapMs    int64
		w5m, w1h bool
	}{
		{"one ms under five minutes", 299_999, true, true},
		{"exactly five minutes", 300_000, true, true},
		{"one ms over five minutes", 300_001, false, true},
		{"one ms under an hour", 3_599_999, false, true},
		{"exactly one hour", 3_600_000, false, true},
		{"one ms over an hour", 3_600_001, false, false},
		{"a zero-length gap", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := req(1, "u", "s", base, "m"), req(2, "u", "s", base+tc.gapMs, "m")
			Derive([]*Request{a, b})
			if a.Within5m != tc.w5m || a.Within1h != tc.w1h {
				t.Errorf("gap %d ms -> within_5m=%v within_1h=%v; want %v/%v",
					tc.gapMs, a.Within5m, a.Within1h, tc.w5m, tc.w1h)
			}
		})
	}
}

// Timestamps tie: 9 of 12,635 consecutive pairs in the production corpus share a
// millisecond. The id breaks the tie, so the derivation is deterministic and a zero-length
// gap stays zero instead of becoming negative.
func TestTiedTimestampsAreOrderedByIDAndGiveAZeroGap(t *testing.T) {
	a, b := req(7, "u", "s", base, "m"), req(3, "u", "s", base, "m")
	rows := []*Request{a, b}
	Derive(rows)
	if !b.HasNext || b.NextID != 7 {
		t.Errorf("the lower id must come first: id 3 next = %d (has_next=%v)", b.NextID, b.HasNext)
	}
	if got, ok := b.Idle(); !ok || got != 0 {
		t.Errorf("tied timestamps give a zero gap, got %v (ok=%v)", got, ok)
	}
	if a.HasNext {
		t.Error("the higher id is last and has no successor")
	}
}

// Derive leaves the slice in WALL-CLOCK order, not grouped by conversation. The simulator
// replays it as given, and replaying one conversation to its end before starting the next
// would carry that conversation's whole future into the statistics every other decision sees.
func TestDeriveLeavesTheSetInChronologicalOrder(t *testing.T) {
	rows := []*Request{
		req(1, "u", "b", base+3000, "m"),
		req(2, "u", "a", base+1000, "m"),
		req(3, "u", "b", base+4000, "m"),
		req(4, "u", "a", base+2000, "m"),
	}
	Derive(rows)
	for i := 1; i < len(rows); i++ {
		if rows[i-1].TS > rows[i].TS {
			t.Fatalf("not chronological at %d: %d then %d", i, rows[i-1].TS, rows[i].TS)
		}
	}
}

func TestBucketsAreUTCSixHourBands(t *testing.T) {
	for hour, want := range map[int]Bucket{
		0: BucketNight, 5: BucketNight, 6: BucketMorning, 11: BucketMorning,
		12: BucketAfternoon, 17: BucketAfternoon, 18: BucketEvening, 23: BucketEvening,
		-1: BucketNight, 99: BucketNight,
	} {
		if got := BucketOf(hour); got != want {
			t.Errorf("BucketOf(%d) = %q, want %q", hour, got, want)
		}
	}
	// And the instant form agrees with the hour form, in UTC.
	ts := time.Date(2026, 8, 17, 20, 30, 0, 0, time.UTC).UnixMilli()
	if got := BucketAt(ts); got != BucketEvening {
		t.Errorf("BucketAt(20:30 UTC) = %q, want evening", got)
	}
}

func TestTTLLifetimes(t *testing.T) {
	if TTL5m.Lifetime() != 5*time.Minute || TTL1h.Lifetime() != time.Hour || TTLNone.Lifetime() != 0 {
		t.Fatal("a tier's lifetime is wrong")
	}
	if TTL("ephemeral_10m").Valid() {
		t.Error("an unrecognised tier must not read as valid; the simulator treats it as no cache")
	}
}

// The Observation is defined by what is ABSENT from it. This walks its numeric fields by
// reflection and asserts that no decision carries a fact about a request that had not
// happened yet.
//
// A field-by-field check rather than a comment, because the leak it prevents is invisible on
// screen: a predictor that can see the gap it is predicting reports savings nobody can reach
// in production, and the page would look completely fine.
func TestStrategiesCannotSeeTheFuture(t *testing.T) {
	// Distinctive gaps, so an accidental copy is unmistakable rather than a coincidence.
	rows := []*Request{
		req(11, "u", "s", base, "m"),
		req(12, "u", "s", base+1_234_567, "m"),
		req(13, "u", "s", base+1_234_567+7_654_321, "m"),
	}
	Derive(rows)

	spy := &recorder{}
	Simulate(rows, spy, Config{Prices: testPrices()})
	if len(spy.seen) != len(rows) {
		t.Fatalf("the strategy was consulted %d times, want %d", len(spy.seen), len(rows))
	}
	for i, snap := range spy.seen {
		// Everything about a request LATER than the one being served is the future.
		future := map[int64]string{}
		for _, later := range rows[i+1:] {
			future[later.TS] = "a later request's timestamp"
			future[later.ID] = "a later request's id"
			if later.IdleMs != nil {
				future[*later.IdleMs] = "a later request's idle duration"
			}
		}
		if r := rows[i]; r.IdleMs != nil {
			future[*r.IdleMs] = "THIS request's own idle duration, which is its future"
		}
		// Facts that are legitimately present-tense must not be flagged by a value collision.
		for _, ok := range []int64{rows[i].TS, rows[i].ID, int64(rows[i].HourUTC),
			rows[i].CachedContext} {
			delete(future, ok)
		}
		v := reflect.ValueOf(snap.o)
		for f := 0; f < v.NumField(); f++ {
			fv := v.Field(f)
			if fv.Kind() != reflect.Int64 && fv.Kind() != reflect.Int {
				continue
			}
			if what, bad := future[fv.Int()]; bad {
				t.Errorf("decision %d field %s = %d, which is %s", i,
					v.Type().Field(f).Name, fv.Int(), what)
			}
		}
		// SinceLastMs is the gap that has ALREADY closed, and nothing else.
		want := int64(0)
		if i > 0 && rows[i-1].Key() == rows[i].Key() {
			want = rows[i].TS - rows[i-1].TS
		}
		if snap.o.SinceLastMs != want {
			t.Errorf("decision %d SinceLastMs = %d, want %d (the gap that had already closed)",
				i, snap.o.SinceLastMs, want)
		}
	}
	// And the statistics AS THEY WERE at each decision: the first decision has nothing to
	// know, because no gap has closed yet. A precomputed table over the window would already
	// hold both gaps here.
	if spy.seen[0].n5 != 0 || spy.seen[0].level != LevelNone {
		t.Errorf("the first decision already saw %d closed gaps at level %s; the window's "+
			"future has leaked into its own history", spy.seen[0].n5, spy.seen[0].level)
	}
	if spy.seen[1].n5 != 1 {
		t.Errorf("the second decision saw %d closed gaps, want exactly the one behind it",
			spy.seen[1].n5)
	}
}

// snapshot is what the strategy could see AT the decision, captured then rather than read
// back afterwards — History is a live accumulator, so a pointer held from decision 1 and
// read at the end of the replay reports the end state and proves nothing.
type snapshot struct {
	o     Observation
	n5    int
	p5    float64
	level string
}

// recorder is a strategy that records what it was shown and always caches at five minutes.
type recorder struct{ seen []snapshot }

func (r *recorder) Name() string { return "recorder" }
func (r *recorder) Decide(o Observation) Action {
	s := snapshot{o: o, level: LevelNone}
	if o.Stats != nil {
		s.p5, s.n5, s.level = o.Stats.ReuseWithin(o.User, o.Model, o.Bucket, Horizon5m)
	}
	r.seen = append(r.seen, s)
	return ActionWrite5m
}

// A gap becomes history only when it CLOSES. The decision before a long gap still sees the
// short gaps that preceded it, and does not see the long one.
func TestHistoryOnlyCarriesGapsThatHaveAlreadyClosed(t *testing.T) {
	// Seven 10-second gaps, then a two-hour one.
	var rows []*Request
	ts := base
	for i := int64(1); i <= 8; i++ {
		rows = append(rows, req(i, "u", "s", ts, "m"))
		ts += 10_000
	}
	rows = append(rows, req(9, "u", "s", ts+2*3_600_000, "m"))
	Derive(rows)

	spy := &recorder{}
	Simulate(rows, spy, Config{Prices: testPrices()})
	// The decision taken ON request 8 — the one whose span is the two-hour gap — sees the
	// seven short gaps and nothing else.
	got := spy.seen[7]
	if got.n5 != 7 {
		t.Fatalf("decision 8 saw %d closed gaps, want 7", got.n5)
	}
	if got.p5 != 1 {
		t.Errorf("all seven closed gaps were 10 s, so P(within 5m) = 1, got %.3f", got.p5)
	}
	if got.level != LevelUserBucket {
		t.Errorf("seven gaps in one cell should answer at %s, got %s", LevelUserBucket, got.level)
	}
	// The LAST decision sees eight, because by then the long gap has closed too — and its
	// probability has dropped, which is the whole point of accumulating rather than precomputing.
	last := spy.seen[len(spy.seen)-1]
	if last.n5 != 8 {
		t.Errorf("the final decision saw %d gaps, want 8 (the long one has now closed)", last.n5)
	}
	if last.p5 >= 1 {
		t.Errorf("the two-hour gap did not move P(within 5m): still %.3f", last.p5)
	}
}

// The fallback chain: a user with too little history of their own is decided on their
// model's statistics, then on the service's, and the level says which.
func TestStatsFallBackFromUserToModelToGlobal(t *testing.T) {
	h := NewHistory()
	// Six gaps for one user/model/bucket clears the floor for that cell.
	for i := 0; i < minCell; i++ {
		h.Observe("busy", "m1", BucketMorning, 30*time.Second)
	}
	if _, n, level := h.ReuseWithin("busy", "m1", BucketMorning, Horizon5m); level != LevelUserBucket || n != minCell {
		t.Errorf("a full cell must answer at its own level, got %s n=%d", level, n)
	}
	// A different bucket for the same user/model still has the user+model cell behind it.
	if _, _, level := h.ReuseWithin("busy", "m1", BucketEvening, Horizon5m); level != LevelUserModel {
		t.Errorf("an empty bucket must fall back to user+model, got %s", level)
	}
	// A brand-new user on a known model falls back to the model.
	if _, _, level := h.ReuseWithin("new", "m1", BucketNight, Horizon5m); level != LevelModel {
		t.Errorf("a new user on a known model must fall back to the model, got %s", level)
	}
	// A brand-new user on a brand-new model falls back to the service.
	if _, _, level := h.ReuseWithin("new", "unseen", BucketNight, Horizon5m); level != LevelGlobal {
		t.Errorf("a new user on an unseen model must fall back to global, got %s", level)
	}
	// And nothing at all is LevelNone, never a zero probability.
	empty := NewHistory()
	if p, n, level := empty.ReuseWithin("u", "m", BucketNight, Horizon5m); level != LevelNone || n != 0 || p != 0 {
		t.Errorf("an empty history must answer %s with n=0, got %s n=%d p=%v",
			LevelNone, level, n, p)
	}
	// A cell under the floor does not get to speak for itself.
	thin := NewHistory()
	for i := 0; i < minCell-1; i++ {
		thin.Observe("u", "m", BucketNight, time.Second)
	}
	if _, _, level := thin.ReuseWithin("u", "m", BucketNight, Horizon5m); level == LevelUserBucket {
		t.Errorf("a cell with %d observations answered at its own level; the floor is %d",
			minCell-1, minCell)
	}
	if got, _, _ := h.MedianIdle("busy", "m1", BucketMorning); got != 30*time.Second {
		t.Errorf("median idle = %v, want 30s", got)
	}
}
