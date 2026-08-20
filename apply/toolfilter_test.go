package apply

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// bodyFixture is a miniature of real Claude Code traffic: a catalogue where some tools are
// described in the system prompt's prose and some are not, an environment snapshot on the
// end of the system block, and a transcript that already called one of the removable tools.
const bodyFixture = `{"model":"claude-opus-5",` +
	`"tools":[` +
	`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}},` +
	`{"name":"Read","description":"read a file","input_schema":{"type":"object"}},` +
	`{"name":"Workflow","description":"a long unused thing","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}},` +
	`{"name":"CronCreate","description":"schedule","input_schema":{"type":"object"}},` +
	`{"name":"mcp__ctx7__resolve","description":"resolve","input_schema":{"type":"object"}},` +
	`{"name":"mcp__ctx7__query","description":"query","input_schema":{"type":"object"}}],` +
	`"system":[{"type":"text","text":"You are an agent.\nPrefer dedicated tools over Bash when one fits (Read).\nAlready done.\n\nCurrent branch: main\nRecent commits:\n0898367 add the Workflow tool\n"}],` +
	`"messages":[` +
	`{"role":"user","content":[{"type":"text","text":"hello"}]},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"CronCreate","input":{}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`

func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	gjson.GetBytes(body, "tools").ForEach(func(_, v gjson.Result) bool {
		out = append(out, v.Get("name").String())
		return true
	})
	return out
}

// TestFilterNeedsExplicitOptIn is the rule the whole feature rests on: nothing is removed
// that the account did not name. An empty list must be byte-identical to the input, not
// merely equivalent — a reserialized body re-anchors the cached prefix for a saving of zero.
func TestFilterNeedsExplicitOptIn(t *testing.T) {
	for _, remove := range [][]string{nil, {}, {"NotDeclared"}, {"mcp__other"}} {
		out, tok, n := filterDeclarations([]byte(bodyFixture), remove)
		if string(out) != bodyFixture || tok != 0 || n != 0 {
			t.Errorf("remove=%v changed the body (%d tokens, %d decls)", remove, tok, n)
		}
	}
}

// TestFilterProseGateKeepsDescribedTools is hazard 2: Claude Code's system prompt keeps
// describing its tools in prose, and stripping a declaration while its prose survives
// invites the model to write the call into prose instead of emitting a tool_use. A name
// mentioned in the prose is kept however loudly the configuration asks for it.
func TestFilterProseGateKeepsDescribedTools(t *testing.T) {
	out, tok, n := filterDeclarations([]byte(bodyFixture), []string{"Bash", "Read"})
	if n != 0 || tok != 0 || string(out) != bodyFixture {
		t.Fatalf("prose-described tools were removed: %d decls, %d tokens", n, tok)
	}
	// And the gate is discriminating, not blanket: a name absent from the prose goes.
	out, tok, n = filterDeclarations([]byte(bodyFixture), []string{"CronCreate"})
	if n != 1 || tok <= 0 {
		t.Fatalf("CronCreate not removed: %d decls, %d tokens", n, tok)
	}
	if got := toolNames(t, out); strings.Contains(strings.Join(got, ","), "CronCreate") {
		t.Errorf("CronCreate still declared: %v", got)
	}
}

// TestFilterIgnoresTheVolatileSnapshot is the determinism half of the prose gate. The
// environment snapshot Claude Code appends to its system block changes between turns, so a
// commit subject naming a tool must NOT decide whether that tool is filtered — otherwise the
// filter flips mid-session and re-anchors the prefix on every turn.
func TestFilterIgnoresTheVolatileSnapshot(t *testing.T) {
	// "Workflow" appears ONLY in the fake commit subject inside the snapshot.
	if _, _, n := filterDeclarations([]byte(bodyFixture), []string{"Workflow"}); n != 1 {
		t.Fatalf("a commit subject vetoed a removal: removed %d, want 1", n)
	}
}

