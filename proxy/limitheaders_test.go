package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// oauthToken is shaped like a Claude subscription token. Its tail must never reach the database.
const oauthToken = "sk-ant-oat01-NEVERSTORED-7f3a9c"

// limitsUpstream answers like Anthropic on a subscription: with the limit headers (one bad), unless
// noHeaders is set.
func limitsUpstream(t *testing.T, noHeaders *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

// limitsClient drives one proxy: send makes an Anthropic request with the given auth header,
// limits reads GET /api/me/limits.
type limitsClient struct {
	t   *testing.T
	srv *httptest.Server
}

func (c limitsClient) send(header, value string) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func (c limitsClient) limits() map[string]any {
	c.t.Helper()
	resp, err := c.srv.Client().Get(c.srv.URL + "/api/me/limits")
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET /api/me/limits = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

func newLimitsClient(t *testing.T, noHeaders *atomic.Bool) (limitsClient, *dash.Recorder, string) {
	t.Helper()
	up := limitsUpstream(t, noHeaders)
	t.Cleanup(up.Close)
	dir := t.TempDir()
	h, rec := dashHandler(t, up.URL, dash.Options{DBPath: filepath.Join(dir, "d.db")})
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return limitsClient{t: t, srv: srv}, rec, dir
}

// On a subscription (an OAuth bearer token) the proxy stores the limit headers and the auth kind on
// the row, forwards the answer unchanged, and GET /api/me/limits returns them. The token itself
// never reaches the database. A later answer without the headers leaves its row without them.
func TestLimitHeadersReachTheRowAndTheEndpoint(t *testing.T) {
	var noHeaders atomic.Bool
	c, rec, dir := newLimitsClient(t, &noHeaders)
	if got := c.limits(); len(got) != 0 {
		t.Fatalf("before any request: %v, want {}", got)
	}

	resp := c.send("Authorization", "Bearer "+oauthToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("anthropic-ratelimit-unified-7d-reset") != "garbage" {
		t.Errorf("the answer changed on the way: %d %v", resp.StatusCode, resp.Header)
	}
	waitForRows(t, rec, 1)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("rows: %v %v", page, err)
	}
	row := page.Requests[0]
	l := row.Limits
	if !row.AuthSubscription || l == nil || l.FiveHour.Utilization == nil || *l.FiveHour.Utilization != 0.54 ||
		l.FiveHour.ResetS == nil || *l.FiveHour.ResetS != 1794249600 || l.FiveHour.Status != "allowed" ||
		l.SevenDay.Utilization == nil || *l.SevenDay.Utilization != 0.08 || l.SevenDay.ResetS != nil {
		t.Fatalf("stored row: auth_subscription=%v limits=%+v", row.AuthSubscription, l)
	}
	got := c.limits()
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
	c.send("Authorization", "Bearer "+oauthToken)
	waitForRows(t, rec, 2)
	page, _ = rec.DB().Requests(dash.Filter{}, 0, 10)
	if page.Requests[0].Limits != nil || !page.Requests[0].AuthSubscription {
		t.Errorf("row without headers: %+v sub=%v", page.Requests[0].Limits, page.Requests[0].AuthSubscription)
	}
	if got := c.limits(); got["subscription"] != true || got["five_hour"] == nil {
		t.Errorf("after a row without headers: %v", got)
	}

	// The token never reaches the row: not in the row as read back, not anywhere in the files.
	b, _ := json.Marshal(page.Requests)
	if strings.Contains(string(b), "NEVERSTORED") {
		t.Errorf("the token reached the row: %s", b)
	}
	// The database and its write-ahead log, read while open: the rows are written by now.
	files, _ := filepath.Glob(filepath.Join(dir, "d.db*"))
	if len(files) == 0 {
		t.Fatal("no database files to check")
	}
	for _, f := range files {
		if raw, _ := os.ReadFile(f); bytes.Contains(raw, []byte("NEVERSTORED")) {
			t.Errorf("the token is in %s", f)
		}
	}
}

// An API key is not a subscription: {} even when the answer carries the limit headers.
func TestLimitsAreEmptyForAnAPIKey(t *testing.T) {
	var noHeaders atomic.Bool
	c, rec, _ := newLimitsClient(t, &noHeaders)
	c.send("x-api-key", "sk-ant-api03-somekey")
	waitForRows(t, rec, 1)
	page, _ := rec.DB().Requests(dash.Filter{}, 0, 10)
	if page.Requests[0].AuthSubscription || page.Requests[0].Limits == nil {
		t.Errorf("API-key row: sub=%v limits=%+v", page.Requests[0].AuthSubscription, page.Requests[0].Limits)
	}
	if got := c.limits(); len(got) != 0 {
		t.Errorf("API key: %v, want {}", got)
	}
	// A bearer token that is not a subscription token is not one either.
	c.send("Authorization", "Bearer sk-ant-api03-somekey")
	waitForRows(t, rec, 2)
	if got := c.limits(); len(got) != 0 {
		t.Errorf("bearer API key: %v, want {}", got)
	}
}

// An account that moves from a subscription to an API key stops showing the old share at once.
func TestLimitsStopAfterASwitchToAnAPIKey(t *testing.T) {
	var noHeaders atomic.Bool
	c, rec, _ := newLimitsClient(t, &noHeaders)
	c.send("Authorization", "Bearer "+oauthToken)
	waitForRows(t, rec, 1)
	if got := c.limits(); got["subscription"] != true {
		t.Fatalf("on the subscription: %v", got)
	}
	noHeaders.Store(true) // an API-key answer has no limit headers
	c.send("x-api-key", "sk-ant-api03-somekey")
	waitForRows(t, rec, 2)
	if got := c.limits(); len(got) != 0 {
		t.Errorf("after the switch: %v, want {}", got)
	}
}

func TestCallerIsSubscription(t *testing.T) {
	for _, tc := range []struct {
		header, value string
		want          bool
	}{
		{"Authorization", "Bearer " + oauthToken, true},
		{"authorization", "bearer " + oauthToken, true},
		{"Authorization", "Bearer sk-ant-api03-x", false},
		{"x-api-key", oauthToken, false},
		{"x-api-key", "sk-ant-api03-x", false},
		{"Authorization", "Bearer", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
		r.Header.Set(tc.header, tc.value)
		if got := callerIsSubscription(r); got != tc.want {
			t.Errorf("%s: %q = %v, want %v", tc.header, tc.value, got, tc.want)
		}
	}
}
