package proxy

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// PingLimits reads a ping report's response headers with the request path's parser (#995).
func TestPingLimitsUsesTheRequestParser(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	l := PingLimits(components.KeepAliveReport{Header: h})
	want := readLimitHeaders(h)
	if l == nil || *l.FiveHour.Utilization != *want.FiveHour.Utilization || l.SevenDay.Status != "rejected" {
		t.Fatalf("PingLimits = %+v, want %+v", l, want)
	}
	if l := PingLimits(components.KeepAliveReport{}); l != nil {
		t.Errorf("a report with no header = %+v, want nil", l)
	}
	bad := http.Header{}
	bad.Set("anthropic-ratelimit-unified-5h-status", "allowed; drop table")
	if l := PingLimits(components.KeepAliveReport{Header: bad}); l != nil {
		t.Errorf("a bad header = %+v, want nil", l)
	}
}