// TestFilterRemovesAWholeMCPServer: `mcp__<server>` is the unit a user adds and removes.
func TestFilterRemovesAWholeMCPServer(t *testing.T) {
	out, tok, n := filterDeclarations([]byte(bodyFixture), []string{"mcp__ctx7"})
	if n != 2 || tok <= 0 {
		t.Fatalf("server removal removed %d decls (%d tokens), want 2", n, tok)
	}
	for _, name := range toolNames(t, out) {
		if strings.HasPrefix(name, "mcp__ctx7__") {
			t.Errorf("%s survived a whole-server removal", name)
		}
	}
}

// TestFilterKeepsHistoricalToolUseForwardable is safety rule 1 as a test: a transcript that
// already CALLED a now-undeclared tool must still forward, byte-intact. Verified against
// production first (66 mid-session tool-set shrinks: 64x HTTP 200, 0x 400, 20 bodies
// carrying a tool_use absent from their own tools[]), and pinned here so a future rewrite
// cannot start rewriting the transcript to "match" the catalogue.
func TestFilterKeepsHistoricalToolUseForwardable(t *testing.T) {
	out, _, n := filterDeclarations([]byte(bodyFixture), []string{"CronCreate"})
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	in := gjson.Get(bodyFixture, "messages")
	got := gjson.GetBytes(out, "messages")
	if in.Raw != got.Raw {
		t.Errorf("messages were rewritten:\n%s\n%s", in.Raw, got.Raw)
	}
	if !gjson.ValidBytes(out) {
		t.Fatal("output is not valid JSON")
	}
	if name := gjson.GetBytes(out, "messages.1.content.0.name").String(); name != "CronCreate" {
		t.Errorf("historical tool_use name = %q, want CronCreate", name)
	}
}

// TestFilterSkipsAPendingRemovedCall is safety rule 3, scoped to the remove set. The narrow
// predicate is what keeps rule 3 from fighting the determinism rule: see the call site.
func TestFilterSkipsAPendingRemovedCall(t *testing.T) {
	// Drop the trailing tool_result so the last turn is an unanswered tool_use.
	pending := strings.Replace(bodyFixture,
		`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`, "", 1)
	if _, _, n := filterDeclarations([]byte(pending), []string{"CronCreate"}); n != 0 {
		t.Errorf("filtered a request with a pending call for the removed tool")
	}
	// A pending call for something else does not block an unrelated removal.
	if _, _, n := filterDeclarations([]byte(pending), []string{"Workflow"}); n != 1 {
		t.Errorf("an unrelated pending call blocked the filter")
	}
}

// TestFilterNeverEmptiesTheCatalogue: a body that declares tools and a body that declares
// none are different shapes to the provider, and some reject the empty array outright.
func TestFilterNeverEmptiesTheCatalogue(t *testing.T) {
	const one = `{"tools":[{"name":"Solo","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"x"}]}`
	if out, _, n := filterDeclarations([]byte(one), []string{"Solo"}); n != 0 || string(out) != one {
		t.Errorf("emptied the catalogue: %s", out)
	}
}

// TestFilterKeepsAForcedTool: tool_choice REQUIRES the named tool, so removing its
// declaration would turn a forced call into a 400.
func TestFilterKeepsAForcedTool(t *testing.T) {
	forced := strings.Replace(bodyFixture, `"model":"claude-opus-5",`,
		`"model":"claude-opus-5","tool_choice":{"type":"tool","name":"CronCreate"},`, 1)
	if _, _, n := filterDeclarations([]byte(forced), []string{"CronCreate"}); n != 0 {
		t.Error("removed the tool tool_choice forces")
	}
}

