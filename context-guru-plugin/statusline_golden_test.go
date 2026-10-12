package plugin

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
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
		return fmt.Sprintf(`{"session":"s","model":"m","last_ts":%d,"now_ms":%d,"ttl_s":%d,`+
			`"prefix_tokens":%d,"priced":true,"rewrite_usd":%s,"window":1000000,"window_exact":true}`,
			now-agoSec*1000, now, ttl, prefix, usd)
	}
	ping := func(body string) string { return strings.Replace(body, `"priced"`, `"keepalive":true,"priced"`, 1) }
	// 412k tokens in a 1M window; Claude Code itself believes the window is 200k, the proxy knows better.
	payload := `{"session_id":"sess-gold","cost":{"total_cost_usd":0.41},"context_window":` +
		`{"total_input_tokens":412000,"total_output_tokens":900,"context_window_size":200000}}`
	small := strings.Replace(payload, "412000", "52000", 1)
	full := strings.Replace(payload, "412000", "900000", 1)
	sess := "saved $0.31 session · $4.20 today · ★ $212.40 lifetime"
	for _, c := range []struct {
		name, cache, stdin, layout string
		width                      int
		want                       string
	}{
		{"warm/minimal", cs(60, 300, 52000, "0.30"), small, "minimal", 200, "saved $0.31 │ cache ● █████░ 4:00"},
		{"warm/balanced", cs(60, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"warm/ping", ping(cs(60, 300, 52000, "0.30")), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 ↻ │ ctx ░░░░░░░░ 5%"},
		{"warm/detailed", cs(60, 3600, 52000, "0.30"), small, "detailed", 200,
			sess + " │ spent $0.41\ncache ● ██████ 59:00 │ ctx ░░░░░░░░ 5%"},
		{"expiring/cheap", cs(258, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ░░░░░░░░ 5%"},
		{"expiring/costly", cs(258, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ███░░░░░ 41% │ ▸ a pause now costs ≈$1.20 extra"},
		{"expiring/ping-covers-it", ping(cs(258, 300, 412000, "1.20")), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 ↻ │ ctx ███░░░░░ 41%"},
		{"warm/rewrite-near", cs(60, 300, 900000, "2.70"), full, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███████░ 90% ⚠ rewrite near │ ▸ context 90% full — /compact"},
		{"cold/balanced", cs(400, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1:40 │ ctx ███░░░░░ 41% │ ▸ your next message costs ≈$1.20 extra"},
		{"cold/minimal", cs(400, 300, 412000, "1.20"), payload, "minimal", 200,
			"saved $0.31 │ cache ○ ░░░░░░ cold 1:40 │ ▸ your next message costs ≈$1.20 extra"},
		{"cold/narrow", cs(400, 300, 412000, "1.20"), payload, "balanced", 60,
			sess + "\ncache ○ cold 1:40 │ ▸ your next message costs ≈$1.20 extra"},
		{"warm/narrow", cs(60, 300, 52000, "0.30"), small, "balanced", 40,
			"saved $0.31\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"first-run", `{"session":"s","none":true,"now_ms":1}`, `{"session_id":"sess-gold"}`, "balanced", 200,
			"saved $4.20 today · ★ $212.40 lifetime\n◌ no cache yet"}, // a brand-new session claims no session saving yet
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
		return fmt.Sprintf(`{"session":"s","model":%q,"last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":412000,`+
			`"priced":true,"rewrite_usd":1.20,"window":1000000,"window_exact":true,"threads":[%s]}`,
			model, now-lastAgo*1000, now, strings.Join(threads, ","))
	}
	sess := "saved $0.31 session · $4.20 today · ★ $212.40 lifetime"
	m := "claude-sonnet-5-5"
	for _, c := range []struct {
		name, cache, stdin, layout string
		width                      int
		want                       string
	}{
		{"critical", body(m, 290, th("", 290, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ◐ ░░░░░░ 0:10! │ ctx ███░░░░░ 41% │ ▸ a pause now costs ≈$1.20 extra"},
		{"cold-counts-up", body(m, 375, th("", 375, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1:15 │ ctx ███░░░░░ 41% │ ▸ your next message costs ≈$1.20 extra"},
		{"cold-long", body(m, 3900, th("", 3900, 300, 412000, "1.20")), ctxPay(m), "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold 1h00m │ ctx ███░░░░░ 41% │ ▸ your next message costs ≈$1.20 extra"},
		// The latest request of the session was a sub-agent's; the main cache must still count down on its own.
		{"main-not-reset-by-subagent", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("", 200, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ██░░░░ 1:40 │ agent ● 4:55 │ ctx ███░░░░░ 41%"},
		{"two-agents", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ agents 2 · soonest ◐ 0:50 │ ctx ███░░░░░ 41%"},
		{"many-agents", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"),
			th("a:3", 100, 300, 9000, "0.03"), th("a:4", 7, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ agents 4 · soonest ◐ 0:50 │ ctx ███░░░░░ 41%"},
		{"cold-agents-are-finished", body(m, 100, th("a:1", 900, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")),
			ctxPay(m), "balanced", 200, sess + "\ncache ● ████░░ 3:20 │ ctx ███░░░░░ 41%"},
		{"agents-narrow", body(m, 5, th("a:1", 5, 300, 9000, "0.03"), th("a:2", 250, 300, 9000, "0.03"),
			th("a:3", 100, 300, 9000, "0.03"), th("", 100, 300, 412000, "1.20")), ctxPay(m), "balanced", 36,
			"saved $0.31\ncache ● 3:20 │ agents 3 │ ctx 41%"},
		{"model-switch", body(m, 60, th("", 60, 300, 412000, "1.20")), ctxPay("claude-opus-5-5"), "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███░░░░░ 41% │ ▸ model changed: your next message costs ≈$1.20 extra"},
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

// TestStatuslineGoldenModes pins the five terminal-view layouts at three widths. The clock is pinned
// (CG_STATUSLINE_NOW, TZ=UTC) so the day- and hour-based views are reproducible.
func TestStatuslineGoldenModes(t *testing.T) {
	const nowS = 1_800_000_000 // 2027-01-15 08:00:00 UTC
	const now = nowS * 1000
	const day = 86_400_000
	midnight := int64(nowS/86400) * day
	th := func(id string, agoSec int) string {
		return fmt.Sprintf(`{"id":%q,"main":%t,"model":"claude-sonnet-5-5","last_ts":%d,"end_ts":%d,"ttl_s":300,`+
			`"prefix_tokens":412000,"priced":true,"rewrite_usd":1.20}`, id, id == "", now-int64(agoSec)*1000, now-int64(agoSec)*1000)
	}
	cache := fmt.Sprintf(`{"session":"s","model":"claude-sonnet-5-5","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":412000,`+
		`"priced":true,"rewrite_usd":1.20,"window":1000000,"window_exact":true,"threads":[%s,%s,%s,%s]}`,
		now-100_000, now, th("", 100), th("a:01", 30), th("a:02", 250), th("a:03", 3000))
	extra := func(mux *http.ServeMux) {
		mux.HandleFunc("/api/series", func(w http.ResponseWriter, r *http.Request) {
			var b []string
			if strings.Contains(r.URL.RawQuery, "bucket=3600000") { // hourly: misses at 02h, 03h, 06h (6 of them)
				for h, n := range map[int]int{2: 1, 3: 2, 6: 6, 7: 1} {
					b = append(b, fmt.Sprintf(`{"ts":%d,"cache_misses":%d}`, midnight+int64(h)*3_600_000, n))
				}
			} else { // 14 days of saving, rising, then a good day
				for d := 0; d < 13; d++ {
					b = append(b, fmt.Sprintf(`{"ts":%d,"saved_usd":%d}`, midnight-int64(13-d)*day, 1+d%5))
				}
			}
			fmt.Fprintf(w, `{"buckets":[%s]}`, strings.Join(b, ","))
		})
		mux.HandleFunc("/api/spend/explain", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"daily":[{"day":%d,"rewrite_usd":1.4}]}`, midnight)
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
	for _, c := range []struct {
		layout string
		width  int
		want   string
	}{
		{"spark", 60,
			"saved $4.20 today · last 14 days ▂▄▅▇█▂▄▅▇█▂▄▅▇\ncache ● ████░░ 3:20"}, {"spark", 80,
			"saved $4.20 today · last 14 days ▂▄▅▇█▂▄▅▇█▂▄▅▇\ncache ● ████░░ 3:20"}, {"spark", 120,
			"saved $4.20 today · last 14 days ▂▄▅▇█▂▄▅▇█▂▄▅▇\ncache ● ████░░ 3:20"},
		{"strip", 60,
			"last 20 ■■■■■■■■✖■■■■■▨▨■■■■ ✖ miss $1.40 today\ncache ● ████░░ 3:20"}, {"strip", 80,
			"last 20 requests ■■■■■■■■✖■■■■■▨▨■■■■  ✖ miss  $1.40 lost today\ncache ● ████░░ 3:20"}, {"strip", 120,
			"last 20 requests ■■■■■■■■✖■■■■■▨▨■■■■  ✖ miss ▨ partial ■ hit  $1.40 lost today\ncache ● ████░░ 3:20"},
		{"heat", 60,
			"misses per hour 0h  ░▒  █░ ···············24h  $1.40 lost\ncache ● ████░░ 3:20"}, {"heat", 80,
			"misses per hour 0h  ░▒  █░ ···············24h  $1.40 lost\ncache ● ████░░ 3:20"}, {"heat", 120,
			"misses per hour 0h  ░▒  █░ ···············24h  $1.40 lost\ncache ● ████░░ 3:20"},
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
			env := routedEnv(cgStub(t, cache, extra))
			env["CG_STATUSLINE_COLUMNS"], env["CG_STATUSLINE_NOW"], env["TZ"] = fmt.Sprint(c.width), fmt.Sprint(nowS), "UTC"
			env["NO_COLOR"] = "1"
			// the day total and the two series are background reads: the first render may have only the cache
			runStatusline(t, env, stdin, "--layout="+c.layout)
			out, code, _ := runStatusline(t, env, stdin, "--layout="+c.layout)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			got := strings.TrimRight(out, "\n")
			for _, ln := range strings.Split(got, "\n") {
				if n := len([]rune(ln)); n > c.width {
					t.Errorf("line is %d columns, wider than %d: %q", n, c.width, ln)
				}
			}
			if got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
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
