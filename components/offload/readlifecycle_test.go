package offload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// --- fixture: real Read/Edit sequences off this box's captures ---------------------
//
// testdata/read_lifecycle.json is extracted from /tmp/cg-runs/capture-swebench.jsonl,
// capture-swe.jsonl, capture-tb.jsonl and the four bench arms: for every transcript,
// the ordered Read/Edit/Write calls with their real arguments and (6 KB-capped) real
// outputs. Invented sequences would prove nothing here — what decides a classification
// is the actual interleaving of Reads and Edits a coding agent produces, including the
// partial Reads (offset/limit) that make path-only supersession wrong.

type rlCall struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Out  string          `json:"out"`
}

type rlTranscript struct {
	Source string   `json:"source"`
	Calls  []rlCall `json:"calls"`
}

func loadReadFixture(t *testing.T) []rlTranscript {
	t.Helper()
	b, err := os.ReadFile("testdata/read_lifecycle.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []rlTranscript
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("empty fixture")
	}
	return out
}

// callMsgs renders one call as the assistant tool_use + the tool result, the pairing
// schema.ToolCalls reads. Appends to msgs and returns it.
func callMsgs(msgs []bschemas.ChatMessage, tool string, args any, out string) []bschemas.ChatMessage {
	id := fmt.Sprintf("toolu_%d", len(msgs))
	var raw string
	switch a := args.(type) {
	case json.RawMessage:
		raw = string(a)
	case string:
		raw = a
	default:
		b, _ := json.Marshal(a)
		raw = string(b)
	}
	name := tool
	msgs = append(msgs, bschemas.ChatMessage{
		Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{{
			ID:       &id,
			Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: raw},
		}}},
	})
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &id}}
	schema.SetMessageText(&m, out)
	return append(msgs, m)
}

func transcriptMsgs(tr rlTranscript) []bschemas.ChatMessage {
	var msgs []bschemas.ChatMessage
	for _, c := range tr.Calls {
		msgs = callMsgs(msgs, c.Tool, c.Args, c.Out)
	}
	return msgs
}

func rlFor(t *testing.T, yaml string) *ReadLifecycle {
	t.Helper()
	comp, err := newReadLifecycle([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*ReadLifecycle)
}

// rlCtx is a cache-unaware Ctx: every index is fair game, so a test measures the
// CLASSIFIER rather than the tail gate (which has its own tests below).
func rlCtx() *components.Ctx {
	return &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1}
}

// --- 1. the measured split on real transcripts ------------------------------------

// TestSplitOnRealTranscripts is the deliverable: what fraction of REAL Reads on this
// box's traffic are stale, superseded and fresh. headroom's read_lifecycle.py claims
// 67% stale / 12% superseded / 20% fresh of Read BYTES; this prints ours, and asserts
// only the properties that must hold (every Read is classified, the classes partition,
// and both non-fresh classes are actually reachable on real data).
func TestSplitOnRealTranscripts(t *testing.T) {
	rl := rlFor(t, "min_tokens: 1\n")
	var n, tok [3]int
	for _, tr := range loadReadFixture(t) {
		msgs := transcriptMsgs(tr)
		req := &bschemas.BifrostChatRequest{Input: msgs}
		events := rl.fileEvents(req)
		for k, ev := range events {
			if ev.edit {
				continue
			}
			cls := rl.classify(events, k)
			n[cls]++
			tok[cls] += schema.TextTokens(schema.MessageText(req.Input[ev.idx]))
		}
	}
	total, tt := n[0]+n[1]+n[2], tok[0]+tok[1]+tok[2]
	if total == 0 {
		t.Fatal("no Reads classified: the fixture or the pairing is broken")
	}
	for i, name := range []string{"fresh", "superseded", "stale"} {
		t.Logf("%-11s n=%3d (%4.1f%%)  tokens=%7d (%4.1f%%)",
			name, n[i], 100*float64(n[i])/float64(total), tok[i],
			100*float64(tok[i])/float64(max(tt, 1)))
	}
	t.Logf("total reads=%d read tokens=%d", total, tt)
	if n[readStale] == 0 {
		t.Error("stale detection fires on no real transcript")
	}
	// Deliberately NOT an assertion. On this box's corpus superseded is ZERO once the
	// range key is honoured (the only path-level repeat Reads are Terminal-Bench's image
	// Reads, which are excluded, and one partial re-read of a different range worth ~870
	// tokens). That is our measured truth, not a broken detector — TestSupersededRequires-
	// SameRange proves the mechanism works; asserting it here would just invite loosening
	// the range key until the number moved.
	if n[readSuperseded] == 0 {
		t.Log("no superseded Read on this corpus (see docs/components/readlifecycle.md)")
	}
	if n[readFresh] == 0 {
		t.Error("no fresh Read in the corpus: the classifier is over-claiming")
	}
}

