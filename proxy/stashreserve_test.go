package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
)

// The rewind reserve, end to end through the handler (#187).
//
// Two things are asserted on RENDERED output rather than on any in-process value, because both
// have a handler between the value and the reader and that is where this class of bug lives:
//
//   - the body that reaches the UPSTREAM must not carry a <<cg:HASH>> marker whose payload the
//     store cannot serve. The model reads that body; a component's return value is not what the
//     model reads, and #187 was precisely a marker on the wire with nothing behind it.
//   - /stats must actually publish the exhaustion. `stash_refused` is filled by the /stats
//     handler after the aggregator snapshot is taken, so a correctly-counted refusal can be
//     dropped on the way out — the same shape as the frozen_* family, which exported a
//     permanent 0 for exactly that reason.
func TestAnExhaustedRewindReserveNeitherStampsUnbackedMarkersNorHidesItself(t *testing.T) {
	var sent atomic.Value // the last body the upstream received
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	// max_entries 4 => a 2-payload reserve, against five large tool outputs. Every component
	// in the pipeline is an Offload whose marker_mode defaults to full, so the sixth removal
	// onwards has nowhere to put its original.
	cfg, err := config.LoadBytes([]byte("pipeline: [linecap]\ncomponents:\n  linecap: {max_line_chars: 40, min_size: 10}\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{MaxEntries: 4})
	h := proxy.New(pipe, st, agg, proxy.Options{OpenAIUpstream: upstream.URL, AnthropicUpstream: upstream.URL})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	msgs := []map[string]any{{"role": "user", "content": "go"}}
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		msgs = append(msgs, map[string]any{
			"role": "tool", "tool_call_id": tag,
			"content": "line " + tag + ": " + strings.Repeat(tag+"q", 900),
		})
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-x", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	wire, _ := sent.Load().(string)
	if wire == "" {
		t.Fatal("the upstream received nothing, so there is no rendered request to assert on")
	}
	// Both spellings: encoding/json HTML-escapes "<", and every marker is written after a
	// newline, so on the wire a marker usually exists only as \u003c\u003ccg:HASH\u003e\u003e.
	markerRe := regexp.MustCompile(`(?:<|(?i:\\u003c)){2}cg:([A-Za-z0-9_-]{1,64})(?:>|(?i:\\u003e)){2}`)
	found := markerRe.FindAllStringSubmatch(wire, -1)
	if len(found) == 0 {
		t.Fatal("the request that reached the upstream carries no markers at all, so it cannot " +
			"show whether an unbacked one got through — the fixture is not offloading")
	}
	for _, m := range found {
		if _, ok := st.Get(m[1]); !ok {
			t.Errorf("the body sent UPSTREAM offers the model <<cg:%s>> and the store cannot "+
				"produce it: a removal advertised as reversible is not. That is #187 on the wire", m[1])
		}
	}
	if len(found) > 2 {
		t.Errorf("%d markers reached the upstream against a 2-payload reserve: a removal whose "+
			"payload cannot be stored must be refused, not stamped anyway", len(found))
	}

	sresp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	sbody, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	var snap struct {
		StashRefused  int64 `json:"stash_refused"`
		StashLive     int   `json:"stash_live"`
		StashCapacity int   `json:"stash_capacity"`
	}
	if err := json.Unmarshal(sbody, &snap); err != nil {
		t.Fatalf("/stats did not parse: %v\n%s", err, sbody)
	}
	if snap.StashCapacity != 2 {
		t.Errorf("/stats stash_capacity = %d, want 2 (max_entries 4): the handler is not "+
			"publishing the store's reserve, so nobody can see how close it is to binding",
			snap.StashCapacity)
	}
	if snap.StashLive != 2 {
		t.Errorf("/stats stash_live = %d, want 2", snap.StashLive)
	}
	if snap.StashRefused == 0 {
		t.Error("/stats stash_refused = 0 while removals were being declined for want of a " +
			"payload slot. This is the LEADING indicator for expand_unresolved_missing, which " +
			"cannot move until the agent happens to call expand — so with this at 0 an " +
			"exhausted reserve is invisible until an agent is already confused")
	}
	if !strings.Contains(string(sbody), `"stash_refused"`) {
		t.Error(`/stats does not render a "stash_refused" key at all`)
	}
}
