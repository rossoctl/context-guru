package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dash"
)

// TestDashboardRecordsExpands: the expand loop recorded restorations only to the
// aggregator, so dash.Event.Expands/ExpandTokens were permanently 0 — the "Restorations"
// tile read zero while /stats showed the true count, and Overview.SavedAdjusted
// (SavedUnique − ExpandTokens) OVER-REPORTED net savings by the whole bounce.
func TestDashboardRecordsExpands(t *testing.T) {
	var calls atomic.Int64 // an atomic carries the happens-before edge the round trip does not
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// The model asks for the offloaded original back.
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
				`]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer up.Close()

	h, rec := dashHandler(t, up.URL, dash.Options{})
	h.store.Put("HASH", []byte(strings.Repeat("the original content that had to come back\n", 20)))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body := `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"a","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"},` +
		`{"role":"tool","tool_call_id":"b","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"}]}`
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected the expand continuation (2 upstream calls), got %d", n)
	}
	waitForRows(t, rec, 1)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("captured %d rows; want 1", page.Total)
	}
	e := page.Requests[0]
	if e.Expands != 1 {
		t.Errorf("row expands = %d; want 1", e.Expands)
	}
	if e.ExpandTokens <= 0 {
		t.Errorf("row expand_tokens = %d; want the restored content's tokens", e.ExpandTokens)
	}

	o, err := rec.DB().Overview(dash.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Expands != 1 || o.ExpandTokens <= 0 {
		t.Fatalf("overview expands=%d expand_tokens=%d", o.Expands, o.ExpandTokens)
	}
	if o.ExpandRate <= 0 {
		t.Errorf("expand_rate = %v; want > 0", o.ExpandRate)
	}
	if o.SavedUnique <= 0 {
		t.Fatalf("nothing was compacted (saved_unique=%d); the assertion below would be vacuous", o.SavedUnique)
	}
	if o.SavedAdjusted != o.SavedUnique-o.ExpandTokens {
		t.Errorf("saved_adjusted = %d; want saved_unique(%d) − expand_tokens(%d)",
			o.SavedAdjusted, o.SavedUnique, o.ExpandTokens)
	}
	if o.SavedAdjusted >= o.SavedUnique {
		t.Errorf("a restoration must REDUCE net savings: adjusted=%d unique=%d",
			o.SavedAdjusted, o.SavedUnique)
	}
}

// F2: the client keeps its own copy of `No such tool available`, so the repair runs again on
// every later turn that re-sends the transcript. Charging the restored tokens each time drives
// SavedAdjusted (SavedUnique − ExpandTokens) arbitrarily negative — and the units do not even
// match, because SavedUnique is counted once per distinct content and this would be counted
// once per turn. The recovery is charged the FIRST time and not again.
func TestTheRepairChargesTheRestoredTokensOnce(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	h, rec := dashHandler(t, up.URL, dash.Options{})
	h.store.Put("HASH", []byte(strings.Repeat("the original content that had to come back\n", 20)))
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	// The turn a client produces after receiving our own tool_use: its error, answered.
	body := `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{` +
		`"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"Error: No such tool available: context_guru_expand"}]}`
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	waitForRows(t, rec, 3)

	o, err := rec.DB().Overview(dash.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Expands != 1 {
		t.Fatalf("three turns repairing ONE stale error is one recovery, got expands=%d", o.Expands)
	}
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	charged := 0
	for _, e := range page.Requests {
		if e.ExpandTokens > 0 {
			charged++
		}
	}
	if charged != 1 {
		t.Fatalf("%d of 3 rows carry expand_tokens; only the turn the original first came "+
			"back may be charged", charged)
	}
}

// The other half of F2, and the one that decides whether the gate is a fix or a mute: a
// legitimate SECOND recovery must still be charged. The suppression keys on `contentKey`,
// the same key SavedUnique dedups by — which is what puts the two operands of
// SavedUnique − ExpandTokens in one unit — so it must suppress a re-repair of the same
// CONTENT and nothing else. Keying it on the tool-call id instead would look identical on
// the test above and silently stop charging every distinct original after the first.
//
// Three ids, two distinct originals: A and C share content, B differs. Three recoveries
// happen; two are charged.
func TestADistinctOriginalIsStillCharged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	h, rec := dashHandler(t, up.URL, dash.Options{})
	shared := strings.Repeat("the original A and C both point at\n", 20)
	other := strings.Repeat("a different original entirely, B's\n", 20)
	h.store.Put("HASH_A", []byte(shared))
	h.store.Put("HASH_B", []byte(other))
	h.store.Put("HASH_C", []byte(shared)) // same CONTENT as A, different id
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	turn := func(call, hash string) string {
		return `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","tool_calls":[{"id":"` + call + `","type":"function","function":{` +
			`"name":"context_guru_expand","arguments":"{\"id\":\"` + hash + `\"}"}}]},` +
			`{"role":"tool","tool_call_id":"` + call + `","content":"Error: No such tool available: context_guru_expand"}]}`
	}
	for _, tc := range []struct{ call, hash string }{
		{"call_a", "HASH_A"}, {"call_b", "HASH_B"}, {"call_c", "HASH_C"},
	} {
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json",
			strings.NewReader(turn(tc.call, tc.hash)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	waitForRows(t, rec, 3)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	charged := 0
	for _, e := range page.Requests {
		if e.ExpandTokens > 0 {
			charged++
		}
	}
	// A is charged, B is a distinct original and is charged, C repeats A's content and is not.
	if charged != 2 {
		t.Fatalf("%d of 3 rows carry expand_tokens, want 2: A and B are distinct originals "+
			"and must both be charged; only C, which repeats A's content, may be suppressed. "+
			"Suppressing on the call id rather than the content would give 1 here and still "+
			"pass TestTheRepairChargesTheRestoredTokensOnce", charged)
	}
}

// ⭐ #404 point 7, through the real proxy. An expand of a marker the SWEEP made is charged to the sweep on
// the row (sweep_expands, sweep_expand_tokens, sweep_false_expands), in addition to the totals; an expand
// of any other marker is not. Both host call sites feed it (the in-band loop here, and the request-path
// repair), so the first test drives the loop and the second the repair.
func TestAnExpandOfASweepMarkerIsChargedToTheSweepOnTheDashboardRow(t *testing.T) {
	offload.ResetSweepLedgerForTest()
	t.Cleanup(offload.ResetSweepLedgerForTest)
	for _, tc := range []struct {
		name   string
		sweeps bool
	}{{"a sweep marker", true}, {"another component's marker", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
						`{"id":"call_1","type":"function","function":{"name":"context_guru_expand","arguments":"{\"id\":\"HASH\"}"}}` +
						`]},"finish_reason":"tool_calls"}]}`))
					return
				}
				w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],` +
					`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			}))
			defer up.Close()
			h, rec := dashHandler(t, up.URL, dash.Options{})
			original := strings.Repeat("the original content the sweep dropped\n", 20)
			h.store.Put("HASH", []byte(original))
			if tc.sweeps {
				offload.SeedSweepDropForTest(h.store, "sess-sweep", original, "HASH")
			}
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()
			body := `{"model":"gpt-x","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[` +
				`{"role":"user","content":"go"},` +
				`{"role":"tool","tool_call_id":"a","content":"` + strings.Repeat("a verbose repeated tool output line\\n", 60) + `"}]}`
			resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if n := calls.Load(); n != 2 {
				t.Fatalf("expected the expand continuation (2 upstream calls), got %d", n)
			}
			waitForRows(t, rec, 1)
			page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			e := page.Requests[0]
			if e.Expands != 1 || e.ExpandTokens <= 0 {
				t.Fatalf("the totals lost the expand: %d expands, %d tokens", e.Expands, e.ExpandTokens)
			}
			if tc.sweeps {
				if e.SweepExpands != 1 || e.SweepExpandTokens != e.ExpandTokens || e.SweepFalseExpands != 1 {
					t.Errorf("sweep columns = %d expands, %d tokens, %d false; want 1, %d, 1",
						e.SweepExpands, e.SweepExpandTokens, e.SweepFalseExpands, e.ExpandTokens)
				}
			} else if e.SweepExpands != 0 || e.SweepExpandTokens != 0 || e.SweepFalseExpands != 0 {
				t.Errorf("an expand of another marker was charged to the sweep: %d, %d, %d",
					e.SweepExpands, e.SweepExpandTokens, e.SweepFalseExpands)
			}
			// The headline still subtracts EVERY expand, as before: the split changes nothing there.
			o, err := rec.DB().Overview(dash.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if o.ExpandTokens != int64(e.ExpandTokens) {
				t.Errorf("overview expand_tokens = %d, want the row's %d", o.ExpandTokens, e.ExpandTokens)
			}
		})
	}
}