// TestFilterDeclarationsByteStable is the cache-safety test. `tools` renders at position 0
// with no breakpoint on it, so output that is not byte-identical for identical input
// re-anchors the whole prefix on EVERY request — a pure loss. Go map iteration and the
// maphash seed are re-randomized per process, so same-process repetition cannot prove this
// alone and the check re-runs itself in child processes.
func TestFilterDeclarationsByteStable(t *testing.T) {
	remove := []string{"CronCreate", "Workflow", "mcp__ctx7"}
	first, ftok, fn := filterDeclarations([]byte(bodyFixture), remove)
	if fn == 0 {
		t.Fatal("fixture removed nothing; the test would prove nothing")
	}
	for i := 0; i < 500; i++ {
		got, tok, n := filterDeclarations([]byte(bodyFixture), remove)
		if string(got) != string(first) || tok != ftok || n != fn {
			t.Fatalf("iteration %d differs:\n got %s\nwant %s", i, got, first)
		}
	}
	if os.Getenv("CG_TOOLFILTER_CHILD") == "1" {
		out, tok, _ := filterDeclarations([]byte(bodyFixture), remove)
		os.Stdout.WriteString(string(out) + "\n")
		os.Stdout.WriteString(itoa(tok) + "\n")
		return
	}
	for i := 0; i < 5; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestFilterDeclarationsByteStable$", "-test.v=false")
		cmd.Env = append(os.Environ(), "CG_TOOLFILTER_CHILD=1")
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("child run %d: %v", i, err)
		}
		lines := strings.SplitN(string(raw), "\n", 3)
		if len(lines) < 2 {
			t.Fatalf("child run %d printed nothing usable: %q", i, raw)
		}
		if lines[0] != string(first) {
			t.Fatalf("child %d produced different bytes:\n got %s\nwant %s", i, lines[0], first)
		}
		if lines[1] != itoa(ftok) {
			t.Fatalf("child %d counted %s tokens, want %d", i, lines[1], ftok)
		}
	}
}

// itoa keeps the child's output dependency-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestFilterProseWordBoundary: a substring test would keep `Read` alive on the word
// "already", which would refuse every removal on a long enough prompt.
func TestFilterProseWordBoundary(t *testing.T) {
	for _, tc := range []struct {
		prose, name string
		want        bool
	}{
		{"Already done.", "Read", false},
		{"use Read here", "Read", true},
		{"(Read, Edit)", "Read", true},
		{"ReadFile only", "Read", false},
		{"my_Read_thing", "Read", false},
		{"Read", "Read", true},
		{"nothing here", "", true}, // an unknown name must refuse, never guess
	} {
		if got := proseReferenced(tc.prose, tc.name); got != tc.want {
			t.Errorf("proseReferenced(%q, %q) = %v, want %v", tc.prose, tc.name, got, tc.want)
		}
	}
}

// removalDecisions replays one request through the real filter with EVERY declared tool on
// the removal list, and reports the decision the filter reached for each name: true = the
// declaration was removed, false = something kept it. Deriving it from filterDeclarations'
// own output rather than from proseReferenced is the point — this is the decision that lands
// on the wire.
//
// Returns ok=false for a request the filter deliberately passes through whole (a pending call
// for a removed name), which is not a decision about any name. See the call site.
func removalDecisions(body []byte) (dec map[string]bool, ok bool) {
	var declared []string
	gjson.GetBytes(body, "tools").ForEach(func(_, tv gjson.Result) bool {
		if n := declName(tv); n != "" {
			declared = append(declared, n)
		}
		return true
	})
	if len(declared) == 0 {
		return nil, false
	}
	names, servers := removeSets(declared)
	if pendingCallFor(body, names, servers) {
		return nil, false
	}
	out, _, n := filterDeclarations(body, declared)
	if n == 0 {
		return nil, false
	}
	survived := map[string]bool{}
	gjson.GetBytes(out, "tools").ForEach(func(_, tv gjson.Result) bool {
		survived[declName(tv)] = true
		return true
	})
	dec = make(map[string]bool, len(declared))
	for _, nm := range declared {
		dec[nm] = !survived[nm]
	}
	return dec, true
}

