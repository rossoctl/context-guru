package predictor

import (
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// base is a real production timestamp, the same one kvcache's own test suite anchors on
// (2026-08-17T11:48:31Z), so a stray deadline off by one field reads as familiar rather
// than as a fresh coincidence to debug.
const base = int64(1_786_967_311_185)

// req is a terse builder for one row, mirroring kvcache_test.go's own helper of the same
// name. cachedTokens/reverts/expands/missReason/stopReason are the fields this package's
// features actually read; everything else takes a fixed, uninteresting default.
func req(id int64, user, conv string, tsMs int64, model string, cachedTokens int64, stopReason, missReason string, reverts, expands int) *kvcache.Request {
	return &kvcache.Request{
		ID: id, User: user, ConversationID: conv, TS: tsMs, Model: model,
		HourUTC: time.UnixMilli(tsMs).UTC().Hour(), Bucket: kvcache.BucketAt(tsMs),
		CachedContext: cachedTokens, InputTokens: 100, OutputTokens: 50, TTL: kvcache.TTL5m,
		TTLSource: kvcache.TTLSourceConfigured, StopReason: stopReason, MissReason: missReason,
		Reverts: reverts, Expands: expands,
	}
}

// derive is Derive, called on a copy so a test's own slice of pointers is not silently
// reordered out from under it between two calls that both expect the input order.
func derive(rows []*kvcache.Request) []*kvcache.Request {
	out := make([]*kvcache.Request, len(rows))
	copy(out, rows)
	kvcache.Derive(out)
	return out
}

// stubPredictor answers a fixed probability for every call, ok as configured — the same
// shape kvcache's own tests use to plug a Predictor in without a real fit.
type stubPredictor struct {
	p  float64
	ok bool
}

func (s stubPredictor) ReuseProbability(kvcache.Observation, time.Duration) (float64, bool) {
	return s.p, s.ok
}
