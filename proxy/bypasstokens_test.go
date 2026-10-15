package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// The agent's own compaction request (see agentcompaction.go): bypassed, and allowed past the
// 32 MiB ceiling, which the x-context-guru-bypass header is not.
const compactPromptForBypassTest = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n" +
	"- Tool calls will be REJECTED and will waste your only turn — you will fail the task.\n\n" +
	"Your task is to create a detailed summary of the conversation so far, paying close " +
	"attention to the user's explicit requests and your previous actions.\n" +
	"3. Files and Code Sections: ... include full code snippets where applicable"

func bypassBody(t *testing.T, bytesWanted int) []byte {
	t.Helper()
	line := "Authorized uses only. All activity may be monitored and reported.\n"
	body, err := json.Marshal(map[string]any{
		"model": "gpt-x",
		"messages": []map[string]any{
			{"role": "user", "content": strings.Repeat(line, bytesWanted/len(line)+1)},
			{"role": "user", "content": compactPromptForBypassTest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A bypassed request is forwarded untouched, so the proxy must not tokenize it on the request
// path: counting a 33 MiB transcript took ~12 s before the request reached the upstream.
func TestBypassedRequestIsNotTokenizedOnTheRequestPath(t *testing.T) {
	arrived := make(chan time.Time, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- time.Now()
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg,
		Options{OpenAIUpstream: up.URL, AnthropicUpstream: up.URL, Prices: fixedPricer{}})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := bypassBody(t, 33<<20)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
	encodedBefore := tokens.Stats().EncodedBytes
	sent := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The 33 MiB loopback transfer itself is part of this figure. Before the change it was ~12 s.
	var at time.Time
	select {
	case at = <-arrived:
	default:
		t.Fatalf("the request never reached the upstream (status %d)", resp.StatusCode)
	}
	// Not a stopwatch (it would flake under -race): the tokenizer must not have run over the body.
	// Before the change it encoded all 33 MiB here, ~12 s on the request path.
	if enc := tokens.Stats().EncodedBytes - encodedBefore; enc > 1<<20 {
		t.Fatalf("a bypassed 33 MiB request encoded %d bytes of text on the request path, want ~0", enc)
	}
	t.Logf("a bypassed 33 MiB request reached the upstream after %v", at.Sub(sent))
}

// The count moves off the request path, it does not disappear: the dashboard row of a bypassed
// request ends up with the figures main stored for the same body, and still says why it was not
// compacted. The constants are what origin/main wrote for bypassBody(64 KiB) through this route.
func TestBypassedRowGetsItsTokenCountFromTheWorker(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{CaptureContent: true, ContentCap: 1 << 16,
		ContentMaxPerRequest: 10})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := bypassBody(t, 64<<10)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-context-guru-bypass", "true")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForRows(t, rec, 1)

	var e *dash.Event
	for i := 0; i < 400; i++ { // the worker fills the counts after the insert
		page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
		if err != nil || len(page.Requests) != 1 {
			t.Fatalf("rows = %v, err %v", page, err)
		}
		if e = page.Requests[0]; e.TokensBefore > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e.Mode != dash.ModeBypass || e.UncompressedReason != dash.ReasonBypassed {
		t.Errorf("mode %q reason %q, want bypass / bypassed", e.Mode, e.UncompressedReason)
	}
	const want = 11992 // origin/main stored this for the same body
	if e.TokensBefore != want || e.TokensAfter != want || e.AttemptedTokens != want || e.FrozenTokens != 0 {
		t.Errorf("before/after/attempted/frozen = %d/%d/%d/%d, want %d/%d/%d/0",
			e.TokensBefore, e.TokensAfter, e.AttemptedTokens, e.FrozenTokens, want, want, want)
	}
	if e.TokenAccounting != dash.AccountingComplete {
		t.Errorf("accounting = %q, want complete", e.TokenAccounting)
	}

	// Totals are what origin/main reported for the same request. The process-wide counters
	// (/stats, cg_tokens_*_total) never saw a bypassed request: Pipeline.Run returns before it
	// emits its report. The per-tenant series are summed from the rows, so they carry the count.
	if s := h.agg.Snapshot(); s.Requests != 0 || s.TokensBefore != 0 || s.TokensAfter != 0 {
		t.Errorf("process-wide counters = %d requests / %d / %d tokens, want 0 as on main",
			s.Requests, s.TokensBefore, s.TokensAfter)
	}
	rows, err := rec.DB().TenantMetrics(0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("tenant metrics = %v, err %v", rows, err)
	}
	if rows[0].TokensBefore != want || rows[0].TokensAfter != want {
		t.Errorf("per-tenant token totals = %d / %d, want %d (what main stored)", rows[0].TokensBefore, rows[0].TokensAfter, want)
	}
}

// cg_ms on the request line is the whole pre-forward stage, not the sum of the components.
func TestLifecycleLoggerCGMsIsThePreForwardTime(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	lifecycleLogger(lg, apply.Trace{Run: &components.RunReport{DurationMs: 0.4}}, false, 612*time.Millisecond).Info("x")
	out := buf.String()
	if !strings.Contains(out, "cg_ms=612") || !strings.Contains(out, "components_ms=0.4") {
		t.Fatalf("log line = %q, want cg_ms=612 and components_ms=0.4", out)
	}
	if strings.Contains(out, "tokens_pending") {
		t.Fatalf("a normal request is labelled tokens_pending: %q", out)
	}
	buf.Reset()
	lifecycleLogger(lg, apply.Trace{Run: &components.RunReport{}}, true, time.Millisecond).Info("x")
	if !strings.Contains(buf.String(), "tokens_pending=true") {
		t.Fatalf("a bypassed request's log line does not say its tokens are pending: %q", buf.String())
	}
}
