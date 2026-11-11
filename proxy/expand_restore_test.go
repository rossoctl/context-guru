package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Fixed restore end to end (#407), on the path that matters most: the IN-BAND continuation, where
// the proxy answers context_guru_expand inside the request and the client never sees the call or
// its result. Session e8f16627 request 3552 was exactly this (upstream_rounds=2).

const restoreProbe = "the original tool output line that had to come back"

var restoreBig = strings.Repeat(restoreProbe+"\n", 200)

// extract is the compactor on purpose: e8f16627's output was compacted by extract, which has no
// replay path, and the first version of this fix did not reach it.
const restorePipeline = "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 5}\n"

var restoreMarkerRe = regexp.MustCompile(`<<cg:([A-Za-z0-9_-]{1,64})>>`)

func unescapeMarkers(s string) string {
	return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(s)
}

// restoreRig is a proxy in front of an upstream that, when told to, answers the next request by
// expanding the first marker it finds in the transcript, then answers the continuation.
type restoreRig struct {
	t      *testing.T
	wire   string // "anthropic" | "responses"
	srv    *httptest.Server
	st     store.Store
	mu     sync.Mutex
	bodies [][]byte
	expand bool
}

func newRestoreRig(t *testing.T, wire, yaml string) *restoreRig {
	rg := &restoreRig{t: t, wire: wire}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rg.mu.Lock()
		rg.bodies = append(rg.bodies, b)
		doExpand := rg.expand
		rg.expand = false
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		field := "messages"
		if wire == "responses" {
			field = "input"
		}
		id := ""
		if m := restoreMarkerRe.FindStringSubmatch(unescapeMarkers(gjson.GetBytes(b, field).Raw)); m != nil {
			id = m[1]
		}
		switch {
		case wire == "responses" && doExpand:
			w.Write([]byte(`{"id":"r1","output":[{"type":"function_call","call_id":"call_x","name":"context_guru_expand","arguments":"{\"id\":\"` + id + `\"}"}]}`))
		case wire == "responses":
			w.Write([]byte(`{"id":"r2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
		case doExpand:
			w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu_x","name":"context_guru_expand","input":{"id":"` + id + `"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":1}}`))
		default:
			w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`))
		}
	}))
	t.Cleanup(up.Close)
	h, st := buildHandler(t, yaml, up.URL)
	rg.st = st
	rg.srv = httptest.NewServer(h.Mux())
	t.Cleanup(rg.srv.Close)
	return rg
}

func (rg *restoreRig) field() string {
	if rg.wire == "responses" {
		return "input"
	}
	return "messages"
}

// post sends one client turn and returns the upstream bodies it caused.
func (rg *restoreRig) post(msgs []any, expand bool) [][]byte {
	rg.t.Helper()
	rg.mu.Lock()
	rg.expand = expand
	n := len(rg.bodies)
	rg.mu.Unlock()
	var b []byte
	path := "/anthropic/v1/messages"
	if rg.wire == "responses" {
		path = "/openai/v1/responses"
		b, _ = json.Marshal(map[string]any{"model": "gpt-5.6", "input": msgs,
			"tools": []any{map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}}}})
	} else {
		b, _ = json.Marshal(map[string]any{"model": "claude-x", "max_tokens": 100, "messages": msgs,
			"tools": []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}})
	}
	resp, err := http.Post(rg.srv.URL+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		rg.t.Fatal(err)
	}
	resp.Body.Close()
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.bodies[n:]
}

func (rg *restoreRig) msgs(body []byte) []gjson.Result {
	return gjson.GetBytes(body, rg.field()).Array()
}

// transcript builds the client's history, in its dialect: a Read whose result is the big output,
// then any number of (assistant reply, user follow-up) turns.
func restoreTranscript(wire string, replies ...string) []any {
	var out []any
	if wire == "responses" {
		out = []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "Read", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": restoreBig},
		}
		for _, r := range replies {
			out = append(out,
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": r}}},
				map[string]any{"role": "user", "content": "next after " + r})
		}
		return out
	}
	out = []any{
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": restoreBig}}},
	}
	for _, r := range replies {
		out = append(out,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": r}}},
			map[string]any{"role": "user", "content": "next after " + r})
	}
	return out
}

// copies is how many full copies of the big output a body carries.
func copies(body []byte) int { return strings.Count(string(body), restoreProbe) / 200 }

// turnN drives the expanding turn and checks it is the in-band shape this whole test is about.
func (rg *restoreRig) turnN() (sent []byte, id string) {
	rg.t.Helper()
	rounds := rg.post(restoreTranscript(rg.wire), true)
	if len(rounds) != 2 {
		rg.t.Fatalf("turn N must drive one in-band continuation, got %d upstream rounds", len(rounds))
	}
	m := restoreMarkerRe.FindStringSubmatch(unescapeMarkers(gjson.GetBytes(rounds[0], rg.field()).Raw))
	if copies(rounds[0]) != 0 || m == nil {
		rg.t.Fatalf("turn N did not compact the output behind a marker, so there is nothing to restore: %.600s", gjson.GetBytes(rounds[0], rg.field()).Raw)
	}
	if copies(rounds[1]) != 1 {
		rg.t.Fatalf("the in-band continuation did not carry the original (%d copies)", copies(rounds[1]))
	}
	return rounds[0], m[1]
}

func TestFixedRestoreAfterAnInBandExpand(t *testing.T) {
	for _, wire := range []string{"anthropic", "responses"} {
		t.Run(wire, func(t *testing.T) {
			// CONTROL, in the same test: with the switch off, the very same turns flip the original
			// back to full at its own position. This is what proves the comparison below can see a
			// flip at all.
			off := newRestoreRig(t, wire, restorePipeline)
			sentOff, _ := off.turnN()
			n1off := off.post(restoreTranscript(wire, "done"), false)[0]
			if off.msgs(n1off)[2].Raw == off.msgs(sentOff)[2].Raw || copies(n1off) != 1 {
				t.Fatalf("control: with fixed restore off the expanded output did not revert in place " +
					"(today's behaviour), so this fixture cannot detect a flip")
			}

			rg := newRestoreRig(t, wire, restorePipeline+"expand:\n  fixed_restore: true\n")
			sentN, id := rg.turnN()
			L := len(rg.msgs(sentN))
			n1 := rg.post(restoreTranscript(wire, "done"), false)
			n2 := rg.post(restoreTranscript(wire, "done", "done"), false)
			if len(n1) != 1 || len(n2) != 1 {
				t.Fatalf("turns N+1 and N+2 must be one upstream round each, got %d and %d", len(n1), len(n2))
			}
			client := restoreTranscript(wire, "done")
			cb, _ := json.Marshal(client)
			if strings.Contains(string(cb), "context_guru_expand") {
				t.Fatal("precondition: the client's history must hold no expand call or result")
			}
			for _, tc := range []struct {
				turn   string
				body   []byte
				client int
			}{{"N+1", n1[0], L + 2}, {"N+2", n2[0], L + 4}} {
				turn, body := tc.turn, tc.body
				got := rg.msgs(body)
				// PRECONDITION: the new path ran — the client's messages plus exactly one.
				if len(got) != tc.client+1 {
					t.Fatalf("%s: %d messages, want the client's %d plus one restored copy", turn, len(got), tc.client)
				}
				// (a) every byte before the inserted message is turn N's prefix — including the
				// original at its own position, which stays COMPACTED (d: extract did not flip).
				for i := 0; i < L; i++ {
					if got[i].Raw != rg.msgs(sentN)[i].Raw {
						t.Fatalf("%s: message %d differs from turn N's — a change inside the cached prefix:\n  N:   %.160s\n  now: %.160s",
							turn, i, rg.msgs(sentN)[i].Raw, got[i].Raw)
					}
				}
				for _, f := range []string{"tools", "system", "instructions"} {
					if gjson.GetBytes(body, f).Raw != gjson.GetBytes(sentN, f).Raw {
						t.Fatalf("%s: %s changed", turn, f)
					}
				}
				// (b) the restored copy sits at index L, the first position after the expanding turn's
				// request, labelled with the marker the agent expanded.
				r := unescapeMarkers(got[L].Raw)
				if got[L].Get("role").String() != "user" || !strings.Contains(r, "<<cg:"+id+">>") {
					t.Fatalf("%s: message %d is not the restored copy of %s: %.200s", turn, L, id, got[L].Raw)
				}
				// (c) the content reaches the model, once, although the client's history never had it
				// outside the (compacted) original.
				if copies(body) != 1 || strings.Count(got[L].Raw, restoreProbe) != 200 {
					t.Fatalf("%s: carries %d full copies of the expanded output, want exactly 1, in the "+
						"restored message", turn, copies(body))
				}
			}
			// (b) byte-identical across turns.
			if rg.msgs(n1[0])[L].Raw != rg.msgs(n2[0])[L].Raw {
				t.Fatal("the restored copy changed between N+1 and N+2")
			}
			// And N+2's whole prefix through N+1's last message is N+1's, restored copy included.
			a, b := rg.msgs(n1[0]), rg.msgs(n2[0])
			for i := range a {
				if a[i].Raw != b[i].Raw {
					t.Fatalf("N+2 message %d differs from N+1's", i)
				}
			}
		})
	}
}

// FAIL OPEN: whatever makes the anchor unusable, the turn behaves exactly as with the switch off —
// the original in full at its own position, no inserted copy — never "compacted with no copy".
func TestFixedRestoreFailsOpenToKeptVerbatim(t *testing.T) {
	t.Run("the store lost the anchor", func(t *testing.T) {
		rg := newRestoreRig(t, "anthropic", restorePipeline+"expand:\n  fixed_restore: true\n")
		rg.turnN()
		next := restoreTranscript("anthropic", "done")
		nb, _ := json.Marshal(map[string]any{"messages": next})
		sess := apply.SessionIDFor("", "", bschemas.Anthropic, nb)
		if gjson.GetBytes(func() []byte { b, _ := rg.st.Get(store.RestorePrefix + sess); return b }(), "#").Int() != 1 {
			t.Fatalf("precondition: no anchor recorded under session %q", sess)
		}
		rg.st.Put(store.RestorePrefix+sess, []byte("[]"))
		body := rg.post(next, false)[0]
		got := rg.msgs(body)
		if len(got) != len(next) || copies(body) != 1 || !strings.Contains(got[2].Raw, restoreProbe) {
			t.Fatalf("with the anchor gone the original must go out in full in place and nothing be "+
				"inserted: %d messages (client sent %d), %d copies", len(got), len(next), copies(body))
		}
	})
	t.Run("the client compacted its history", func(t *testing.T) {
		rg := newRestoreRig(t, "anthropic", restorePipeline+"expand:\n  fixed_restore: true\n")
		rg.turnN()
		// Same session head (system + first user message), different transcript after it.
		compacted := []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "summary"}}},
			map[string]any{"role": "user", "content": "continue"},
		}
		body := rg.post(compacted, false)[0]
		if n := len(rg.msgs(body)); n != len(compacted) || strings.Contains(unescapeMarkers(string(body)), "is already expanded") {
			t.Fatalf("an anchor was honoured in a transcript that no longer has its message (%d messages)", n)
		}
	})
}

// THE REPAIR PATH: the client received our tool_use and answered it itself. The repaired answer
// is a pointer ("present in the transcript above"), so under fixed restore the copy it points at is
// inserted right before the message holding the call, and the original stays compacted.
func TestFixedRestoreOnTheRepairPath(t *testing.T) {
	rg := newRestoreRig(t, "anthropic", restorePipeline+"expand:\n  fixed_restore: true\n")
	first := rg.post(restoreTranscript("anthropic"), false)[0]
	m := restoreMarkerRe.FindStringSubmatch(unescapeMarkers(gjson.GetBytes(first, "messages").Raw))
	if m == nil || copies(first) != 0 {
		t.Fatal("precondition: turn N did not compact the output")
	}
	hist := append(restoreTranscript("anthropic"),
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "tu_c", "name": "context_guru_expand", "input": map[string]any{"id": m[1]}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu_c", "is_error": true,
			"content": "<tool_use_error>Error: No such tool available: context_guru_expand</tool_use_error>"}}})
	for turn := 0; turn < 2; turn++ {
		body := rg.post(hist, false)[0]
		got := rg.msgs(body)
		if len(got) != len(hist)+1 {
			t.Fatalf("turn %d: %d messages, want the client's %d plus the restored copy", turn, len(got), len(hist))
		}
		if got[2].Raw != gjson.GetBytes(first, "messages.2").Raw {
			t.Fatalf("turn %d: the original flipped at its own position", turn)
		}
		if !strings.Contains(unescapeMarkers(got[3].Raw), "<<cg:"+m[1]+">>") || got[4].Get("content.0.name").String() != "context_guru_expand" {
			t.Fatalf("turn %d: the restored copy is not right before the expanding turn: %.200s", turn, got[3].Raw)
		}
		if copies(body) != 1 || !strings.Contains(got[5].Raw, "present in the transcript above") {
			t.Fatalf("turn %d: %d copies; answer %.200s", turn, copies(body), got[5].Raw)
		}
	}
}
