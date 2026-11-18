package keepalive

import (
	"log/slog"
	"sort"
)

// Metadata-only measures of the manual holds (issue #1103). They hold counts and byte sizes only:
// never a body, a key or a credential. The host exports them; it computes nothing.

// Why a manual hold was evicted.
const (
	EvictReasonBytes = "bytes" // the tenant's held bytes passed ManualHoldMaxBytesPerTenant
	EvictReasonCount = "count" // the tenant's held entries passed ManualHoldMaxPerTenant
)

// ManualHoldBucketsMiB are the upper bounds of the body-size histogram, in MiB.
var ManualHoldBucketsMiB = [...]int64{1, 4, 8, 16, 32}

// LargeManualHoldBytes is the size from which a held body counts as "large" for the log line.
const LargeManualHoldBytes = 8 << 20

// manualMetrics is guarded by Keeper.mu.
type manualMetrics struct {
	evictedBytes, evictedCount int64
	tooLarge                   int64
	// bucket[i] counts bodies <= ManualHoldBucketsMiB[i]; the last cell is the +Inf bucket.
	bucket    [len(ManualHoldBucketsMiB) + 1]int64
	sizeSum   int64
	sizeCount int64
	// pair is set for a tenant whose 2-large-bodies line was logged and whose count has not fallen
	// below 2 since, so a replaced thread or a third body does not log again.
	pair map[string]bool
}

// ManualHoldTenant is one tenant's current manual holds.
type ManualHoldTenant struct {
	Tenant string
	Bytes  int64
	Count  int
}

// ManualHoldSnapshot is one scrape's reading. Buckets are NOT cumulative.
type ManualHoldSnapshot struct {
	EvictedBytes, EvictedCount int64
	TooLarge                   int64
	Buckets                    [len(ManualHoldBucketsMiB) + 1]int64
	SizeSum, SizeCount         int64
	Tenants                    []ManualHoldTenant // sorted by tenant id; only tenants holding now
}

// ManualHoldSnapshot reads the manual-hold measures. A nil Keeper reads as zero.
func (k *Keeper) ManualHoldSnapshot() ManualHoldSnapshot {
	if k == nil {
		return ManualHoldSnapshot{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	s := ManualHoldSnapshot{
		EvictedBytes: k.mm.evictedBytes, EvictedCount: k.mm.evictedCount, TooLarge: k.mm.tooLarge,
		Buckets: k.mm.bucket, SizeSum: k.mm.sizeSum, SizeCount: k.mm.sizeCount,
	}
	by := map[string]*ManualHoldTenant{}
	for _, e := range k.live {
		if !e.manualOnly {
			continue
		}
		t := by[e.tenant]
		if t == nil {
			t = &ManualHoldTenant{Tenant: e.tenant}
			by[e.tenant] = t
		}
		t.Bytes += int64(len(e.body))
		t.Count++
	}
	for _, t := range by {
		s.Tenants = append(s.Tenants, *t)
	}
	sort.Slice(s.Tenants, func(i, j int) bool { return s.Tenants[i].Tenant < s.Tenants[j].Tenant })
	return s
}

// recordManualHoldLocked records the body size of a new manual hold and logs when the tenant now
// holds 2 bodies of LargeManualHoldBytes or more. k.mu must be held. It runs before the caps evict.
func (k *Keeper) recordManualHoldLocked(e *kaEntry) {
	size := int64(len(e.body))
	i := 0
	for i < len(ManualHoldBucketsMiB) && size > ManualHoldBucketsMiB[i]<<20 {
		i++
	}
	k.mm.bucket[i]++
	k.mm.sizeSum += size
	k.mm.sizeCount++
	var large int
	var sizes []int64
	for _, o := range k.live {
		if o.manualOnly && o.tenant == e.tenant && int64(len(o.body)) >= LargeManualHoldBytes {
			large++
			sizes = append(sizes, int64(len(o.body)))
		}
	}
	if large < 2 {
		delete(k.mm.pair, e.tenant)
		return
	}
	if !k.mm.pair[e.tenant] {
		if k.mm.pair == nil {
			k.mm.pair = map[string]bool{}
		}
		k.mm.pair[e.tenant] = true
		sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
		slog.Warn("context-guru: a tenant holds 2 or more large manual-hold bodies at once; the per-tenant cap may evict one (#1103)",
			"tenant", e.tenant, "body_bytes", sizes, "tenant_cap_bytes", int64(ManualHoldMaxBytesPerTenant))
	}
}
