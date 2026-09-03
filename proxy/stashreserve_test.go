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

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/proxy"
	"github.com/rossoctl/context-guru/store"
)

// offloadStashMissing reads the process-wide dangling-replay counter. Wrapped so the assertions
// below name the thing rather than the package path.
func offloadStashMissing() int64 { return offload.StashMissing() }

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
		StashMissing  int64 `json:"stash_missing"`
		StashLive     int   `json:"stash_live"`
		StashCapacity int   `json:"stash_capacity"`
		StashBytes    int64 `json:"stash_bytes"`
		StashMaxBytes int64 `json:"stash_max_bytes"`
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
	// The reserve's OTHER budget. Without both an operator told to "raise max_entries" cannot
	// tell whether entries were the binding constraint at all — a payload is a whole tool output
	// while every other exempt entry is a marker line, so the two budgets bind at very different
	// workloads.
	if snap.StashMaxBytes == 0 {
		t.Error("/stats stash_max_bytes = 0: the byte budget is not published, so an operator " +
			"reading stash_refused cannot tell which of the two budgets bound")
	}
	if snap.StashBytes == 0 {
		t.Error("/stats stash_bytes = 0 although payloads are held: the reserve's real cost is " +
			"invisible")
	}
	if snap.StashBytes > snap.StashMaxBytes {
		t.Errorf("/stats reports %d bytes held against a %d-byte budget", snap.StashBytes, snap.StashMaxBytes)
	}
	// stash_missing is the opposite outcome from stash_refused and must be its own key: a
	// refusal promises that nothing became irreversible, and a dangling marker is exactly the
	// case where something did. Nothing dangled in this fixture, so the assertion is on the
	// key's PRESENCE — a field that renders only when non-zero cannot be alerted on.
	if !strings.Contains(string(sbody), `"stash_missing"`) {
		t.Error(`/stats does not render a "stash_missing" key, so the one reserve outcome that ` +
			`genuinely breaks reversibility is indistinguishable from the safe one`)
	}
	if snap.StashMissing != 0 {
		t.Errorf("/stats stash_missing = %d in a fixture where every refusal left the content "+
			"verbatim; a declined removal is being counted as a dangling marker", snap.StashMissing)
	}
}

// lostPayloadStore is a real store in which ONE rewind payload has gone while the PINNED frozen
// decision naming it survives. That combination is not contrived: cg:frz: is exempt from LRU
// eviction and the payload is not — which is #187 — and the TTL reaches an idle payload before an
// actively replayed decision. It is built as a wrapper because store.Memory has no delete.
type lostPayloadStore struct {
	*store.Memory
	lost atomic.Value // string: the marker key whose payload is gone
}

func (s *lostPayloadStore) hidden(key string) bool {
	k, _ := s.lost.Load().(string)
	return k != "" && k == key
}

func (s *lostPayloadStore) Get(key string) ([]byte, bool) {
	if s.hidden(key) {
		return nil, false
	}
	return s.Memory.Get(key)
}

// PutStash refuses the lost key specifically. A refresh of a payload that is PRESENT still
// succeeds, exactly as the real reserve behaves — the point is a payload that is absent and
// cannot be put back, which is what makes the replayed marker dangling rather than merely tight.
func (s *lostPayloadStore) PutStash(key string, payload []byte) bool {
	if s.hidden(key) {
		return false
	}
	return s.Memory.PutStash(key, payload)
}

// /stats must publish stash_missing from the live counter, and the VALUE has to move.
//
// The key alone is not enough, and a test asserting only its presence passed with the wiring
// deleted: metrics.Snapshot renders the field at 0 either way. Snapshot.StashMissing is filled by
// the /stats handler AFTER the aggregator snapshot is taken — the same shape that made the
// frozen_* family export a permanent 0 — so a dropped assignment here leaves the one counter that
// reports a BROKEN reversibility promise reading zero forever, on the page an operator checks to
// confirm nothing broke.
func TestStatsPublishesDanglingReplaysFromTheLiveCounter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	// mask, because it FREEZES its decision and replays it on every later turn — the replay is
	// the path on which a payload can be found missing.
	cfg, err := config.LoadBytes([]byte(
		"pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 0, min_tokens: 20}\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	st := &lostPayloadStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	h := proxy.New(pipe, st, agg, proxy.Options{OpenAIUpstream: upstream.URL, AnthropicUpstream: upstream.URL})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	body, err := json.Marshal(map[string]any{"model": "gpt-x", "messages": []map[string]any{
		{"role": "user", "content": "go"},
		{"role": "tool", "tool_call_id": "a",
			"content": "line a: " + strings.Repeat("aq", 900)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	post := func() {
		resp, err := http.Post(srv.URL+"/openai/v1/chat/completions",
			"application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Turn 1: mask offloads, freezes the decision and stashes the original.
	post()
	var stashed string
	for _, k := range st.Memory.StashedKeysForTest() {
		stashed = k
	}
	if stashed == "" {
		t.Fatal("turn 1 stashed no payload, so there is no marker whose payload can go missing")
	}

	// The payload is now gone; the pinned frozen decision naming it is not.
	st.lost.Store(stashed)
	before := offloadStashMissing()
	post() // turn 2 replays the frozen decision and finds nothing behind its marker
	live := offloadStashMissing()
	if live == before {
		t.Fatalf("no dangling replay was recorded (still %d), so this test cannot show whether "+
			"/stats publishes them", live)
	}

	sresp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	sbody, _ := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	var snap struct {
		StashMissing int64 `json:"stash_missing"`
		StashRefused int64 `json:"stash_refused"`
	}
	if err := json.Unmarshal(sbody, &snap); err != nil {
		t.Fatalf("/stats did not parse: %v\n%s", err, sbody)
	}
	if snap.StashMissing != live {
		t.Errorf("/stats stash_missing = %d while offload.StashMissing() = %d: the handler is "+
			"not publishing the live counter, so the ONE reserve outcome that breaks a "+
			"reversibility promise reads zero on the page an operator checks",
			snap.StashMissing, live)
	}
}
