package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dash"
)

// THE REPAIR PATH SENT THE RECOVERED CONTENT UPSTREAM TWICE, ON EVERY TURN, FOREVER (#201).
//
// Two mechanisms, each individually right, composed into it: repairExpandErrors rewrites the
// client's `No such tool available` tool_result with the original on every later turn (the client
// keeps its own copy of the error), AND the same call marks that content kept-verbatim, so
// skipReduce leaves the ORIGINAL message uncompacted at its own position. One copy in place, one in
// the tool_result.
//
// The numbers this test pins, measured before the fix on a 200-line output through `codesafe`:
//
//	without the expand round-trip   252 bytes    1 copy (a marker + head peek)
//	with it, every later turn     21,511 bytes   2 copies
//
// An ~85x blowup against the compacted form, permanently — far larger than the one-off prefix flip
// #201 describes, which is what makes this a defect rather than a design preference.
//
// The control arm is not decoration: without it, "the content appears once" would also pass on a
// pipeline that never compacted that message at all, and the whole finding would be unfounded.
func TestTheRepairPathSendsRecoveredContentUpstreamOnce(t *testing.T) {
	original := strings.Repeat("the original tool output line that had to come back\n", 200)
	const probe = "the original tool output line that had to come back"
	const perCopy = 200 // the probe appears once per line

	var forwarded []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	h, _ := dashHandler(t, up.URL, dash.Options{})
	h.store.Put("HASH", []byte(original))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The transcript a client produces after it answered our tool_use itself: the original output
	// still at its own position, then the expand call, then the client's error answer.
	body := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"call_0","content":` + jsonStr(original) + `},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{` +
		`"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"Error: No such tool available: context_guru_expand"}]}`

	// Two turns: the first establishes the kept-verbatim mark, the second is the steady state every
	// later turn repeats. Both must carry ONE copy.
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json",
			strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(forwarded) < 2 {
		t.Fatalf("expected at least 2 upstream requests, got %d", len(forwarded))
	}
	for i, f := range forwarded {
		if n := strings.Count(f, probe); n != perCopy {
			t.Errorf("turn %d sent the content %.1f times (%d probe hits, %d bytes): the repaired "+
				"tool_result is repeating content that is already in the transcript at its own "+
				"position, on this and every later turn", i+1, float64(n)/perCopy, n, len(f))
		}
	}
	// And the model must be TOLD where it is, or the repair has silently answered its tool call
	// with nothing.
	if !strings.Contains(forwarded[len(forwarded)-1], "present in the transcript above") {
		t.Errorf("the repaired tool_result carries no pointer to the content:\n%s",
			forwarded[len(forwarded)-1])
	}

	// THE CONTROL. Without the expand round-trip the same content compacts to a marker, which is
	// what proves the message was a compaction candidate and the second copy above was real
	// duplication rather than a message the pipeline was never going to touch.
	forwarded = nil
	plain := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"call_0","content":` + jsonStr(original) + `}]}`
	h2, _ := dashHandler(t, up.URL, dash.Options{})
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv2.URL+"/openai/v1/chat/completions", "application/json",
			strings.NewReader(plain))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if n := strings.Count(forwarded[len(forwarded)-1], probe); n >= perCopy {
		t.Fatalf("the control arm sent %d probe hits, so this content was never compacted and the "+
			"duplication measured above is not attributable to the expand path", n)
	}
}

// AND THE POINTER MUST NOT BE WRITTEN WHEN THERE IS NOTHING TO POINT AT.
//
// The agent's own compaction can drop the message the content came from while keeping the expand
// round-trip. Then the tool_result is the model's ONLY copy, and replacing it with a note would lose
// content the repair used to deliver. The presence check is what makes the note safe, so it has its
// own test: same request WITHOUT the original message.
func TestTheRepairStillCarriesTheContentWhenItIsNotInTheTranscript(t *testing.T) {
	original := strings.Repeat("the original tool output line that had to come back\n", 200)
	const probe = "the original tool output line that had to come back"

	var forwarded []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	h, _ := dashHandler(t, up.URL, dash.Options{})
	h.store.Put("HASH", []byte(original))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// No message holding the original — only the expand call and the client's error.
	body := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{` +
		`"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"Error: No such tool available: context_guru_expand"}]}`
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(forwarded) == 0 {
		t.Fatal("no upstream request")
	}
	if !strings.Contains(forwarded[0], probe) {
		t.Fatalf("the content is nowhere in the request: the repair replaced the model's only copy "+
			"with a pointer to something that is not there:\n%s", forwarded[0])
	}
}

// jsonStr quotes a string for embedding in a JSON literal.
func jsonStr(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `"`, `\"`), "\n", `\n`) + `"`
}
