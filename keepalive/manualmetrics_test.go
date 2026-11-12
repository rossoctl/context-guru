package keepalive

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Issue #1103: the manual holds leave counts and byte sizes behind, and no content.

// holdOnce records one request, so a hold is counted once (recordHold sends two).
func holdOnce(k *Keeper, key Key, at time.Time, body string) {
	pol := offPolicy
	pol.ManualHold = true
	k.Record(Record{Key: key, StartedAt: at, Body: []byte(body), Provider: bschemas.Anthropic,
		Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
		UsageOK: true, Policy: pol, Header: kaHeader(), Hold: fakeOf(k).hold(key)})
}

func cheap(fd *fakeDispatcher) {
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 1e-6, Output: 1e-6, CacheRead: 1e-6, CacheWrite: 1e-6}}})
}

func TestManualHoldEvictionByCountMovesTheCountCounter(t *testing.T) {
	k, _, clock := testKeeper(t)
	for i := 0; i <= ManualHoldMaxPerTenant; i++ {
		holdOnce(k, tenantKey("a", "s"+strconv.Itoa(i)), clock.now().Add(time.Duration(i)*time.Millisecond), kaBody)
	}
	s := k.ManualHoldSnapshot()
	if s.EvictedCount != 1 || s.EvictedBytes != 0 {
		t.Errorf("evicted count=%d bytes=%d, want 1 and 0", s.EvictedCount, s.EvictedBytes)
	}
	if len(s.Tenants) != 1 || s.Tenants[0].Count != ManualHoldMaxPerTenant {
		t.Errorf("tenants = %+v, want tenant a holding %d", s.Tenants, ManualHoldMaxPerTenant)
	}
}

func TestManualHoldEvictionByBytesMovesTheBytesCounterAndTheGauge(t *testing.T) {
	k, fd, clock := testKeeper(t)
	cheap(fd)
	body := padBody(20 << 20)
	holdOnce(k, tenantKey("a", "one"), clock.now(), body)
	if s := k.ManualHoldSnapshot(); s.EvictedBytes != 0 || len(s.Tenants) != 1 || s.Tenants[0].Bytes < 20<<20 {
		t.Fatalf("after one hold: %+v", s)
	}
	holdOnce(k, tenantKey("a", "two"), clock.now().Add(time.Second), body)
	s := k.ManualHoldSnapshot()
	if s.EvictedBytes != 1 || s.EvictedCount != 0 {
		t.Errorf("evicted bytes=%d count=%d, want 1 and 0", s.EvictedBytes, s.EvictedCount)
	}
	if len(s.Tenants) != 1 || s.Tenants[0].Count != 1 || s.Tenants[0].Bytes != int64(len(body)) {
		t.Errorf("tenants = %+v, want 1 hold of %d bytes", s.Tenants, len(body))
	}
}

func TestManualHoldGaugeFollowsAHoldAndItsRelease(t *testing.T) {
	k, _, clock := testKeeper(t)
	recordHold(k, tenantKey("a", "s"), clock.now(), kaBody, offPolicy, true)
	s := k.ManualHoldSnapshot()
	if len(s.Tenants) != 1 || s.Tenants[0].Count != 1 || s.Tenants[0].Bytes != int64(len(kaBody)) {
		t.Fatalf("after hold: %+v", s.Tenants)
	}
	clock.advance(ManualHoldMax + time.Second)
	k.expireManual(tenantKey("a", "s").id())
	if s := k.ManualHoldSnapshot(); len(s.Tenants) != 0 {
		t.Errorf("after release: %+v, want none", s.Tenants)
	}
}

func TestATooLargeBodyCountsAsRefused(t *testing.T) {
	k, fd, clock := testKeeper(t)
	cheap(fd)
	recordHold(k, sess1, clock.now(), padBody(MaxBodyBytes), kaPolicy(), true)
	if s := k.ManualHoldSnapshot(); s.TooLarge == 0 {
		t.Error("too_large counter did not move")
	}
	if s := k.ManualHoldSnapshot(); s.SizeCount != 0 {
		t.Error("a refused body must not reach the hold-size histogram")
	}
}

func TestTheHoldSizeHistogramRecordsTheBodySize(t *testing.T) {
	k, fd, clock := testKeeper(t)
	cheap(fd)
	holdOnce(k, tenantKey("a", "small"), clock.now(), kaBody)
	body := padBody(5 << 20)
	holdOnce(k, tenantKey("b", "mid"), clock.now(), body)
	s := k.ManualHoldSnapshot()
	if s.SizeCount != 2 {
		t.Errorf("count = %d, want 2", s.SizeCount)
	}
	if want := int64(len(kaBody) + len(body)); s.SizeSum != want {
		t.Errorf("sum = %d, want %d", s.SizeSum, want)
	}
	if s.Buckets[0] != 1 || s.Buckets[2] != 1 { // <=1 MiB and (4,8] MiB
		t.Errorf("buckets = %v", s.Buckets)
	}
}

func TestTwoLargeHoldsLogOnce(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	k, fd, clock := testKeeper(t)
	cheap(fd)
	body := padBody(9 << 20)
	holdOnce(k, tenantKey("a", "one"), clock.now(), body)
	if strings.Contains(buf.String(), "large manual-hold") {
		t.Fatal("logged for a single large body")
	}
	holdOnce(k, tenantKey("a", "two"), clock.now().Add(time.Second), body)
	holdOnce(k, tenantKey("a", "three"), clock.now().Add(2*time.Second), body)
	if n := strings.Count(buf.String(), "large manual-hold"); n != 1 {
		t.Errorf("logged %d times, want 1:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "xxxxxxxx") {
		t.Error("the log line carries body content")
	}
}
