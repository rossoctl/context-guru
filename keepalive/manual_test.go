package keepalive

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
)

// reportSink is an Emitter that keeps every keep-alive report, so a test can see exactly what a
// host's recorder would have been told.
type reportSink struct {
	mu   sync.Mutex
	reps []components.KeepAliveReport
}

func (*reportSink) Component(components.Report) {}
func (*reportSink) Run(components.RunReport)    {}
func (s *reportSink) KeepAlivePing(r components.KeepAliveReport) {
	s.mu.Lock()
	s.reps = append(s.reps, r)
	s.mu.Unlock()
}
func (s *reportSink) all() []components.KeepAliveReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]components.KeepAliveReport(nil), s.reps...)
}

// recordWithSink records the two turns a pingable thread needs, reporting through sink.
func recordWithSink(k *Keeper, sink *reportSink, key Key, at time.Time) {
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: key, StartedAt: at.Add(d), Body: []byte(kaBody), Provider: "anthropic",
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, Policy: kaPolicy(), Header: kaHeader(), Emitter: sink, Hold: fakeOf(k).hold(key)})
	}
}

func manualKeeper(t *testing.T) (*Keeper, *reportSink, *testClock) {
	t.Helper()
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	sink := &reportSink{}
	recordWithSink(k, sink, sess1, clock.now())
	return k, sink, clock
}

// One manual ping is ONE request of the ordinary ping shape: the recorded body with max_tokens 1
// and stream false, so it reads the same cache entry the automatic ping would.
func TestPingNowSendsOneOrdinaryPingAndReportsItAsManual(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	fd := fakeOf(k)

	res, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second, RewriteUSD: 0.18})
	if err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	if fd.n() != 1 {
		t.Fatalf("sent %d requests, want exactly 1", fd.n())
	}
	sent := fd.sent[0]
	if gjson.GetBytes(sent.Body, "max_tokens").Int() != 1 || gjson.GetBytes(sent.Body, "stream").Bool() {
		t.Errorf("not an ordinary ping body: %s", sent.Body)
	}
	if res.Status != http.StatusOK || res.Usage.CacheRead != 48576 || !res.StartedAt.Equal(clock.now()) {
		t.Errorf("result = %+v", res)
	}
	if res.CostUSD <= 0 || !res.Priced {
		t.Errorf("a priced ping must report its cost: %+v", res)
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	r := reps[0]
	if !r.Manual || r.Strategy != "manual" || r.RewriteUSD != 0.18 || r.CostUSD != res.CostUSD {
		t.Errorf("report = %+v, want Manual, strategy \"manual\", the avoided rewrite and the ping's cost", r)
	}
}

// A manual ping is not one of the K automatic pings: it must neither spend them nor leave the
// entry due sooner. It DOES refresh the provider's entry, so the automatic clock restarts at it.
func TestPingNowDoesNotSpendAutomaticPingsAndRestartsTheClock(t *testing.T) {
	k, _, clock := manualKeeper(t)
	fd := fakeOf(k)
	clock.advance(100 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if e, _ := k.Entry(sess1); e.Pings != 0 {
		t.Errorf("entry pings = %d, a manual ping must not count against K", e.Pings)
	}
	// 280 s after the RECORD would be due; the manual ping at +100 s moved it to +380 s.
	if n := k.Sweep(clock.advance(181 * time.Second)); n != 0 {
		t.Fatalf("automatic ping fired %d times only 181 s after a manual one", n)
	}
	if n := k.Sweep(clock.advance(100 * time.Second)); n != 1 {
		t.Fatalf("automatic ping did not fire 281 s after the manual one (%d)", n)
	}
	if fd.n() != 2 {
		t.Errorf("sent %d, want manual + one automatic", fd.n())
	}
}

func TestPingNowRefusesWhatTheKeeperDoesNotHold(t *testing.T) {
	k, _, _ := manualKeeper(t)
	if _, err := k.PingNow(ManualRequest{Key: Key{Tenant: "t1", Session: "other"}}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("an unknown session = %v, want ErrNotHeld", err)
	}
	// Another tenant's key addresses nothing: the key carries the tenant.
	if _, err := k.PingNow(ManualRequest{Key: Key{Tenant: "t2", Session: sess1.Session}}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("another tenant's session = %v, want ErrNotHeld", err)
	}
	if fakeOf(k).n() != 0 {
		t.Error("a refused manual ping sent a request")
	}
}

func TestPingNowRefusesAThreadTheKeeperStopped(t *testing.T) {
	k, _, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil })
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err == nil {
		t.Fatal("a refused ping must be an error")
	}
	if _, err := k.PingNow(ManualRequest{Key: sess1}); !errors.Is(err, ErrNotHeld) {
		t.Errorf("after the upstream refused, a second manual ping = %v, want ErrNotHeld", err)
	}
}

func TestPingNowIsRateLimitedPerThread(t *testing.T) {
	k, _, clock := manualKeeper(t)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Second)
	_, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second})
	var soon *TooSoonError
	if !errors.As(err, &soon) {
		t.Fatalf("second ping after 10 s = %v, want *TooSoonError", err)
	}
	if soon.RetryAfter != 20*time.Second {
		t.Errorf("RetryAfter = %s, want 20s", soon.RetryAfter)
	}
	if fakeOf(k).n() != 1 {
		t.Error("a rate-limited ping reached the provider")
	}
	clock.advance(20 * time.Second)
	if _, err := k.PingNow(ManualRequest{Key: sess1, MinGap: 30 * time.Second}); err != nil {
		t.Errorf("after the gap: %v", err)
	}
}

// A ping that read nothing paid for a write, so it avoided nothing: no rewrite credit.
func TestPingNowCreditsNothingWhenThePingMissed(t *testing.T) {
	k, sink, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) {
		return Usage{CacheWrite: 48576, Output: 1}, http.StatusOK, nil
	})
	res, err := k.PingNow(ManualRequest{Key: sess1, RewriteUSD: 0.18})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.CacheRead != 0 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if r := sink.all()[0]; r.RewriteUSD != 0 {
		t.Errorf("a ping that wrote credited a rewrite of %v", r.RewriteUSD)
	}
}

// The upstream failing is an error to the caller, and the audit row still records the attempt's status.
func TestPingNowReportsAnUpstreamTransportFailure(t *testing.T) {
	k, sink, _ := manualKeeper(t)
	fakeOf(k).setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, 0, errors.New("dial") })
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err == nil {
		t.Fatal("want an error")
	}
	if len(sink.all()) != 0 {
		t.Error("a ping that never got an answer wrote a row")
	}
}