// TestOffloadsOnlyStaleAndSuperseded checks the whole component end to end on the real
// fixture: every message it rewrote carries the right marker, and every message it left
// alone is byte-identical to the original.
func TestOffloadsOnlyStaleAndSuperseded(t *testing.T) {
	rl := rlFor(t, "min_tokens: 20\n")
	for _, tr := range loadReadFixture(t) {
		before := transcriptMsgs(tr)
		req := &bschemas.BifrostChatRequest{Input: schema.CloneMessages(before)}
		c := rlCtx()
		events := rl.fileEvents(req)
		want := map[int]readClass{}
		for k, ev := range events {
			if !ev.edit {
				want[ev.idx] = rl.classify(events, k)
			}
		}
		var rep components.Report
		if _, err := rl.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		for i := range req.Input {
			got, orig := schema.MessageText(req.Input[i]), schema.MessageText(before[i])
			if got == orig {
				continue
			}
			cls, isRead := want[i]
			if !isRead {
				t.Fatalf("%s: rewrote message %d, which is not a Read result", tr.Source, i)
			}
			if cls == readFresh {
				t.Fatalf("%s: rewrote a FRESH Read at %d", tr.Source, i)
			}
			marker := "stale file read"
			if cls == readSuperseded {
				marker = "superseded file read"
			}
			if !strings.Contains(got, marker) {
				t.Fatalf("%s: message %d class %v got wrong marker: %q", tr.Source, i, cls, got)
			}
			if !expand.HasPlaceholder(got) {
				t.Fatalf("%s: message %d has no resolvable marker", tr.Source, i)
			}
		}
	}
}

// --- 2. the safety property: a fresh Read is never touched ------------------------

func TestFreshReadUntouched(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	msgs := callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body)
	msgs = callMsgs(msgs, "Read", map[string]string{"file_path": "/x/OTHER.go"}, body)
	msgs = callMsgs(msgs, "Edit", map[string]string{"file_path": "/x/OTHER.go"}, "ok")
	req := &bschemas.BifrostChatRequest{Input: msgs}
	var rep components.Report
	if _, err := rlFor(t, "min_tokens: 5\n").Offload(req, &rep, rlCtx()); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[1]); got != body {
		t.Fatalf("a fresh Read was rewritten:\n%q", got)
	}
	if got := schema.MessageText(req.Input[3]); got == body {
		t.Fatal("the Read of the later-edited file was NOT offloaded")
	}
	if rep.Gates["fresh_read"] != 1 {
		t.Fatalf("fresh_read gate = %d, want 1 (gates: %v)", rep.Gates["fresh_read"], rep.Gates)
	}
}

