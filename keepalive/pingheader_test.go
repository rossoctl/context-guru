package keepalive

import (
	"net/http"
	"testing"
	"time"
)

// limitHeader is a ping answer as a Claude subscription gets it: the anthropic-ratelimit-unified-*
// headers beside the usual ones.
func limitHeader() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1762790400")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	return h
}

// A scheduled ping hands its response headers to the host on the report, so the host can record
// the subscription limit headers on the ping's row (#995). The keeper itself reads nothing from them.
func TestScheduledPingReportCarriesTheResponseHeader(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	fakeOf(k).respHeader = limitHeader()
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("sweep fired %d pings, want 1", n)
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if got := reps[0].Header.Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.42" {
		t.Errorf("report header utilization = %q, want 0.42", got)
	}
}

// A ping refused for the rate limit (a 429) is still booked with its row, and its answer's headers
// go with it: the provider sends the limit headers on a refusal too, with the status "rejected".
func TestTransientPingReportCarriesTheResponseHeader(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	fd := fakeOf(k)
	fd.respHeader = limitHeader()
	fd.setResp(func(DispatchRequest) (Usage, int, error) { return Usage{}, http.StatusTooManyRequests, nil })
	k.Sweep(clock.advance(281 * time.Second))
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if got := reps[0].Header.Get("anthropic-ratelimit-unified-5h-status"); got != "allowed" {
		t.Errorf("report header status = %q, want allowed", got)
	}
}

// A manual ping (the Ping button) is a real request too, so its report carries the headers.
func TestManualPingReportCarriesTheResponseHeader(t *testing.T) {
	k, sink, _ := manualKeeper(t)
	fakeOf(k).respHeader = limitHeader()
	if _, err := k.PingNow(ManualRequest{Key: sess1}); err != nil {
		t.Fatalf("PingNow: %v", err)
	}
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if got := reps[0].Header.Get("anthropic-ratelimit-unified-5h-reset"); got != "1762790400" {
		t.Errorf("report header reset = %q, want 1762790400", got)
	}
}

// With no header on the answer the report has none: nothing is made up.
func TestPingReportWithoutResponseHeaderIsEmpty(t *testing.T) {
	k, sink, clock := manualKeeper(t)
	k.Sweep(clock.advance(281 * time.Second))
	reps := sink.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %d, want 1", len(reps))
	}
	if len(reps[0].Header) != 0 {
		t.Errorf("report header = %v, want none", reps[0].Header)
	}
}
