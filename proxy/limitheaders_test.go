package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

func TestReadLimitHeaders(t *testing.T) {
	// The values a live probe saw on 2026-11-09 (Claude Code 2.1.319).
	good := http.Header{}
	good.Set("anthropic-ratelimit-unified-5h-utilization", "0.54")
	good.Set("anthropic-ratelimit-unified-5h-reset", "1794249600")
	good.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	good.Set("anthropic-ratelimit-unified-7d-utilization", "0.08")
	good.Set("anthropic-ratelimit-unified-7d-reset", "1794348000")
	good.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	l := readLimitHeaders(good)
	if l == nil || l.FiveHour.Utilization == nil || *l.FiveHour.Utilization != 0.54 ||
		l.FiveHour.ResetS == nil || *l.FiveHour.ResetS != 1794249600 || l.FiveHour.Status != "allowed" ||
		l.SevenDay.Utilization == nil || *l.SevenDay.Utilization != 0.08 ||
		l.SevenDay.ResetS == nil || *l.SevenDay.ResetS != 1794348000 || l.SevenDay.Status != "allowed" {
		t.Fatalf("good headers read as %+v", l)
	}

	// No headers at all: an API key, Bedrock, Vertex.
	if l := readLimitHeaders(http.Header{"Content-Type": {"application/json"}}); l != nil {
		t.Errorf("no headers read as %+v, want nil", l)
	}
	if l := readLimitHeaders(nil); l != nil {
		t.Errorf("nil header read as %+v", l)
	}

	// Bad values are left out one by one; a good one beside them is kept.
	bad := http.Header{}
	bad.Set("anthropic-ratelimit-unified-5h-utilization", "lots")
	bad.Set("anthropic-ratelimit-unified-5h-reset", "-5")
	bad.Set("anthropic-ratelimit-unified-5h-status", "allowed; drop table")
	bad.Set("anthropic-ratelimit-unified-7d-utilization", "NaN")
	bad.Set("anthropic-ratelimit-unified-7d-reset", "1794348000.5")
	bad.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	l = readLimitHeaders(bad)
	if l == nil || !l.FiveHour.Empty() || l.SevenDay.Utilization != nil || l.SevenDay.ResetS != nil ||
		l.SevenDay.Status != "rejected" {
		t.Errorf("bad headers read as %+v, want only the 7d status", l)
	}
	for _, v := range []string{"-0.1", "Inf", "1e9", ""} {
		h := http.Header{}
		h.Set("anthropic-ratelimit-unified-5h-utilization", v)
		if l := readLimitHeaders(h); l != nil {
			t.Errorf("utilization %q read as %+v, want nil", v, l)
		}
	}
	if !limitStatusOK("allowed_warning") || limitStatusOK(strings.Repeat("a", 33)) {
		t.Error("status check")
	}

	// Mixed case in the names (as the wire may carry them) and in the status word.
	mixed := http.Header{}
	mixed["ANTHROPIC-RateLimit-Unified-5H-Utilization"] = []string{" 0.97 "}
	mixed.Set("Anthropic-Ratelimit-Unified-5h-Status", "Allowed_Warning")
	l = readLimitHeaders(canonical(mixed))
	if l == nil || l.FiveHour.Utilization == nil || *l.FiveHour.Utilization != 0.97 ||
		l.FiveHour.Status != "allowed_warning" || !l.SevenDay.Empty() {
		t.Errorf("mixed-case headers read as %+v", l)
	}
}

// canonical rebuilds h the way net/http does for headers that arrive on the wire.
func canonical(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

// The proxy stores the limit headers on the request's row, forwards the answer unchanged, and
// GET /api/me/limits returns them. An answer without the headers leaves the row without them.
func TestLimitHeadersReachTheRowAndTheEndpoint(t *testing.T) {
	var noHeaders atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if !noHeaders.Load() {
			// Lower-case names written as-is, the way an HTTP/2 upstream sends them.
			w.Header()["anthropic-ratelimit-unified-5h-utilization"] = []string{"0.54"}
			w.Header()["anthropic-ratelimit-unified-5h-reset"] = []string{"1794249600"}
			w.Header()["anthropic-ratelimit-unified-5h-status"] = []string{"allowed"}
			w.Header()["anthropic-ratelimit-unified-7d-utilization"] = []string{"0.08"}
			w.Header()["anthropic-ratelimit-unified-7d-reset"] = []string{"garbage"}
		}
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		  "usage":{"input_tokens":12,"output_tokens":34,
		           "cache_read_input_tokens":9000,"cache_creation_input_tokens":1500}}`)
	}))
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	limits := func() map[string]any {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + "/api/me/limits")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/me/limits = %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := limits(); len(got) != 0 {
		t.Fatalf("before any request: %v, want {}", got)
	}

	send := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
			strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	resp := send()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("anthropic-ratelimit-unified-7d-reset") != "garbage" {
		t.Errorf("the answer changed on the way: %d %v", resp.StatusCode, resp.Header)
	}
	waitForRows(t, rec, 1)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("rows: %v %v", page, err)
	}
	l := page.Requests[0].Limits
	if l == nil || l.FiveHour.Utilization == nil || *l.FiveHour.Utilization != 0.54 ||
		l.FiveHour.ResetS == nil || *l.FiveHour.ResetS != 1794249600 || l.FiveHour.Status != "allowed" ||
		l.SevenDay.Utilization == nil || *l.SevenDay.Utilization != 0.08 || l.SevenDay.ResetS != nil {
		t.Fatalf("stored limits = %+v", l)
	}
	got := limits()
	five, _ := got["five_hour"].(map[string]any)
	seven, _ := got["seven_day"].(map[string]any)
	if got["subscription"] != true || got["at_ms"] == nil || five["utilization"] != 0.54 ||
		five["reset_s"] != float64(1794249600) || five["status"] != "allowed" ||
		seven["utilization"] != 0.08 || seven["reset_s"] != nil {
		t.Errorf("GET /api/me/limits = %v", got)
	}

	// A later answer without the headers: its row has none, and the endpoint still shows the
	// latest values the proxy saw.
	noHeaders.Store(true)
	send()
	waitForRows(t, rec, 2)
	page, _ = rec.DB().Requests(dash.Filter{}, 0, 10)
	if page.Requests[0].Limits != nil {
		t.Errorf("row without headers stored %+v", page.Requests[0].Limits)
	}
	if got := limits(); got["subscription"] != true {
		t.Errorf("after a row without headers: %v", got)
	}
}