// TestFilterProseSetStableOverCapture is the PROOF of the property the whole file is arranged
// around, on real traffic rather than by argument: for a given session, the removal decision
// for a declaration NEVER changes after the first request it appears in.
//
// It used to be a guard for the hazard instead of a proof of the fix. The prose region
// included `messages.0`, which Claude Code's auto-compaction rewrites into a model-written
// conversation summary (measured on bench/long.jsonl: 32,512 -> 53,752 bytes mid-session),
// and `system[0]`, whose billing-header cc_version drifts. proseRegion now excludes both, so
// this asserts stability of the decision itself, per session, over every capture — including
// the multi-session harness captures, where sessions are separated the same way the proxy
// separates them.
//
// Requests the filter passes through whole because of a pending call for a removed name are
// not decisions and are counted, not asserted on: that skip is unreachable in the steady
// state (the model never saw the declaration, so it cannot have called it) but is very much
// reachable when replaying a capture that was never filtered.
//
// Skipped unless CONTEXT_GURU_CAPTURE names a readable capture:
//
//	CONTEXT_GURU_CAPTURE=/home/vpcuser/cg-research/bench/long.jsonl \
//	  go test ./apply -run FilterProseSetStableOverCapture -v
func TestFilterProseSetStableOverCapture(t *testing.T) {
	path := os.Getenv("CONTEXT_GURU_CAPTURE")
	if path == "" {
		t.Skip("set CONTEXT_GURU_CAPTURE to a capture to run this")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	type seen struct {
		first  map[string]bool
		region string
		at     int
	}
	sessions := map[string]*seen{}
	regionLens := map[int]bool{}
	requests, decided, skipped := 0, 0, 0
	for i := 0; sc.Scan(); i++ {
		var rec struct{ Body json.RawMessage }
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Body) == 0 {
			continue
		}
		requests++
		regionLens[len(proseRegion(rec.Body))] = true
		dec, ok := removalDecisions(rec.Body)
		if !ok {
			skipped++
			continue
		}
		decided++
		sid := explicitSession("", rec.Body)
		s := sessions[sid]
		if s == nil {
			sessions[sid] = &seen{first: dec, region: proseRegion(rec.Body), at: i}
			continue
		}
		// The sharper property, and the one the fix actually installs: the region itself no
		// longer moves within a session, so no gate computed from it can flip. Asserted as
		// well as the decision because a region that moved and happened not to change any
		// decision is the exact state this file was in before.
		if r := proseRegion(rec.Body); r != s.region {
			t.Errorf("session %s request %d: the prose region moved (%d -> %d bytes) since "+
				"request %d", sid, i, len(s.region), len(r), s.at)
		}
		for nm, removed := range dec {
			was, known := s.first[nm]
			if !known {
				s.first[nm] = removed // a declaration the client only started sending later
				continue
			}
			if was != removed {
				t.Errorf("session %s request %d: the removal decision for %q flipped "+
					"%v -> %v since request %d, so `tools` re-anchors on this turn",
					sid, i, nm, was, removed, s.at)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if decided == 0 {
		t.Fatalf("no request in %s yielded a removal decision; the test proved nothing", path)
	}
	// The region size moving is the hazard this fix removed the teeth from; log it so the
	// numbers in the file comment stay checkable.
	t.Logf("%d requests, %d decided, %d passed through whole, %d sessions, "+
		"%d distinct prose-region sizes", requests, decided, skipped, len(sessions), len(regionLens))
}

// TestFilterRemovedSetSurvivesCompaction is the same property as a committed regression, on
// the three things measured to move mid-session while the session id does not. No capture on
// this box exhibits the flip — the compaction summaries there happen to name only tools the
// gate already keeps — so the hostile version is synthesized here: a summary that names every
// removable tool by hand, which is exactly what a model writing "I used the CronCreate tool
// to..." produces.
func TestFilterRemovedSetSurvivesCompaction(t *testing.T) {
	remove := []string{"CronCreate", "Workflow", "mcp__ctx7"}
	base, baseTok, baseN := filterDeclarations([]byte(bodyFixture), remove)
	if baseN != 4 {
		t.Fatalf("fixture removed %d declarations, want 4; the test would prove nothing", baseN)
	}
	baseTools := gjson.GetBytes(base, "tools").Raw

	// A compaction summary naming every removable tool, in the first user message.
	const summary = "This session is being continued from a previous conversation. Summary: " +
		"I used the CronCreate tool to schedule the job, then the Workflow tool, then " +
		"mcp__ctx7__resolve and mcp__ctx7__query to look up the docs."
	compacted := strings.Replace(bodyFixture,
		`{"role":"user","content":[{"type":"text","text":"hello"}]}`,
		`{"role":"user","content":[{"type":"text","text":`+jsonQuote(summary)+`}]}`, 1)
	// A billing-header pseudo-system block whose cc_version drifts, as system[0] — and which
	// in the drifted turn happens to contain a tool name, the shape that would veto a removal
	// if the block were scanned as prose.
	withHdr := func(ver string) string {
		return strings.Replace(compacted, `"system":[`,
			`"system":[{"type":"text","text":`+
				jsonQuote("x-anthropic-billing-header: cc_version="+ver+"; cc_entrypoint=claude-vscode;")+`},`, 1)
	}
	for _, tc := range []struct{ name, body string }{
		{"compaction summary names every removable tool", compacted},
		{"summary + billing header", withHdr("2.1.236.c73")},
		{"summary + drifted billing header", withHdr("2.1.236.339")},
		{"summary + billing header naming a tool", withHdr("2.1.236.Workflow")},
	} {
		out, tok, n := filterDeclarations([]byte(tc.body), remove)
		if n != baseN || tok != baseTok {
			t.Errorf("%s: removed %d declarations / %d tokens, want %d / %d — the removal set "+
				"moved mid-session and `tools` re-anchors", tc.name, n, tok, baseN, baseTok)
		}
		if got := gjson.GetBytes(out, "tools").Raw; got != baseTools {
			t.Errorf("%s: `tools` is not byte-identical to the first request's\n got %s\nwant %s",
				tc.name, got, baseTools)
		}
	}
}

// TestFilterProseGateReadsAnOpenAISystemMessage: the OpenAI dialect has no top-level
// `system` — its system prompt is messages[0] with role "system". Excluding `messages.0` for
// determinism must not throw that away, or every OpenAI-dialect request loses the prose gate
// entirely and the filter starts stripping declarations whose prose survives. The role, not
// the position, is what distinguishes it from the user turn a compaction summary replaces.
func TestFilterProseGateReadsAnOpenAISystemMessage(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"Read"}},` +
		`{"type":"function","function":{"name":"CronCreate"}}],`
	const prose = "You are an agent. Prefer dedicated tools over Bash when one fits (Read)."
	sysMsg := `{` + tools + `"messages":[{"role":"system","content":` + jsonQuote(prose) + `},` +
		`{"role":"user","content":"hi"}]}`
	if _, _, n := filterDeclarations([]byte(sysMsg), []string{"Read"}); n != 0 {
		t.Error("stripped a tool named in the OpenAI dialect's system message")
	}
	if _, _, n := filterDeclarations([]byte(sysMsg), []string{"CronCreate"}); n != 1 {
		t.Error("the gate refused a removal it should have allowed")
	}
	// The same text in a USER turn is not consulted: that is the slot a compaction summary
	// lands in.
	userMsg := `{` + tools + `"messages":[{"role":"user","content":` + jsonQuote(prose) + `}]}`
	if _, _, n := filterDeclarations([]byte(userMsg), []string{"Read"}); n != 1 {
		t.Error("a user turn still vetoes a removal, so a compaction summary can flip the set")
	}
}

// jsonQuote keeps the fixtures above readable without pulling encoding/json into the string
// building; it is only ever fed literals from this file.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