// The request-path REPAIR is the second host call site. The client answered our expand tool_use with an
// error, so the transcript carries the stale error on every later turn and the repair puts the original
// back each time. The expand is ONE false drop, not one per turn: only the first turn charges the sweep.
func TestTheRepairPathChargesASweepMarkerOnceNotOnEveryTurn(t *testing.T) {
	offload.ResetSweepLedgerForTest()
	t.Cleanup(offload.ResetSweepLedgerForTest)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer up.Close()
	h, rec := dashHandler(t, up.URL, dash.Options{})
	original := strings.Repeat("the original tool output line that had to come back\n", 200)
	// The marker id is the content's own hash by construction (expand.Marker(hashKey(original))), and the
	// repair derives it from the restored text, so the fixture must use the real id, not a stand-in.
	id := offload.SweepMarkerID(original)
	h.store.Put(id, []byte(original))
	offload.SeedSweepDropForTest(h.store, "sess-sweep", original, id)
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	body := `{"model":"gpt-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"tool","tool_call_id":"call_0","content":` + jsonStr(original) + `},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{` +
		`"name":"context_guru_expand","arguments":"{\"id\":\"` + id + `\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"Error: No such tool available: context_guru_expand"}]}`
	for i := 0; i < 3; i++ {
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	waitForRows(t, rec, 3)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sweepRows, falseTotal int
	for _, e := range page.Requests {
		if e.SweepExpands > 0 {
			sweepRows++
		}
		falseTotal += e.SweepFalseExpands
	}
	if sweepRows != 1 || falseTotal != 1 {
		t.Errorf("%d rows charged the sweep and %d false expands over 3 turns of one stale error; want 1 and 1",
			sweepRows, falseTotal)
	}
}
