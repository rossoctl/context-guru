package proxy

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
)

// The OOM (#639) was request bodies pinned by something that outlived the request, and a replay
// WITHOUT a real dash.Recorder cannot see it. So this holds 200 x ~1.5 MiB manual-only threads through
// a host with a real recorder, lets them all expire, and checks that the heap is back where it started
// and that the keeper counts nothing.
func TestManualHoldLeavesNoBodyBehindAfterItExpires(t *testing.T) {
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(nil, nil, metrics.NewAggregator(), Options{Dashboard: rec, Prices: fixedPricer{}})
	t.Cleanup(h.Close)
	clock := &testClock{at: time.Now()}
	useFakeUpstream(h, &fakeSender{}, clock)

	heap := func() uint64 {
		for i := 0; i < 3; i++ {
			runtime.GC()
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := heap()

	const n, size = 200, 1536 << 10
	pad := strings.Repeat("x", size)
	for i := 0; i < n; i++ {
		body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+pad+`"`, 1)
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		r.Header.Set(ManualHoldHeader, "1")
		sid := "mem-" + string(rune('a'+i%26)) + strings.Repeat("z", i/26)
		for j := 0; j < 2; j++ {
			h.keeper.record(&Tenancy{ID: "", Pipe: components.NewPipeline(nil, nil)}, sid, "", clock.now().Add(time.Duration(j)*time.Second),
				[]byte(body), upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
				http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	held := h.keeper.Stats().Live
	if held == 0 {
		t.Fatal("nothing was held, so this proves nothing")
	}
	peak := heap()
	if peak < base+uint64(held)*size/2 {
		t.Fatalf("held %d threads but the heap grew only %d bytes: the test does not measure the bodies", held, peak-base)
	}

	clock.advance(keepalive.ManualHoldMax + time.Second)
	h.keeper.Sweep(clock.now())

	if st := h.keeper.Stats(); st.Live != 0 {
		t.Fatalf("%d entries still held after expiry", st.Live)
	}
	after := heap()
	if after > base+16<<20 { // everything held was ~300 MiB; allow 16 MiB of unrelated growth
		t.Errorf("heap %d MiB after expiry vs %d MiB before: %d MiB of held bodies is still reachable",
			after>>20, base>>20, (after-base)>>20)
	}
	t.Logf("held %d threads: heap %d MiB at peak, %d MiB after expiry, %d MiB at start", held, peak>>20, after>>20, base>>20)
}
