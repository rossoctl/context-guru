package plugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cgStub answers the three reads the redesigned status line makes. cache is the /api/cachestate
// body; everything is fixed, so the rendered line is byte-for-byte reproducible.
func cgStub(t *testing.T, cache string, extra ...func(*http.ServeMux)) (port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "since=0") { // lifetime, tenant-wide
			w.Write([]byte(`{"total_saved_usd": 212.4}`))
			return
		}
		if strings.Contains(r.URL.RawQuery, "since=") { // today, tenant-wide
			w.Write([]byte(`{"total_saved_usd": 4.2}`))
			return
		}
		w.Write([]byte(`{"total_saved_usd": 0.31, "cost_usd": 0.41, "cg_latency_ms_avg": 3, "upstream_ms_avg": 340}`))
	})
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(cache)) })
	for _, f := range extra {
		f(mux)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// TestStatuslineGoldenOutput pins the rendered line for each cache state, layout and width. The
// server-side cache state is fixed relative to its own now_ms, so no wall clock enters the result.
func TestStatuslineGoldenOutput(t *testing.T) {
	const now = 1_800_000_000_000
	cs := func(agoSec, ttl, prefix int, usd string) string {
		return fmt.Sprintf(`{"session":"s","model":"m","money":{"main":{"saved_usd":0.31,"baseline_usd":2.10,"threads":1},"others":{"saved_usd":0,"baseline_usd":0,"threads":0}},"last_ts":%d,"now_ms":%d,"ttl_s":%d,`+
			`"prefix_tokens":%d,"priced":true,"rewrite_usd":%s,"window":1000000,"window_exact":true}`,
			now-agoSec*1000, now, ttl, prefix, usd)
	}
	ping := func(body string) string { return strings.Replace(body, `"priced"`, `"keepalive":true,"priced"`, 1) }
	// 412k tokens in a 1M window; Claude Code itself believes the window is 200k, the proxy knows better.
	payload := `{"session_id":"sess-gold","cost":{"total_cost_usd":0.41},"context_window":` +
		`{"total_input_tokens":412000,"total_output_tokens":900,"context_window_size":200000}}`
	small := strings.Replace(payload, "412000", "52000", 1)
	full := strings.Replace(payload, "412000", "900000", 1)
	sess := "saved $0.31 of $2.10 session"
	for _, c := range []struct {
		name, cache, stdin, layout string
		width                      int
		want                       string
	}{
		{"warm/minimal", cs(60, 300, 52000, "0.30"), small, "minimal", 200, "saved $0.31 of $2.10 │ cache ● █████░ 4:00"},
		{"warm/balanced", cs(60, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"warm/ping", ping(cs(60, 300, 52000, "0.30")), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 ↻ │ ctx ░░░░░░░░ 5%"},
		{"warm/detailed", cs(60, 3600, 52000, "0.30"), small, "detailed", 200,
			sess + " │ spent $0.41\ncache ● ██████ 59:00 │ ctx ░░░░░░░░ 5%"},
		{"expiring/cheap", cs(258, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ░░░░░░░░ 5%"},
		{"expiring/costly", cs(258, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ███░░░░░ 41%"},
		{"expiring/ping-covers-it", ping(cs(258, 300, 412000, "1.20")), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 ↻ │ ctx ███░░░░░ 41%"},
		{"warm/rewrite-near", cs(60, 300, 900000, "2.70"), full, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███████░ 90% ⚠ rewrite near"},
		{"cold/balanced", cs(400, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1:40 │ ctx ███░░░░░ 41%"},
		{"cold/minimal", cs(400, 300, 412000, "1.20"), payload, "minimal", 200,
			"saved $0.31 of $2.10 │ cache ○ ░░░░░░ cold 1:40"},
		{"cold/narrow", cs(400, 300, 412000, "1.20"), payload, "balanced", 60,
			sess + "\ncache ○ ░░░░░░ cold 1:40 │ ctx ███░░░░░ 41%"},
		{"warm/narrow", cs(60, 300, 52000, "0.30"), small, "balanced", 40,
			"saved $0.31 of $2.10 session\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"first-run", `{"session":"s","none":true,"now_ms":1}`, `{"session_id":"sess-gold"}`, "balanced", 200,
			"◌ no cache yet"}, // a brand-new session claims no saving yet
	} {
		t.Run(c.name, func(t *testing.T) {
			env := routedEnv(cgStub(t, c.cache))
			env["CG_STATUSLINE_COLUMNS"] = fmt.Sprint(c.width)
			out, code, elapsed := runStatusline(t, env, c.stdin, "--layout="+c.layout)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, c.want)
			}
			if elapsed > time.Second {
				t.Errorf("render took %v; the status line must stay cheap", elapsed)
			}
		})
	}
}

// Proxy down is `cg ✕ proxy offline` in every layout; NO_COLOR=unset leaves ANSI on and the line still fits the width.
func TestStatuslineGoldenProxyDownAndColour(t *testing.T) {
	port := cgStub(t, `{"none":true}`)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	if out, _, _ := runStatusline(t, routedEnv(dead), `{}`, "--layout=detailed"); strings.TrimSpace(out) != "cg ✕ proxy offline" {
		t.Errorf("proxy down: got %q, want the offline notice", out)
	}
	env := routedEnv(port)
	env["NO_COLOR"] = ""
	out, _, _ := runStatusline(t, env, `{"session_id":"sess-gold"}`)
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("colour expected when NO_COLOR is empty, got %q", out)
	}
}

// TestStatuslineGoldenLive pins the live-countdown states: critical and cold clocks, per-thread
// (sub-agent) caches, and the warnings that need no extra request. Times are relative to the
// proxy's own now_ms, so no wall clock enters the result.
func TestStatuslineGoldenLive(t *testing.T) {
	const now = 1_800_000_000_000
	th := func(id string, agoSec, ttl, prefix int, usd string) string {
		return fmt.Sprintf(`{"id":%q,"main":%t,"model":"claude-sonnet-5-5","last_ts":%d,"end_ts":%d,"ttl_s":%d,`+
			`"prefix_tokens":%d,"priced":true,"rewrite_usd":%s}`, id, id == "", now-agoSec*1000, now-agoSec*1000, ttl, prefix, usd)
	}
	ctxPay := func(model string) string {
		return `{"session_id":"sess-gold","model":{"id":"` + model + `"},"cost":{"total_cost_usd":4.1,"total_duration_ms":3600000},` +
			`"context_window":{"total_input_tokens":412000,"total_output_tokens":900,"context_window_size":200000}}`
	}
	body := func(model string, lastAgo int, threads ...string) string {
		return fmt.Sprintf(`{"session":"s","model":%q,"money":{"main":{"saved_usd":0.31,"baseline_usd":2.10,"threads":1},"others":{"saved_usd":0,"baseline_usd":0,"threads":0}},"last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":412000,`+
			`"priced":true,"rewrite_usd":1.20,"window":1000000,"window_exact":true,"threads":[%s]}`,
			model, now-lastAgo*1000, now, strings.Join(threads, ","))
	}
	sess := "saved $0.31 of $2.10 session"
	m := "claude-sonnet-5-5"
	for _, c := range []struct {
		name, cache, stdin, layout string
		width                      int
		want                       string
	}{
		{"critical", body(m, 290, th("", 290, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ◐ ░░░░░░ 0:10! │ ctx ███░░░░░ 41%"},
		{"cold-counts-up", body(m, 375, th("", 375, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1:15 │ ctx ███░░░░░ 41%"},
		{"cold-long", body(m, 3900, th("", 3900, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1h00m │ ctx ███░░░░░ 41%"},
		// The latest request of the session was a sub-agent's; the main cache must still count down on its own.
		{"main-not-reset-by-subagent", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("", 200, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ██░░░░ 1:40 │ agent ● 4:55 │ ctx ███░░░░░ 41%"},
		{"two-agents", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ agents 2 · soonest ◐ 0:50 │ ctx ███░░░░░ 41%"},
		{"many-agents", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"),
			th("a:3", 100, 300, 9000, "0.03"), th("a:4", 7, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ agents 4 · soonest ◐ 0:50 │ ctx ███░░░░░ 41%"},
		{"cold-agents-are-counted", body(m, 100, th("a:1", 900, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ agents 1 cold │ ctx ███░░░░░ 41%"},
		{"agents-narrow", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"),
			th("a:3", 100, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")), ctxPay(m), "balanced", 36,
			"saved $0.31 of $2.10 session\ncache ● 3:20 │ agents 3 │ ctx 41%"},
		{"model-switch", body(m, 60, th("", 60, 300, 412000, "1.20")), ctxPay("claude-opus-5-5"), "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███░░░░░ 41%"},
		{"same-model-other-spelling", body("aws/claude-sonnet-5-5", 60, th("", 60, 300, 412000, "1.20")), ctxPay("claude-sonnet-5-5[1m]"), "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███░░░░░ 41%"},
		{"spend-detailed", body(m, 60, th("", 60, 300, 412000, "1.20")), ctxPay(m), "detailed", 200,
			sess + " │ spent $4.10 · $4.10/h · up 1h00m\ncache ● █████░ 4:00 │ ctx ███░░░░░ 41%"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := routedEnv(cgStub(t, c.cache))
			env["CG_STATUSLINE_COLUMNS"] = fmt.Sprint(c.width)
			out, code, _ := runStatusline(t, env, c.stdin, "--layout="+c.layout)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, c.want)
			}
		})
	}
}

// TestStatuslineGoldenAgentsAndKeepAlive pins the per-agent and keep-alive elements: each
// sub-agent's cache under the name Claude Code's agents list shows (read from the session's own
// subagents/ files, for both id forms Claude Code sends; a prefix-grouped `p:` thread is a side
// request and not listed), the main cache held warm by planned pings
// (clock = time until cold, `↻`), this session's pings, and that no lifetime total is ever read.
func TestStatuslineGoldenAgentsAndKeepAlive(t *testing.T) {
	const now = 1_800_000_000_000
	dir := t.TempDir()
	sub := filepath.Join(dir, "sess-gold", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		// a plain sub-agent: its header is the file id
		"agent-ad9ba182bf3e760e6.meta.json": `{"agentType":"Explore","description":"find the parser"}`,
		// an agent-team teammate: its header is <name>@<team>, not the file id
		"agent-areviewer-8808ee2f0c216961.meta.json": `{"name":"reviewer","teamName":"session-ab47f1d6","agentType":"reviewer"}`,
	} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 7; i++ {
		meta := fmt.Sprintf(`{"name":"w%d","teamName":"t"}`, i)
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("agent-aw%d-%016x.meta.json", i, i)), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	th := func(id string, agoSec int, held string) string {
		return fmt.Sprintf(`{"id":%q,"main":%t,"model":"m","last_ts":%d,"end_ts":%d,"ttl_s":300,`+
			`"prefix_tokens":%d,"priced":true,"rewrite_usd":0.03%s}`, id, id == "", now-agoSec*1000, now-agoSec*1000,
			map[bool]int{true: 52000, false: 9000}[id == ""], held)
	}
	held := func(sec int) string { return fmt.Sprintf(`,"pings_left":2,"held_until_ms":%d`, now+sec*1000) }
	cache := func(threads ...string) string {
		return fmt.Sprintf(`{"session":"s","model":"m","money":{"main":{"saved_usd":0.31,"baseline_usd":2.64,"threads":1},`+
			`"others":{"saved_usd":0.12,"baseline_usd":0.90,"threads":4}},"last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,`+
			`"priced":true,"rewrite_usd":0.30,"window":1000000,"window_exact":true,"threads":[%s]}`,
			now-60_000, now, strings.Join(threads, ","))
	}
	stdin := `{"session_id":"sess-gold","transcript_path":"` + filepath.Join(dir, "sess-gold.jsonl") + `",` +
		`"cost":{"total_cost_usd":0.41},"context_window":{"total_input_tokens":52000,"total_output_tokens":900,"context_window_size":200000}}`
	var queries []string
	stub := func(cache string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			w.Write([]byte(`{"total_saved_usd": 0.31, "cost_usd": 2.50, "cg_llm_cost_usd": 0.10, ` +
				`"keepalive_pings": 3, "keepalive_ping_usd": 0.04}`))
		})
		mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(cache)) })
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln) //nolint:errcheck
		t.Cleanup(func() { srv.Close() })
		return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	}
	for _, c := range []struct {
		name, cache string
		width       int
		want        string
	}{
		{"named-agents", cache(th("", 60, ""), th("a:ad9ba182bf3e760e6", 10, ""), th("a:reviewer@session-ab47f1d6", 250, held(700)),
			th("a:finished", 900, ""), th("p:0123456789ab", 10, "")), 200, // p: is a side request, not an agent
			"saved $0.31 of $2.64 session · saved $0.12 of $0.90 subagents\ncache ● █████░ 4:00 │ agents Explore active ● 4:50 · reviewer ● 11:40 ↻ · 1 cold │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		// Most recently used first, at most five, then `+N more`; `active` within the last 10 s.
		{"many-agents-capped", cache(th("", 60, ""), th("a:w1@t", 200, ""), th("a:w2@t", 5, ""), th("a:w3@t", 100, ""),
			th("a:w4@t", 30, ""), th("a:w5@t", 250, ""), th("a:w6@t", 60, ""), th("a:w7@t", 150, "")), 300,
			"saved $0.31 of $2.64 session · saved $0.12 of $0.90 subagents\ncache ● █████░ 4:00 │ agents w2 active ● 4:55 · w4 ● 4:30 · w6 ● 4:00 · w3 ● 3:20 · w7 ● 2:30 · +2 more │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		{"main-held-warm", cache(th("", 60, held(820))), 200,
			"saved $0.31 of $2.64 session · saved $0.12 of $0.90 subagents\ncache ● █████░ 13:40 ↻ │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		// A plan cannot revive an expired entry (the ping failed or never came): cold is cold.
		{"plan-cannot-revive-a-cold-cache", cache(th("", 320, held(500))), 200,
			"saved $0.31 of $2.64 session · saved $0.12 of $0.90 subagents\ncache ○ ░░░░░░ cold 0:20 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		{"narrow-drops-ka-first", cache(th("", 60, held(820)), th("a:ad9ba182bf3e760e6", 10, "")), 60,
			"saved $0.31 of $2.64\ncache ● █████░ 13:40 ↻ │ agents 1 │ ctx ░░░░░░░░ 5%"}, // narrow: the main thread's figure only,
	} {
		t.Run(c.name, func(t *testing.T) {
			env := routedEnv(stub(c.cache))
			env["CG_STATUSLINE_COLUMNS"] = fmt.Sprint(c.width)
			out, code, _ := runStatusline(t, env, stdin, "--layout=balanced")
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, c.want)
			}
		})
	}
	for _, q := range queries {
		if strings.Contains(q, "since=") || !strings.Contains(q, "session=sess-gold") {
			t.Errorf("/api/stats?%s: the status line reads this session's totals only (#510)", q)
		}
	}
}

// TestStatuslineGoldenMoney pins the money elements: the session (main thread) and subagents (every
// other thread, side requests included) figures, each listed agent's own `saved $z of $q` next to its
// clock, a side (p:) thread counted in the total but never listed, a main-only session, the
// narrow-width degrade, and a missing money field rendering nothing rather than $0.00.
func TestStatuslineGoldenMoney(t *testing.T) {
	const now = 1_800_000_000_000
	dir := t.TempDir()
	sub := filepath.Join(dir, "sess-m", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"agent-areviewer-8808ee2f0c216961.meta.json": `{"name":"reviewer","teamName":"t"}`,
		"agent-ad9ba182bf3e760e6.meta.json":          `{"agentType":"Explore"}`,
	} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	th := func(id string, ago int, money string) string {
		return fmt.Sprintf(`{"id":%q,"main":%t,"model":"m","last_ts":%d,"end_ts":%d,"ttl_s":300,`+
			`"prefix_tokens":52000,"priced":true,"rewrite_usd":0.03%s}`, id, id == "", now-ago*1000, now-ago*1000, money)
	}
	pair := func(x, y string) string { return `,"saved_usd":` + x + `,"baseline_usd":` + y }
	cache := func(money string, threads ...string) string {
		return fmt.Sprintf(`{"session":"s","model":"m",%s"last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,`+
			`"priced":true,"rewrite_usd":0.30,"window":1000000,"window_exact":true,"threads":[%s]}`,
			money, now-60_000, now, strings.Join(threads, ","))
	}
	both := `"money":{"main":{"saved_usd":50.32,"baseline_usd":250,"threads":1},"others":{"saved_usd":60.03,"baseline_usd":300,"threads":3}},`
	stdin := `{"session_id":"sess-m","transcript_path":"` + filepath.Join(dir, "sess-m.jsonl") + `",` +
		`"cost":{"total_cost_usd":4.1},"context_window":{"total_input_tokens":52000,"total_output_tokens":900,"context_window_size":200000}}`
	agents := []string{th("", 60, pair("50.32", "250")), th("a:reviewer@t", 5, pair("1.20", "6.00")),
		th("a:ad9ba182bf3e760e6", 250, pair("0.30", "2.10")), th("p:0123456789ab", 20, pair("58.53", "291.90"))}
	for _, c := range []struct {
		name, cache string
		width       int
		want        string
	}{
		{"main-only", cache(`"money":{"main":{"saved_usd":0.31,"baseline_usd":2.64,"threads":1},"others":{"saved_usd":0,"baseline_usd":0,"threads":0}},`,
			th("", 60, pair("0.31", "2.64"))), 200,
			"saved $0.31 of $2.64 session\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		// Two named agents with their own figures; the p: side thread is in the total only.
		{"agents-and-a-side-thread", cache(both, agents...), 220,
			"saved $50.32 of $250.00 session · saved $60.03 of $300.00 subagents\n" +
				"cache ● █████░ 4:00 │ agents reviewer active ● 4:55 saved $1.20 of $6.00 · Explore ◐ 0:50 saved $0.30 of $2.10 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		// Narrow: the per-agent money goes first, then the subagents total, before the names.
		{"narrow", cache(both, agents...), 90,
			"saved $50.32 of $250.00 session · saved $60.03 of $300.00 subagents\n" +
				"cache ● █████░ 4:00 │ agents reviewer active ● 4:55 · Explore ◐ 0:50 │ ctx ░░░░░░░░ 5%"},
		// An agent with no money in its row shows no figure; the other keeps its own.
		{"agent-without-money", cache(both, th("", 60, pair("50.32", "250")), th("a:reviewer@t", 5, pair("1.20", "6.00")),
			th("a:ad9ba182bf3e760e6", 250, "")), 220,
			"saved $50.32 of $250.00 session · saved $60.03 of $300.00 subagents\n" +
				"cache ● █████░ 4:00 │ agents reviewer active ● 4:55 saved $1.20 of $6.00 · Explore ◐ 0:50 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		// A missing field is unknown, not $0.00: that figure is left out.
		{"missing-fields", cache(`"money":{"main":{"saved_usd":0.31,"threads":1},"others":{"baseline_usd":0.9,"threads":2}},`,
			th("", 60, ""), th("a:reviewer@t", 5, `,"saved_usd":1.2`)), 200,
			"cache ● █████░ 4:00 │ agents reviewer active ● 4:55 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
		{"missing-subagents-figure", cache(`"money":{"main":{"saved_usd":0.31,"baseline_usd":2.64,"threads":1},"others":{"baseline_usd":0.9,"threads":2}},`,
			th("", 60, "")), 200,
			"saved $0.31 of $2.64 session\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5% │ ka 3 pings $0.04"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(`{"total_saved_usd": 9, "keepalive_pings": 3, "keepalive_ping_usd": 0.04}`))
			})
			mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(c.cache)) })
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go srv.Serve(ln) //nolint:errcheck
			t.Cleanup(func() { srv.Close() })
			env := routedEnv(fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
			env["CG_STATUSLINE_COLUMNS"] = fmt.Sprint(c.width)
			out, code, elapsed := runStatusline(t, env, stdin, "--layout=balanced")
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := strings.TrimRight(out, "\n"); got != c.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, c.want)
			}
			if elapsed > 500*time.Millisecond {
				t.Errorf("render took %v; the budget is 0.5 s", elapsed)
			}
		})
	}
}

// TestStatuslineAgentNamesAreBounded: the names come from the session's subagents/ files, and a
// long session can hold thousands. A render opens only the files of the agents the proxy lists
// (directly by id for a sub-agent, by a name-prefix match in a capped directory listing for a
// teammate) and reads at most 4 KB of each, so 5,000 files, a 2 MB file and 16 listed agents
// still render inside the budget. Two agents of one type read `Explore` and `Explore 2`.
func TestStatuslineAgentNamesAreBounded(t *testing.T) {
	const now = 1_800_000_000_000
	dir := t.TempDir()
	sub := filepath.Join(dir, "sess-big", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5000; i++ {
		write(fmt.Sprintf("agent-a%016x.meta.json", i), `{"agentType":"Explore"}`)
	}
	write("agent-areviewer-8808ee2f0c216961.meta.json", `{"name":"reviewer","teamName":"t","agentType":"reviewer"}`)
	write("agent-ahuge.meta.json", `{"agentType":"`+strings.Repeat("x", 2<<20)+`"}`)
	var threads []string
	th := func(id string, ago int) {
		threads = append(threads, fmt.Sprintf(`{"id":%q,"main":%t,"model":"m","last_ts":%d,"end_ts":%d,"ttl_s":300,`+
			`"prefix_tokens":9000,"priced":true,"rewrite_usd":0.03}`, id, id == "", now-ago*1000, now-ago*1000))
	}
	th("", 60)
	th("a:a0000000000000001", 20)
	th("a:a0000000000000002", 21)
	th("a:huge", 22)
	th("a:reviewer@t", 23)
	for i := 0; i < 12; i++ { // more listed teammates whose files do not exist
		th(fmt.Sprintf("a:ghost%d@t", i), 200+i)
	}
	cache := fmt.Sprintf(`{"session":"s","model":"m","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,`+
		`"priced":true,"rewrite_usd":0.30,"window":1000000,"window_exact":true,"threads":[%s]}`, now-60_000, now, strings.Join(threads, ","))
	env := routedEnv(cgStub(t, cache))
	env["CG_STATUSLINE_COLUMNS"] = "300"
	stdin := `{"session_id":"sess-big","transcript_path":"` + filepath.Join(dir, "sess-big.jsonl") + `"}`
	out, code, elapsed := runStatusline(t, env, stdin, "--layout=balanced")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "agents Explore ● 4:40 · Explore 2 ● 4:39 · reviewer ● 4:37 · +13 more"
	if !strings.Contains(out, want) {
		t.Errorf("got %q, want it to contain %q (the 2 MB file's agent unlabelled, counted in +N more)", out, want)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("render took %v with 5,000 agent files; the budget is 0.5 s", elapsed)
	}
}

// TestStatuslineTimerMath pins the countdown arithmetic: the expiry cliff (the last ten seconds of
// the five-minute entry, then cold at 300 s from the last response's end), the colour bands, the
// response-end anchor, and that the result depends on the server's clock alone, never the local one.
func TestStatuslineTimerMath(t *testing.T) {
	py := requireTool(t, "python3")
	script := `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("statusline", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
NOW = 1_800_000_000_000
def rem(ago, ttl=300, end_after=None, age=0.0):
    e = {"last_ts": NOW - ago * 1000, "ttl_s": ttl, "now_ms": NOW}
    if end_after is not None:
        e["end_ts"] = NOW - ago * 1000 + end_after * 1000
    return m.remaining_seconds(e, age)
for ago in (0, 239, 240, 285, 289, 290, 299, 300, 301, 3900):
    r = rem(ago)
    print("ago", ago, r, m.entry_state(r, 300))
print("end", rem(300, end_after=40))           # a 40 s answer: warm for 40 s more
print("age", rem(100, age=30.0))               # sample held 30 s: 30 s less
print("1h", m.entry_state(720, 3600), m.entry_state(721, 3600))
print("bad", m.remaining_seconds({"last_ts": 1, "ttl_s": 0, "now_ms": 2}, 0), m.remaining_seconds({}, 0))
a = rem(250)
m.time.time = lambda: 1.0                       # a client clock decades off changes nothing
print("skew", a == rem(250))
`
	cmd := exec.Command(py, "-c", script, filepath.Join(scriptsDir(t), "statusline.py"))
	cmd.Env = append(sandboxEnv(t), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := strings.Join([]string{
		"ago 0 300.0 warm", "ago 239 61.0 warm", "ago 240 60.0 expiring", "ago 285 15.0 critical",
		"ago 289 11.0 critical", "ago 290 10.0 critical", "ago 299 1.0 critical", "ago 300 0.0 cold",
		"ago 301 -1.0 cold", "ago 3900 -3600.0 cold",
		"end 40.0", "age 170.0", "1h expiring warm", "bad None None", "skew True",
	}, "\n")
	if got := strings.TrimRight(string(out), "\n"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// --set-layout remembers a layout across renders (the toggle), and `auto` forgets it.
func TestStatuslineSetLayout(t *testing.T) {
	port := cgStub(t, `{"none":true}`)
	cfg := t.TempDir()
	env := routedEnv(port)
	env["XDG_CONFIG_HOME"] = cfg
	env["CG_STATUSLINE_COLUMNS"] = "200"
	run := func(args ...string) string {
		cmd := exec.Command(requireTool(t, "python3"), append([]string{filepath.Join(scriptsDir(t), "statusline.py")}, args...)...)
		cmd.Env = append(sandboxEnv(t), "XDG_CONFIG_HOME="+cfg, "NO_COLOR=1")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = strings.NewReader(`{"session_id":"sess-gold","cost":{"total_cost_usd":4.1,"total_duration_ms":3600000}}`)
		out, _ := cmd.Output()
		return string(out)
	}
	if got := run("--set-layout=detailed"); !strings.Contains(got, "detailed") {
		t.Fatalf("set-layout said %q", got)
	}
	if got := run(); !strings.Contains(got, "spent $4.10") {
		t.Errorf("the saved detailed layout was not used: %q", got)
	}
	run("--set-layout=auto")
	if got := run(); strings.Contains(got, "spent") {
		t.Errorf("auto must fall back to balanced: %q", got)
	}
}

// modesRender runs one status line layout against a stub that serves the series, spend and request
// reads the way the proxy does (series buckets aligned to the UNIX epoch, not to the user's day), in
// time zone tz with the clock pinned to nowS. It renders twice: the series and totals are background
// reads, so the first render may have only the cache.
func modesRender(t *testing.T, tz string, nowS int64, layout string, width int) string {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Skipf("no tz database for %s: %v", tz, err)
	}
	const hour = int64(3_600_000)
	now := nowS * 1000
	localMidnight := func(daysAgo int) int64 {
		n := time.Unix(nowS, 0).In(loc)
		return time.Date(n.Year(), n.Month(), n.Day()-daysAgo, 0, 0, 0, 0, loc).UnixMilli()
	}
	epochHour := func(ms int64) int64 { return ms / hour * hour } // the proxy buckets by (ts/bucket)*bucket
	th := func(id string, agoSec int) string {
		return fmt.Sprintf(`{"id":%q,"main":%t,"model":"claude-sonnet-5-5","last_ts":%d,"end_ts":%d,"ttl_s":300,`+
			`"prefix_tokens":412000,"priced":true,"rewrite_usd":1.20}`, id, id == "", now-int64(agoSec)*1000, now-int64(agoSec)*1000)
	}
	cache := fmt.Sprintf(`{"session":"s","model":"claude-sonnet-5-5","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":412000,`+
		`"priced":true,"rewrite_usd":1.20,"window":1000000,"window_exact":true,"threads":[%s,%s,%s,%s]}`,
		now-100_000, now, th("", 100), th("a:01", 30), th("a:02", 250), th("a:03", 3000))
	extra := func(mux *http.ServeMux) {
		mux.HandleFunc("/api/series", func(w http.ResponseWriter, r *http.Request) {
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			var b []string
			add := func(ts int64, body string) {
				if ts >= since && ts <= now {
					b = append(b, fmt.Sprintf(`{"ts":%d,%s}`, epochHour(ts), body))
				}
			}
			for n := 13; n >= 1; n-- { // one distinct amount per past day, oldest 1 ... yesterday 13, at local noon
				add(localMidnight(n)+12*hour, fmt.Sprintf(`"saved_usd":%d`, 14-n))
			}
			for h, n := range map[int64]int{2: 1, 3: 2, 6: 6, 7: 1} { // today's misses by local hour
				add(localMidnight(0)+h*hour, fmt.Sprintf(`"cache_misses":%d`, n))
			}
			fmt.Fprintf(w, `{"buckets":[%s]}`, strings.Join(b, ","))
		})
		mux.HandleFunc("/api/spend/explain", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"daily":[{"day":%d,"rewrite_usd":1.4}]}`, localMidnight(0))
		})
		mux.HandleFunc("/api/requests", func(w http.ResponseWriter, _ *http.Request) {
			var rows []string
			for i := 0; i < 20; i++ { // newest first, as the API pages
				ctx := 200_000 + (19-i)*10_000
				read, write := ctx-2000, 2000
				switch i {
				case 4, 5:
					read, write = ctx/2, ctx/2 // partial
				case 11:
					read, write = 0, ctx // a full rewrite
				}
				rows = append(rows, fmt.Sprintf(`{"id":%d,"ts":%d,"thread_id":"","messages":%d,"fresh_input":0,"cache_read":%d,"cache_write":%d}`,
					i, now-int64(i)*60_000-100_000, 40+(19-i), read, write))
			}
			fmt.Fprintf(w, `{"requests":[%s]}`, strings.Join(rows, ","))
		})
	}
	stdin := `{"session_id":"sess-gold","cost":{"total_cost_usd":0.41},"context_window":{"total_input_tokens":412000,"total_output_tokens":900,"context_window_size":200000}}`
	env := routedEnv(cgStub(t, cache, extra))
	env["CG_STATUSLINE_COLUMNS"], env["CG_STATUSLINE_NOW"], env["TZ"] = fmt.Sprint(width), fmt.Sprint(nowS), tz
	env["NO_COLOR"] = "1"
	runStatusline(t, env, stdin, "--layout="+layout)
	out, code, _ := runStatusline(t, env, stdin, "--layout="+layout)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := strings.TrimRight(out, "\n")
	for _, ln := range strings.Split(got, "\n") {
		if n := len([]rune(ln)); n > width {
			t.Errorf("line is %d columns, wider than %d: %q", n, width, ln)
		}
	}
	return got
}

// TestStatuslineGoldenModes pins the five terminal-view layouts at three widths (UTC, 08:00).
func TestStatuslineGoldenModes(t *testing.T) {
	for _, c := range []struct {
		layout string
		width  int
		want   string
	}{
		{"spark", 60,
			"saved $4.20 today · last 14 days ▁▂▂▃▄▄▅▅▆▇▇██▃\ncache ● ████░░ 3:20"}, {"spark", 80,
			"saved $4.20 today · last 14 days ▁▂▂▃▄▄▅▅▆▇▇██▃\ncache ● ████░░ 3:20"}, {"spark", 120,
			"saved $4.20 today · last 14 days ▁▂▂▃▄▄▅▅▆▇▇██▃\ncache ● ████░░ 3:20"},
		{"strip", 60,
			"last 20 ■■■■■■■■✖■■■■■▨▨■■■■ ✖ miss $1.40 today\ncache ● ████░░ 3:20"}, {"strip", 80,
			"last 20 requests ■■■■■■■■✖■■■■■▨▨■■■■  ✖ miss  $1.40 lost today\ncache ● ████░░ 3:20"}, {"strip", 120,
			"last 20 requests ■■■■■■■■✖■■■■■▨▨■■■■  ✖ miss ▨ partial ■ hit  $1.40 lost today\ncache ● ████░░ 3:20"},
		{"heat", 60,
			"misses/hour 0h  ░▒  █░ ···············24h  $1.40 lost today\ncache ● ████░░ 3:20"}, {"heat", 80,
			"misses/hour 0h  ░▒  █░ ···············24h  $1.40 lost today\ncache ● ████░░ 3:20"}, {"heat", 120,
			"misses/hour 0h  ░▒  █░ ···············24h  $1.40 lost today\ncache ● ████░░ 3:20"},
		{"fill", 60,
			"context ▒▒▒▒▒█████░░░░░░░░░░░░░░ 41% · full in 58m\ncache ● ████░░ 3:20"}, {"fill", 80,
			"context ▒▒▒▒▒▒▒▒████████░░░░░░░░░░░░░░░░░░░░░░░░ 41% · full in 58m\ncache ● ████░░ 3:20"}, {"fill", 120,
			"context ▒▒▒▒▒▒▒▒████████░░░░░░░░░░░░░░░░░░░░░░░░ 41% · full in 58m · a miss now costs $1.20\ncache ● ████░░ 3:20"},
		{"watch", 60,
			"saved $0.31 │ cache ● ████░░ 3:20\nagent caches (2) ████ 4:30  █░░░ 0:50"}, {"watch", 80,
			"saved $0.31 │ cache ● ████░░ 3:20\nagent caches (2) ████ 4:30  █░░░ 0:50"}, {"watch", 120,
			"saved $0.31 │ cache ● ████░░ 3:20\nagent caches (2) ████ 4:30  █░░░ 0:50"},
	} {
		t.Run(fmt.Sprintf("%s/%d", c.layout, c.width), func(t *testing.T) {
			if got := modesRender(t, "UTC", 1_800_000_000, c.layout, c.width); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// The day and hour views must follow the USER's local calendar. East of UTC the local date runs ahead
// of the proxy's UTC buckets, west of it behind; Kolkata is on a half hour. The same data (one distinct
// amount per local day, misses at local hours 2, 3, 6, 7) must give the same sparkline in every zone,
// and the heat strip must end at the local hour reached.
func TestStatuslineModesFollowLocalTime(t *testing.T) {
	const nowS = 1_800_020_000 // 13:33 UTC: 19:03 in Kolkata, 14:33 in Berlin, 05:33 in Los Angeles
	spark := "saved $4.20 today · last 14 days ▁▂▂▃▄▄▅▅▆▇▇██▃\ncache ● ████░░ 3:20"
	for _, c := range []struct{ tz, heat string }{
		{"Asia/Kolkata", "misses/hour 0h  ░▒  █░            ····24h  $1.40 lost today\ncache ● ████░░ 3:20"},
		{"Europe/Berlin", "misses/hour 0h  ░▒  █░       ·········24h  $1.40 lost today\ncache ● ████░░ 3:20"},
		{"America/Los_Angeles", "misses/hour 0h  ▒█  ··················24h  $1.40 lost today\ncache ● ████░░ 3:20"},
	} {
		t.Run(c.tz, func(t *testing.T) {
			if got := modesRender(t, c.tz, nowS, "spark", 80); got != spark {
				t.Errorf("spark: got\n%s\nwant\n%s", got, spark)
			}
			if got := modesRender(t, c.tz, nowS, "heat", 80); got != c.heat {
				t.Errorf("heat: got\n%s\nwant\n%s", got, c.heat)
			}
		})
	}
}

// The modes draw with tvlib, so CG_ASCII swaps their glyphs for plain characters.
func TestStatuslineModesAscii(t *testing.T) {
	port := cgStub(t, `{"none":true}`, func(mux *http.ServeMux) {
		mux.HandleFunc("/api/requests", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"requests":[{"ts":2,"fresh_input":0,"cache_read":0,"cache_write":900},{"ts":1,"fresh_input":0,"cache_read":900,"cache_write":0}]}`)) //nolint:errcheck
		})
	})
	env := routedEnv(port)
	env["NO_COLOR"], env["CG_ASCII"] = "1", "1"
	out, _, _ := runStatusline(t, env, `{"session_id":"sess-gold"}`, "--layout=strip")
	if !strings.Contains(out, "last 2 requests oX") {
		t.Errorf("want ascii hit/miss glyphs, got %q", out)
	}
}

// TestStatuslineFinishesInHalfASecondOnASlowProxy: the default layouts must return within 0.5 s wall
// time, Python start-up included, however slow the proxy is (here it sleeps 5 s on every request) and
// whether or not there is a cache to fall back on.
func TestStatuslineFinishesInHalfASecondOnASlowProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		w.Write([]byte(`{"total_saved_usd":1}`)) //nolint:errcheck
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	env := routedEnv(fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	stdin := `{"session_id":"sess-slow","cost":{"total_cost_usd":0.41},"context_window":{"total_input_tokens":52000,"total_output_tokens":900,"context_window_size":200000}}`
	for _, layout := range []string{"minimal", "balanced", "detailed"} {
		t.Run(layout, func(t *testing.T) {
			for i := 0; i < 3; i++ { // the first run has no cache at all; later ones find whatever the earlier ones left
				out, code, elapsed := runStatusline(t, env, stdin, "--layout="+layout)
				if code != 0 {
					t.Fatalf("exit %d", code)
				}
				if elapsed > 500*time.Millisecond {
					t.Errorf("run %d took %v (> 0.5 s) against a proxy that sleeps 5 s: %q", i, elapsed, out)
				}
			}
		})
	}
}

// The refresh lock is an election by exclusive creation: N renders starting at once on the same
// key start exactly one detached refresher, and a lock left by a crashed one only blocks its own window.
func TestStatuslineRefreshLockIsAtomic(t *testing.T) {
	py := requireTool(t, "python3")
	script := `
import importlib.util, sys, threading, os, tempfile, time
spec = importlib.util.spec_from_file_location("statusline", sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
tmp = tempfile.mkdtemp()
path = os.path.join(tmp, "k.json")
spawned = []
class P:  # stands in for the detached refresher
    def __init__(self, *a, **k): spawned.append(1)
m.subprocess.Popen = P
m._refresh = lambda *a, **k: (_ for _ in ()).throw(OSError("down"))   # the cold inline try fails
def burst(n):
    spawned.clear(); bar = threading.Barrier(n)
    def go():
        bar.wait(); m._bg_json("1", "/x", "k", 60)
    ts = [threading.Thread(target=go) for _ in range(n)]
    [t.start() for t in ts]; [t.join() for t in ts]
    return len(spawned)
m._tmpdir = lambda: tmp
path = os.path.join(tmp, "context-guru-statusline-1-k.json")
print("burst", burst(16))                       # exactly one
print("same window again", burst(4))            # the lock holds: none
slot = int(time.time() // m.REFRESH_WINDOW_SECONDS)
for f in os.listdir(tmp):
    if ".lock." in f: os.rename(os.path.join(tmp, f), os.path.join(tmp, f.rsplit(".", 1)[0] + "." + str(slot - 1)))
print("stale lock from an earlier window", burst(4))   # recovers: one
open(path + ".lock", "w").close()
time.sleep(0)
print("legacy lock name is ignored", burst(1) <= 1)
`
	out, err := exec.Command(py, "-c", script, filepath.Join(scriptsDir(t), "statusline.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "burst 1\nsame window again 0\nstale lock from an earlier window 1\nlegacy lock name is ignored True"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// If the stats fetch has not reported when the budget runs out, a cached body still shows (marked
// stale); "loading" is only for when nothing is cached.
func TestStatuslineNeverSaysLoadingWithACache(t *testing.T) {
	py := requireTool(t, "python3")
	script := `
import importlib.util, sys, os, tempfile, json, io, contextlib
spec = importlib.util.spec_from_file_location("statusline", sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
tmp = tempfile.mkdtemp(); m._tmpdir = lambda: tmp
m._our_port = lambda: "1"
m._gather = lambda *a, **k: {}                    # the fetch thread never reported in time
m._read_stdin_json = lambda: {"session_id": "s1", "cost": {"total_cost_usd": 1}, "context_window": {"total_input_tokens": 1000, "total_output_tokens": 10}}
def render():
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf): m.main()
    return buf.getvalue().strip()
print("no cache:", render())
json.dump({"total_saved_usd": 2.5}, open(m._stats_cache_path("1", "s1"), "w"))
print("cache:", render())
`
	cmd := exec.Command(py, "-c", script, filepath.Join(scriptsDir(t), "statusline.py"))
	cmd.Env = append(sandboxEnv(t), "NO_COLOR=1", "CG_STATUSLINE_COLUMNS=100")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || !strings.Contains(lines[0], "loading") {
		t.Errorf("with nothing cached the line should say loading: %q", out)
	}
	rest := strings.Join(lines[1:], "\n")
	if strings.Contains(rest, "loading") || !strings.Contains(rest, "$2.50") || !strings.Contains(rest, "⏳") {
		t.Errorf("with a cache the stale numbers must show, marked ⏳, never loading: %q", rest)
	}
}

// TestStatuslineGoldenWeeklyTip pins the weekly tip. It names every command (of the ones this
// plugin has) not run in the last 7 days, and shows nothing when all ran this week. It waits a week
// on a new install (a never-run command counts from the first time the status line saw the
// install), reads local files only, treats an unreadable history as no tip, can be turned off, and
// is the first thing a narrow terminal shortens (to the command stale longest), then drops.
func TestStatuslineGoldenWeeklyTip(t *testing.T) {
	const now = 1_800_000_000 // seconds
	const day = 86400
	// The script runs from a copy whose skills/ directory says which commands exist.
	plugin := func(skills ...string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"statusline.py", "settings.py", "tvlib.py"} {
			b, err := os.ReadFile(filepath.Join(scriptsDir(t), f))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "scripts", f), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range skills {
			if err := os.MkdirAll(filepath.Join(dir, "skills", s), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "skills", s, "SKILL.md"), []byte("---\nname: "+s+"\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Join(dir, "scripts", "statusline.py")
	}
	both := plugin("save-more", "remove-mcp-and-skills")
	cache := fmt.Sprintf(`{"session":"s","model":"m","last_ts":%d,"now_ms":%d,"ttl_s":300,"prefix_tokens":52000,`+
		`"priced":true,"rewrite_usd":0.30,"window":1000000,"window_exact":true,`+
		`"money":{"main":{"saved_usd":0.31,"baseline_usd":2.10,"threads":1},"others":{"saved_usd":0,"baseline_usd":0,"threads":0}}}`,
		(now-60)*1000, now*1000)
	port := cgStub(t, cache)
	stdin := `{"session_id":"sess-tip","cost":{"total_cost_usd":0.41},` +
		`"context_window":{"total_input_tokens":52000,"total_output_tokens":900,"context_window_size":200000}}`
	const lines = "saved $0.31 of $2.10 session\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"
	tipBoth := "Tip: you haven't run /context-guru:save-more or /context-guru:remove-mcp-and-skills for over a week — worth a shot."
	py := requireTool(t, "python3")
	for _, c := range []struct {
		name, script, lastRun string
		firstSeen             int // days ago; -1 = no file (a brand-new install)
		args                  []string
		tipsOff               bool
		width                 int
		want                  string
	}{
		{"both-ran-recently", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-2*day, now-6*day), 30, nil, false, 200, lines},
		{"one-stale", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-2*day, now-20*day), 30, nil, false, 200,
			lines + "\nTip: you haven't run /context-guru:remove-mcp-and-skills for over a week — worth a shot."},
		{"exactly-7-days", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-7*day, now-7*day), 30, nil, false, 200, lines},
		{"7-days-and-a-second", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-7*day-1, now-2*day), 30, nil, false, 200,
			lines + "\nTip: you haven't run /context-guru:save-more for over a week — worth a shot."},
		{"unreadable-history", both, `{not json`, 30, nil, false, 200, lines},
		{"both-ran-8-days-ago", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-8*day, now-8*day), 30, nil, false, 200,
			lines + "\n" + tipBoth},
		{"never-run-week-old-install", both, "", 8, nil, false, 200, lines + "\n" + tipBoth},
		{"never-run-new-install", both, "", -1, nil, false, 200, lines},
		{"never-run-2-day-install", both, "", 2, nil, false, 200, lines},
		{"only-one-command-installed", plugin("save-more"), "", 30, nil, false, 200,
			lines + "\nTip: you haven't run /context-guru:save-more for over a week — worth a shot."},
		{"no-command-installed", plugin(), "", 30, nil, false, 200, lines},
		{"tips-off", both, "", 30, nil, true, 200, lines},
		{"hidden", both, "", 30, []string{"--hide=tip"}, false, 200, lines},
		// Narrow: the short form first, naming the command stale longest, then no tip at all.
		{"narrow-short-form", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-8*day, now-20*day), 30, nil, false, 80,
			lines + "\nTip: try /context-guru:remove-mcp-and-skills (not run for 7+ days) (+1)"},
		{"narrow-one-stale-short-form", both, fmt.Sprintf(`{"save-more":%d,"remove-mcp-and-skills":%d}`, now-8*day, now-2*day), 30, nil, false, 60,
			lines + "\nTip: try /context-guru:save-more (not run for 7+ days)"},
		{"narrower-drops-the-tip", both, "", 30, nil, false, 45,
			"saved $0.31 of $2.10 session\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, "state")
			if err := os.MkdirAll(state, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.lastRun != "" {
				if err := os.WriteFile(filepath.Join(state, "last-run.json"), []byte(c.lastRun), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.firstSeen >= 0 {
				seen := fmt.Sprint(now - c.firstSeen*day)
				if err := os.WriteFile(filepath.Join(state, "statusline-first-seen"), []byte(seen), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env := append(sandboxEnv(t), "TMPDIR="+t.TempDir(), "NO_COLOR=1", "CONTEXT_GURU_STATE="+state,
				"XDG_CONFIG_HOME="+filepath.Join(home, "cfg"), fmt.Sprintf("CG_STATUSLINE_NOW=%d", now),
				fmt.Sprintf("CG_STATUSLINE_COLUMNS=%d", c.width))
			for k, v := range routedEnv(port) {
				env = append(env, k+"="+v)
			}
			if c.tipsOff {
				off := exec.Command(py, c.script, "--set-tips=off")
				off.Env = env
				if b, err := off.CombinedOutput(); err != nil || !strings.Contains(string(b), "off") {
					t.Fatalf("--set-tips=off: %v %s", err, b)
				}
			}
			cmd := exec.Command(py, append([]string{c.script, "--layout=balanced"}, c.args...)...)
			cmd.Env, cmd.Stdin = env, strings.NewReader(stdin)
			start := time.Now()
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("statusline.py: %v (%s)", err, b)
			}
			if got := strings.TrimRight(string(b), "\n"); got != c.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, c.want)
			}
			if d := time.Since(start); d > 500*time.Millisecond {
				t.Errorf("render took %v; the budget is 0.5 s", d)
			}
			if c.firstSeen < 0 {
				if _, err := os.Stat(filepath.Join(state, "statusline-first-seen")); err != nil {
					t.Errorf("a new install must record when the status line first saw it: %v", err)
				}
			}
		})
	}
}

// TestTipCommandsRecordTheirRuns: the tip is only honest if each command it names records its runs.
// Every TIP_COMMANDS skill this plugin ships must call `settings.py record-run <its name>`.
func TestTipCommandsRecordTheirRuns(t *testing.T) {
	for _, name := range []string{"save-more", "remove-mcp-and-skills"} {
		b, err := os.ReadFile(filepath.Join(scriptsDir(t), "..", "skills", name, "SKILL.md"))
		if os.IsNotExist(err) {
			continue // not shipped yet: the tip does not name it either
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "record-run "+name) {
			t.Errorf("skills/%s/SKILL.md never runs `settings.py record-run %s`, so the weekly tip would name it as unused", name, name)
		}
	}
}

// TestRecordRunFeedsTheWeeklyTip: `settings.py record-run <command>` is how the commands note a
// run. It prints nothing on success (the skills chain it after their report), merges its key into
// last-run.json, and refuses a name the tip does not know.
func TestRecordRunFeedsTheWeeklyTip(t *testing.T) {
	py := requireTool(t, "python3")
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "last-run.json"), []byte(`{"remove-mcp-and-skills": 5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(name string) ([]byte, error) {
		cmd := exec.Command(py, filepath.Join(scriptsDir(t), "settings.py"), "record-run", name)
		cmd.Env = append(sandboxEnv(t), "CONTEXT_GURU_STATE="+state)
		return cmd.Output() // stdout only: it lands in the command's report
	}
	if b, err := run("save-more"); err != nil || len(b) != 0 {
		t.Fatalf("record-run save-more: err %v, stdout %q; want silent success", err, b)
	}
	var runs map[string]float64
	b, _ := os.ReadFile(filepath.Join(state, "last-run.json"))
	if err := json.Unmarshal(b, &runs); err != nil {
		t.Fatal(err)
	}
	if runs["remove-mcp-and-skills"] != 5 || time.Since(time.Unix(int64(runs["save-more"]), 0)) > time.Minute {
		t.Errorf("last-run.json = %s, want the old key kept and save-more stamped now", b)
	}
	if _, err := run("something-else"); err == nil {
		t.Error("record-run accepted a command the tip does not name")
	}
}

// TestStatuslineMoneyShowsWhenCachestateIsSlowerThanTheRender is #570. A session with hundreds of
// threads made /api/cachestate slower than the render budget, so every render's read failed, the
// last copy aged out after ten minutes, and the line fell back to a bare `saved $x`. The read now
// starts a detached refresher when it misses the budget, so the slow answer still reaches the disk
// cache and a later render shows the money line. The refresher also removes its own key's old
// partial .tmp and old lock files, and nothing of another key's or a fresh one.
func TestStatuslineMoneyShowsWhenCachestateIsSlowerThanTheRender(t *testing.T) {
	const now = 1_800_000_000_000
	cache := fmt.Sprintf(`{"session":"s","model":"m","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,`+
		`"money":{"main":{"saved_usd":50.32,"baseline_usd":250,"threads":1},"others":{"saved_usd":60.03,"baseline_usd":300,"threads":277}}}`,
		now-60_000, now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"total_saved_usd": 71.43}`)) //nolint:errcheck
	})
	mux.HandleFunc("/api/cachestate", func(w http.ResponseWriter, r *http.Request) {
		select { // slower than the 0.36 s render budget, as #570 measured (0.53-0.64 s)
		case <-time.After(700 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(cache)) //nolint:errcheck
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	tmp := t.TempDir()
	key := filepath.Join(tmp, "context-guru-statusline-"+port+"-cs-sess-big.json")
	other := filepath.Join(tmp, "context-guru-statusline-"+port+"-cs-sess-other.json")
	oldTmp, newTmp, oldLock := key+".111.tmp", key+".222.tmp", key+".lock.12"
	otherTmp, otherLock := other+".333.tmp", other+".lock.12"
	for _, f := range []string{oldTmp, newTmp, oldLock, otherTmp, otherLock} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if f == newTmp {
			continue
		}
		if err := os.Chtimes(f, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	env := routedEnv(port)
	env["TMPDIR"] = tmp
	stdin := `{"session_id":"sess-big","cost":{"total_cost_usd":4.1},"context_window":{"total_input_tokens":52000,"total_output_tokens":900,"context_window_size":200000}}`
	want := "saved $50.32 of $250.00 session · saved $60.03 of $300.00 subagents"
	deadline := time.Now().Add(8 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		var code int
		var elapsed time.Duration
		out, code, elapsed = runStatusline(t, env, stdin, "--layout=balanced")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("render took %v; the budget is 0.5 s", elapsed)
		}
		if strings.Contains(out, want) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("the money line never showed against a slow /api/cachestate; last render %q", out)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Errorf("an hour-old partial .tmp must be removed by the refresher: %v", err)
	}
	if _, err := os.Stat(oldLock); !os.IsNotExist(err) {
		t.Errorf("an hour-old lock of a past window must be removed by the refresher: %v", err)
	}
	if _, err := os.Stat(newTmp); err != nil {
		t.Errorf("a fresh .tmp may belong to a write in progress and must stay: %v", err)
	}
	for _, f := range []string{otherTmp, otherLock} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("another session's file %s is not this refresher's to remove: %v", filepath.Base(f), err)
		}
	}
}

// TestStatuslineCachestateRefreshIsOnePerWindow: a cachestate read that misses the budget starts
// a detached refresher, but N renders at once start exactly one, and the next one starts only in
// the next CACHESTATE_REFRESH_WINDOW_SECONDS window (5 s, not _bg_json's 30 s).
func TestStatuslineCachestateRefreshIsOnePerWindow(t *testing.T) {
	py := requireTool(t, "python3")
	script := `
import importlib.util, sys, threading, os, tempfile
spec = importlib.util.spec_from_file_location("statusline", sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
tmp = tempfile.mkdtemp(); m._tmpdir = lambda: tmp
spawned = []
class P:  # stands in for the detached refresher
    def __init__(self, *a, **k): spawned.append(1)
m.subprocess.Popen = P
m._http_get = lambda *a, **k: (_ for _ in ()).throw(TimeoutError("slow"))  # every inline read misses the budget
w = m.CACHESTATE_REFRESH_WINDOW_SECONDS
base = 30 * 1000000 + 1.0  # 1 s into a window of both lengths
clock = [base]; m.time.time = lambda: clock[0]
def burst(n):
    spawned.clear(); bar = threading.Barrier(n)
    def go():
        bar.wait(); m._fetch_json("1", "/api/cachestate?session=s", "cs-s", 2.0, w)
    ts = [threading.Thread(target=go) for _ in range(n)]
    [t.start() for t in ts]; [t.join() for t in ts]
    return len(spawned)
print("window", w)
print("burst", burst(16))              # exactly one
print("same window", burst(4))         # none
clock[0] = base + w + 0.5              # the next 5 s window, still inside the same 30 s one
print("next window", burst(4))         # one
`
	out, err := exec.Command(py, "-c", script, filepath.Join(scriptsDir(t), "statusline.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "window 5.0\nburst 1\nsame window 0\nnext window 1"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