// A later Read of a DIFFERENT range does not supersede an earlier one: it replaces no
// part of it, and claiming otherwise deletes content nothing else carries.
func TestSupersededRequiresSameRange(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	for _, tc := range []struct {
		name       string
		second     string
		superseded bool
	}{
		{"same whole-file read", `{"file_path":"/x/a.go"}`, true},
		{"same explicit range", `{"file_path":"/x/a.go","offset":10,"limit":20}`, false},
		{"different range", `{"file_path":"/x/a.go","offset":900}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := callMsgs(nil, "Read", `{"file_path":"/x/a.go"}`, body)
			msgs = callMsgs(msgs, "Read", tc.second, body)
			req := &bschemas.BifrostChatRequest{Input: msgs}
			var rep components.Report
			if _, err := rlFor(t, "min_tokens: 5\n").Offload(req, &rep, rlCtx()); err != nil {
				t.Fatal(err)
			}
			acted := schema.MessageText(req.Input[1]) != body
			if acted != tc.superseded {
				t.Fatalf("superseded=%v, want %v", acted, tc.superseded)
			}
		})
	}
}

// An image Read (Terminal-Bench reads PNGs: 23 of 23 Reads in capture-tb.jsonl) has no
// text content, and a text rewrite would DROP the image. schema.Rewritable is the guard.
func TestImageReadNeverRewritten(t *testing.T) {
	msgs := callMsgs(nil, "Read", map[string]string{"file_path": "/x/board.png"}, "")
	img := "iVBORw0KGgo" + strings.Repeat("A", 500)
	msgs[1].ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: msgs[1].ChatToolMessage.ToolCallID}
	msgs[1].Content = &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{{
		Type:           bschemas.ChatContentBlockTypeImage,
		ImageURLStruct: &bschemas.ChatInputImage{URL: "data:image/png;base64," + img},
	}}}
	msgs = callMsgs(msgs, "Edit", map[string]string{"file_path": "/x/board.png"}, "ok")
	req := &bschemas.BifrostChatRequest{Input: msgs}
	var rep components.Report
	if _, err := rlFor(t, "min_tokens: 1\n").Offload(req, &rep, rlCtx()); err != nil {
		t.Fatal(err)
	}
	if req.Input[1].Content.ContentBlocks == nil {
		t.Fatal("the image Read's blocks were destroyed by a text rewrite")
	}
	if rep.Gates["non_text_blocks"] == 0 {
		t.Fatalf("expected the non_text_blocks gate, got %v", rep.Gates)
	}
}

// --- 3. Bash edit heuristics: narrow, and off by default --------------------------

func TestBashEditsOffByDefault(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	build := func() []bschemas.ChatMessage {
		m := callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body)
		return callMsgs(m, "Bash", map[string]string{"command": "sed -i s/a/b/ /x/a.go"}, "")
	}
	for _, tc := range []struct {
		yaml  string
		acted bool
	}{
		{"min_tokens: 5\n", false},
		{"min_tokens: 5\nbash_edits: true\n", true},
	} {
		req := &bschemas.BifrostChatRequest{Input: build()}
		var rep components.Report
		if _, err := rlFor(t, tc.yaml).Offload(req, &rep, rlCtx()); err != nil {
			t.Fatal(err)
		}
		if acted := schema.MessageText(req.Input[1]) != body; acted != tc.acted {
			t.Fatalf("%q: acted=%v want %v", tc.yaml, acted, tc.acted)
		}
	}
}

// The whole heuristic in one table. A form that is not here must NOT be read as an edit:
// a false positive deletes correct context, which is the failure this component cannot have.
func TestBashEditPathsIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want string
	}{
		{"echo hi > /x/a.go", "/x/a.go"},
		{"cat b >> /x/a.go", "/x/a.go"},
		{"sed -i s/a/b/ /x/a.go", "/x/a.go"},
		{"sed -i.bak s/a/b/ /x/a.go", "/x/a.go"},
		{"python gen.py | tee /x/a.go", "/x/a.go"},
		{"patch -p1 /x/a.go < d.diff", "/x/a.go"},
		{"truncate -s 0 /x/a.go", "/x/a.go"},
		{"echo hi > '/x/a b.go'", "/x/a b.go"},
		// NOT edits: nothing in the text names the file that changes.
		{"cat /x/a.go", ""},
		{"grep -rn foo /x/a.go", ""},
		{"git apply d.diff", ""},
		{"python build.py", ""},
		{"make -C /x", ""},
		{"go test ./... 2>&1 | head -50", ""},
		{"ls /x > /dev/null", ""},
	} {
		got := bashEditPaths(tc.cmd)
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%q read as writing %v — a false positive deletes correct context", tc.cmd, got)
			}
			continue
		}
		found := false
		for _, p := range got {
			if p == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: got %v, want it to include %q", tc.cmd, got, tc.want)
		}
	}
}

// --- 4. cache stability ----------------------------------------------------------

// TestFrozenPrefixIsByteStable is headroom's cross_turn_dedup invariant 1 as a test:
// process blocks in order, match only against strictly earlier ones, and appending a
// turn must never mutate the bytes of any earlier turn. Run on the real fixture, with
// the cache-tail gate ON — the shape production runs in.
func TestFrozenPrefixIsByteStable(t *testing.T) {
	for _, tr := range loadReadFixture(t) {
		if len(tr.Calls) < 4 {
			continue
		}
		rl := rlFor(t, "min_tokens: 20\n")
		st := store.NewMemory(store.Options{})
		full := transcriptMsgs(tr)
		cut := len(full) - 4 // two calls' worth of messages held back
		run := func(msgs []bschemas.ChatMessage, maxCached int) []bschemas.ChatMessage {
			req := &bschemas.BifrostChatRequest{Input: schema.CloneMessages(msgs)}
			c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
			var rep components.Report
			if _, err := rl.Offload(req, &rep, c); err != nil {
				t.Fatal(err)
			}
			return req.Input
		}
		// Turn 1: nothing cached yet, so every decision this component will ever make on
		// the prefix is made here. Turn 2 appends and must reproduce turn 1's bytes.
		a := run(full[:cut], -1)
		b := run(full, cut-1)
		for i := range a {
			if x, y := mustJSONMsg(t, a[i]), mustJSONMsg(t, b[i]); x != y {
				t.Fatalf("%s: appending a turn changed already-sent message %d:\n%s\n%s",
					tr.Source, i, x, y)
			}
		}
	}
}

// A new decision is never made inside the already-cached prefix: that is the flip
// (full → offloaded) that forces a cache-write of the whole suffix.
func TestNewDecisionStaysInTail(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	msgs := callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body)
	msgs = callMsgs(msgs, "Edit", map[string]string{"file_path": "/x/a.go"}, "ok")
	req := &bschemas.BifrostChatRequest{Input: msgs}
	c := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{}),
		CacheAware: true, MaxCachedIdx: 3}
	var rep components.Report
	if _, err := rlFor(t, "min_tokens: 5\n").Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(req.Input[1]) != body {
		t.Fatal("offloaded inside the cached prefix")
	}
	if rep.Gates["cached_prefix"] == 0 {
		t.Fatalf("expected the cached_prefix gate, got %v", rep.Gates)
	}
	// cold_cache lifts the depth restriction on a provably-expired cache, and only then.
	c.ColdCache = true
	req = &bschemas.BifrostChatRequest{Input: callMsgs(
		callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body),
		"Edit", map[string]string{"file_path": "/x/a.go"}, "ok")}
	if _, err := rlFor(t, "min_tokens: 5\ncold_cache: true\n").Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(req.Input[1]) == body {
		t.Fatal("cold_cache did not lift the depth restriction")
	}
}

// A frozen decision replays on EVERY later turn, at any depth. Without this the message
// flips offloaded → full → offloaded as the tail boundary moves past it, which is the
// churn the freeze exists to prevent.
func TestFrozenDecisionReplaysForever(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	rl := rlFor(t, "min_tokens: 5\n")
	st := store.NewMemory(store.Options{})
	var first string
	for turn := 0; turn < 50; turn++ {
		msgs := callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body)
		msgs = callMsgs(msgs, "Edit", map[string]string{"file_path": "/x/a.go"}, "ok")
		for i := 0; i < turn; i++ { // the session grows; the Read sinks deeper into the prefix
			msgs = callMsgs(msgs, "Bash", map[string]string{"command": "go build ./..."}, "ok")
		}
		maxCached := len(msgs) - 3
		if turn == 0 {
			maxCached = -1
		}
		req := &bschemas.BifrostChatRequest{Input: msgs}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: maxCached}
		var rep components.Report
		if _, err := rl.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		got := schema.MessageText(req.Input[1])
		if turn == 0 {
			if got == body {
				t.Fatal("turn 0 must offload the stale Read (it is in the tail)")
			}
			first = got
		}
		if got != first {
			t.Fatalf("turn %d flipped representation:\n want %q\n got  %q", turn, first, got)
		}
	}
}

// TestDeterministicAcrossProcesses re-runs the transform in a CHILD PROCESS and compares
// a hash of the whole rewritten transcript. Go randomizes map iteration per process, so
// a component that let `schema.ToolCalls`'s map decide any ordering would render the
// prefix differently on each restart and re-anchor the provider's cache every turn.
func TestDeterministicAcrossProcesses(t *testing.T) {
	if os.Getenv("CG_RL_CHILD") == "1" {
		fmt.Println("HASH " + rlFixtureHash(t))
		return
	}
	want := rlFixtureHash(t)
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestDeterministicAcrossProcesses")
		cmd.Env = append(os.Environ(), "CG_RL_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		got := ""
		for _, ln := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(ln, "HASH ") {
				got = strings.TrimPrefix(ln, "HASH ")
			}
		}
		if got != want {
			t.Fatalf("child process %d rendered different bytes: %s != %s", i, got, want)
		}
	}
}

// rlFixtureHash runs the component over every fixture transcript and hashes the result.
func rlFixtureHash(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	for _, tr := range loadReadFixture(t) {
		req := &bschemas.BifrostChatRequest{Input: transcriptMsgs(tr)}
		var rep components.Report
		if _, err := rlFor(t, "min_tokens: 20\n").Offload(req, &rep, rlCtx()); err != nil {
			t.Fatal(err)
		}
		for i := range req.Input {
			fmt.Fprintf(h, "%d\x00%s\x00", i, schema.MessageText(req.Input[i]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- 5. recovery -----------------------------------------------------------------

// The offload must be reversible: the marker left in the request resolves, through the
// same store the context_guru_expand tool reads, to the exact original Read output.
func TestExpandRestoresOffloadedRead(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	msgs := callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body)
	msgs = callMsgs(msgs, "Edit", map[string]string{"file_path": "/x/a.go"}, "ok")
	req := &bschemas.BifrostChatRequest{Input: msgs}
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Session: "s", Store: st, MaxCachedIdx: -1}
	var rep components.Report
	keys, err := rlFor(t, "min_tokens: 5\n").Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("want one stash key, got %v", keys)
	}
	marks := expand.ParseMarkers(schema.MessageText(req.Input[1]))
	if len(marks) != 1 || marks[0] != keys[0] {
		t.Fatalf("marker %v does not match the stash key %v", marks, keys)
	}
	got, ok := expand.Resolve(st, marks[0])
	if !ok || got != body {
		t.Fatalf("expand did not restore the original (ok=%v, %d bytes vs %d)", ok, len(got), len(body))
	}
	if !OwnsKey(st, "s", keys[0]) {
		t.Fatal("the stash is not scoped to the session that made it")
	}
}

func mustJSONMsg(t *testing.T, m bschemas.ChatMessage) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stale_at_depth lifts the tail gate for the STALE class only — superseded Reads stay
// gated, because only the stale case carries the correctness argument that might justify
// paying a re-anchor. Measured net-negative on real traffic (see the doc), so this test
// pins the behaviour of a knob that ships off rather than endorsing it.
func TestStaleAtDepthLiftsGateForStaleOnly(t *testing.T) {
	body := strings.Repeat("    12\tsome real source line\n", 60)
	for _, tc := range []struct {
		name   string
		second func([]bschemas.ChatMessage) []bschemas.ChatMessage
		acted  bool
	}{
		{"stale", func(m []bschemas.ChatMessage) []bschemas.ChatMessage {
			return callMsgs(m, "Edit", map[string]string{"file_path": "/x/a.go"}, "ok")
		}, true},
		{"superseded", func(m []bschemas.ChatMessage) []bschemas.ChatMessage {
			return callMsgs(m, "Read", map[string]string{"file_path": "/x/a.go"}, body)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := tc.second(callMsgs(nil, "Read", map[string]string{"file_path": "/x/a.go"}, body))
			req := &bschemas.BifrostChatRequest{Input: msgs}
			c := &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{}),
				CacheAware: true, MaxCachedIdx: len(msgs) - 1} // everything already cached
			var rep components.Report
			rl := rlFor(t, "min_tokens: 5\nstale_at_depth: true\n")
			if _, err := rl.Offload(req, &rep, c); err != nil {
				t.Fatal(err)
			}
			if acted := schema.MessageText(req.Input[1]) != body; acted != tc.acted {
				t.Fatalf("acted at depth = %v, want %v (gates %v)", acted, tc.acted, rep.Gates)
			}
		})
	}
}
