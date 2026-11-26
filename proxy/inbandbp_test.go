package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/apply"
	"github.com/tidwall/gjson"
)

// inBandClientBody is a Claude Code-shaped request: 2 system breakpoints and a 1h breakpoint on the
// last user message, which names an offloaded output.
const inBandClientBody = `{"model":"claude","max_tokens":64,` +
	`"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"s2","cache_control":{"type":"ephemeral"}}],` +
	`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"look at <<cg:HASH>>","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`

// twoRoundUpstream calls expand on rounds 1 and 2 and answers on round 3.
func twoRoundUpstream(up *upstreamCapture) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := up.record(r)
		w.Header().Set("Content-Type", "application/json")
		if round <= 2 {
			w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"looking"},` +
				`{"type":"tool_use","id":"toolu_` + string(rune('0'+round)) + `","name":"context_guru_expand","input":{"id":"HASH"}}],"stop_reason":"tool_use"}`))
			return
		}
		w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ANSWERED"}],"stop_reason":"end_turn"}`))
	}))
}

func postInBand(t *testing.T, yaml string) *upstreamCapture {
	t.Helper()
	var up upstreamCapture
	upstream := twoRoundUpstream(&up)
	t.Cleanup(upstream.Close)
	h, st := buildHandler(t, yaml, upstream.URL)
	st.Put("HASH", []byte("THE ORIGINAL CONTENT"))
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/anthropic/v1/messages", "application/json", strings.NewReader(inBandClientBody))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(out), "ANSWERED") || up.hits() != 3 {
		t.Fatalf("want 3 upstream rounds and the answer, got %d rounds:\n%s", up.hits(), out)
	}
	return &up
}

// #784, end to end over a real 2-round in-band expand. Each follow-up ends with the proxy's
// tool_result, and the client's 1h message breakpoint sits on it: moved, not added, so every round
// carries the client's 3 breakpoints and the system marks are untouched.
func TestEachInBandFollowUpCarriesTheClientsBreakpointAtItsEnd(t *testing.T) {
	moved, _, _, _ := apply.InBandBreakpointStats()
	up := postInBand(t, offloadCapablePipeline+"expand:\n  inband_breakpoint: true\n")
	first := up.body(1)
	for n := 2; n <= 3; n++ {
		b := up.body(n)
		last := gjson.GetBytes(b, "messages.@reverse.0.content.@reverse.0")
		if last.Get("type").String() != "tool_result" || last.Get("cache_control").Raw != `{"type":"ephemeral","ttl":"1h"}` {
			t.Fatalf("round %d: last block %s, want the proxy's tool_result with the client's 1h mark", n, last.Raw)
		}
		if got, want := apply.CountBreakpoints(b).Total(), apply.CountBreakpoints(first).Total(); got != want || want != 3 {
			t.Fatalf("round %d: %d breakpoints, round 1 had %d; want 3 on both", n, got, want)
		}
		if gjson.GetBytes(b, "system").Raw != gjson.GetBytes(first, "system").Raw {
			t.Fatalf("round %d: system blocks changed", n)
		}
		if gjson.GetBytes(b, "messages.0.content.0.cache_control").Exists() {
			t.Fatalf("round %d: the client's mark stayed on its message", n)
		}
	}
	if m, _, _, _ := apply.InBandBreakpointStats(); m != moved+2 {
		t.Errorf("moved rose by %d, want 2 (one per follow-up)", m-moved)
	}
}

// By default (and with `expand.inband_breakpoint: false`) each follow-up keeps the client's marks
// where they were: expand billing does not change.
func TestTheInBandBreakpointIsOffByDefault(t *testing.T) {
	for _, extra := range []string{"", "expand:\n  inband_breakpoint: false\n"} {
		up := postInBand(t, offloadCapablePipeline+extra)
		for n := 2; n <= 3; n++ {
			b := up.body(n)
			if gjson.GetBytes(b, "messages.@reverse.0.content.@reverse.0.cache_control").Exists() {
				t.Fatalf("%q round %d: off, yet the follow-up's end carries a mark", extra, n)
			}
			if gjson.GetBytes(b, "messages.0.content.0.cache_control.ttl").String() != "1h" {
				t.Fatalf("%q round %d: the client's mark moved while off", extra, n)
			}
		}
	}
}
