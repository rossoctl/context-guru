package plugin

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// cgStub answers the three reads the redesigned status line makes. cache is the /api/cachestate
// body; everything is fixed, so the rendered line is byte-for-byte reproducible.
func cgStub(t *testing.T, cache string) (port string) {
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
	sess := "saved ≈$0.31 session · ≈$4.20 today · ★ ≈$212.40 lifetime"
	for _, c := range []struct {
		name, cache, stdin, layout string
		width                      int
		want                       string
	}{
		{"warm/minimal", cs(60, 300, 52000, "0.30"), small, "minimal", 200, "saved ≈$0.31 │ cache ● █████░ 4:00"},
		{"warm/balanced", cs(60, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"warm/ping", ping(cs(60, 300, 52000, "0.30")), small, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 ↻ │ ctx ░░░░░░░░ 5%"},
		{"warm/detailed", cs(60, 3600, 52000, "0.30"), small, "detailed", 200,
			sess + "\ncache ● ██████ 59:00 │ ctx ░░░░░░░░ 5% │ proxy: 3ms · upstream: 340ms"},
		{"expiring/cheap", cs(258, 300, 52000, "0.30"), small, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ░░░░░░░░ 5%"},
		{"expiring/costly", cs(258, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 │ ctx ███░░░░░ 41% │ ▸ cache gone in 42s: a pause now costs ≈$1.20 to rewrite"},
		{"expiring/ping-covers-it", ping(cs(258, 300, 412000, "1.20")), payload, "balanced", 200,
			sess + "\ncache ◐ █░░░░░ 0:42 ↻ │ ctx ███░░░░░ 41%"},
		{"warm/rewrite-near", cs(60, 300, 900000, "2.70"), full, "balanced", 200,
			sess + "\ncache ● █████░ 4:00 │ ctx ███████░ 90% ⚠ rewrite near │ ▸ context 90% full — /compact"},
		{"cold/balanced", cs(400, 300, 412000, "1.20"), payload, "balanced", 200,
			sess + "\ncache ○ ░░░░░░ cold │ ctx ███░░░░░ 41% │ ▸ cache is cold: your next message costs ≈$1.20 extra — /compact first?"},
		{"cold/minimal", cs(400, 300, 412000, "1.20"), payload, "minimal", 200,
			"saved ≈$0.31 │ cache ○ ░░░░░░ cold │ ▸ cache is cold: your next message costs ≈$1.20 extra — /compact first?"},
		{"cold/narrow", cs(400, 300, 412000, "1.20"), payload, "balanced", 60,
			sess + "\ncache ○ cold │ ▸ cold: next message ≈$1.20 extra — /compact?"},
		{"warm/narrow", cs(60, 300, 52000, "0.30"), small, "balanced", 40,
			"saved ≈$0.31\ncache ● █████░ 4:00 │ ctx ░░░░░░░░ 5%"},
		{"first-run", `{"session":"s","none":true,"now_ms":1}`, `{"session_id":"sess-gold"}`, "balanced", 200,
			"saved ≈$4.20 today · ★ ≈$212.40 lifetime\n◌ no cache yet"}, // a brand-new session claims no session saving yet
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

// Proxy down is `cg!` in every layout; NO_COLOR=unset leaves ANSI on and the line still fits the width.
func TestStatuslineGoldenProxyDownAndColour(t *testing.T) {
	port := cgStub(t, `{"none":true}`)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	if out, _, _ := runStatusline(t, routedEnv(dead), `{}`, "--layout=detailed"); strings.TrimSpace(out) != "cg!" {
		t.Errorf("proxy down: got %q, want cg!", out)
	}
	env := routedEnv(port)
	env["NO_COLOR"] = ""
	out, _, _ := runStatusline(t, env, `{"session_id":"sess-gold"}`)
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("colour expected when NO_COLOR is empty, got %q", out)
	}
}
