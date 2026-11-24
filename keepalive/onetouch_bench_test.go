package keepalive

import (
	"strconv"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
)

// #1365. The dashboard asks per request. Reading the one record means one store read per thread of the asked
// session. The scan over the index is the one the keeper's own map had.
func BenchmarkOneHourTouchesReadsTheStore(b *testing.B) {
	k, _, clock := testKeeperWith(b, nil)
	st := store.NewMemory(store.Options{MaxEntries: 4 * MaxTurnKeys})
	now := clock.now()
	k.mu.Lock()
	for i := 0; i < MaxTurnKeys; i++ {
		sess := "s" + strconv.Itoa(i/16) // 16 threads per session, as the dashboard shows at most
		th := "th" + strconv.Itoa(i%16)
		alias := "al" + strconv.Itoa(i)
		apply.RecordCacheTouchAliasTier(st, alias, now.Add(-time.Minute).UnixMilli(), true)
		key := Key{Tenant: "t", Session: sess, Thread: th}
		k.oneHourThreads[key.id()] = oneHourThread{key: key, provider: bschemas.Anthropic, model: "m", st: st, alias: alias}
	}
	k.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := k.OneHourTouches("t", false, "s7"); len(got) != 16 {
			b.Fatalf("got %d threads, want 16", len(got))
		}
	}
}
