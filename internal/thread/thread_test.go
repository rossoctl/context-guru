package thread

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// conv builds an Anthropic-dialect body from a list of message texts. Even indexes are user
// turns, odd ones assistant turns; the LAST message carries the cache breakpoint, which is
// where Claude Code puts it, so it moves on every turn.
func conv(texts ...string) []byte {
	msgs := make([]any, len(texts))
	for i, t := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		block := map[string]any{"type": "text", "text": t}
		if i == len(texts)-1 {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		msgs[i] = map[string]any{"role": role, "content": []any{block}}
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "system": "sys", "messages": msgs})
	return b
}

func testTracker() *Tracker {
	t := New()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	t.now = func() time.Time { at = at.Add(time.Second); return at }
	return t
}

func TestKeyOfThePrimaryThreadIsTheSession(t *testing.T) {
	if got := Key("s1", Primary); got != "s1" {
		t.Fatalf("Key(s1, primary) = %q, want the session itself", got)
	}
	if Key("s1", "a:x") == "s1" {
		t.Fatal("a subagent thread keys like the session")
	}
}

// The fingerprint must not change for the edits an agent makes in place without starting a new
// thread, or the main thread would split into a new thread on every turn.
func TestFingerprintIgnoresWhatAnAgentRewritesInPlace(t *testing.T) {
	base := `{"messages":[
	  {"role":"user","content":[{"type":"text","text":"go"}]},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"S1"},
	    {"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"a b c"}]}]}`
	edited := `{"messages":[
	  {"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral"}}]},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"other","signature":"S2"},
	    {"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"[Old tool result content cleared]"}]}]}`
	a, okA := Fingerprint([]byte(base))
	b, okB := Fingerprint([]byte(edited))
	if !okA || !okB || len(a) != 3 || len(b) != 3 {
		t.Fatalf("fingerprints not computed: %v %v %d %d", okA, okB, len(a), len(b))
	}
	if a[2] != b[2] {
		t.Error("cache_control, thinking or a cleared tool result changed the fingerprint")
	}
	// Positive control: a real change to the conversation DOES change it.
	changed := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"stop"}]}]}`)
	c, _ := Fingerprint(changed)
	if c[0] == a[0] {
		t.Error("a different first message has the same fingerprint")
	}
}

func TestFingerprintHasNoMessageListToMatch(t *testing.T) {
	for name, body := range map[string]string{
		"server-held history": `{"previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`,
		"no messages":         `{"model":"m"}`,
		"not json":            `garbage`,
		"empty list":          `{"messages":[]}`,
	} {
		if _, ok := Fingerprint([]byte(body)); ok {
			t.Errorf("%s: fingerprint reported a message list", name)
		}
	}
}

// Test 2 and 4 of the issue, at the source: a subagent interleaved with the main thread gets its
// own thread, and the main thread keeps the primary id — with the header and without it.
func TestSubagentGetsItsOwnThreadWithAndWithoutTheHeader(t *testing.T) {
	type req struct {
		agent string
		body  []byte
	}
	reqs := []req{
		{"", conv("fix the bug")},
		{"", conv("fix the bug", "spawning", "task result pending")},
		{"agentS", conv("investigate file x")},
		{"agentS", conv("investigate file x", "reading", "contents")},
		{"", conv("fix the bug", "spawning", "task result pending", "waiting", "still waiting")},
		{"agentS", conv("investigate file x", "reading", "contents", "done", "ok")},
		{"", conv("fix the bug", "spawning", "task result pending", "waiting", "still waiting", "x", "y")},
	}
	run := func(useHeader bool) []string {
		tr := testTracker()
		var ids []string
		for _, r := range reqs {
			agent := ""
			if useHeader {
				agent = r.agent
			}
			ids = append(ids, tr.Resolve("t\x00s", agent, r.body, false).ID)
		}
		return ids
	}
	withHeader, without := run(true), run(false)
	for i, r := range reqs {
		if (r.agent == "") != (withHeader[i] == Primary) {
			t.Errorf("header path, request %d (agent %q): thread %q", i, r.agent, withHeader[i])
		}
		if (r.agent == "") != (without[i] == Primary) {
			t.Errorf("fallback path, request %d (agent %q): thread %q", i, r.agent, without[i])
		}
	}
	// The same GROUPING on both paths: every subagent request in one thread, which is not the
	// main thread. The ids differ (a header id against a fingerprint id); the partition must not.
	if partition(withHeader) != partition(without) {
		t.Errorf("grouping differs:\n header:   %v\n fallback: %v", withHeader, without)
	}
}

// partition renders a list of ids as the shape of the grouping, independent of the names.
func partition(ids []string) string {
	names := map[string]int{}
	out := ""
	for _, id := range ids {
		if _, ok := names[id]; !ok {
			names[id] = len(names)
		}
		out += fmt.Sprint(names[id], ",")
	}
	return out
}

// Test 3: a fork copies its parent's full context and then diverges. It must get its own
// thread, and the parent must keep its own, whichever of the two speaks first after the fork.
func TestForkGetsItsOwnThread(t *testing.T) {
	parent := []string{"task", "a1", "u2", "a2", "u3"}
	for _, forkFirst := range []bool{true, false} {
		for _, useHeader := range []bool{true, false} {
			tr := testTracker()
			name := fmt.Sprintf("forkFirst=%v header=%v", forkFirst, useHeader)
			tr.Resolve("t\x00s", "", conv(parent...), false)
			forkAgent := ""
			if useHeader {
				forkAgent = "fork1"
			}
			parentNext := conv(append(parent, "a3", "u4")...)
			forkNext := conv(append(parent, "fork-a", "fork-u")...)
			var pid, fid string
			if forkFirst {
				fid = tr.Resolve("t\x00s", forkAgent, forkNext, false).ID
				pid = tr.Resolve("t\x00s", "", parentNext, false).ID
			} else {
				pid = tr.Resolve("t\x00s", "", parentNext, false).ID
				fid = tr.Resolve("t\x00s", forkAgent, forkNext, false).ID
			}
			if pid == fid {
				t.Errorf("%s: fork and parent share thread %q", name, pid)
			}
			// Both keep their own thread on their next turn.
			pid2 := tr.Resolve("t\x00s", "", conv(append(parent, "a3", "u4", "a4", "u5")...), false).ID
			fid2 := tr.Resolve("t\x00s", forkAgent, conv(append(parent, "fork-a", "fork-u", "fa2", "fu2")...), false).ID
			if pid2 != pid || fid2 != fid {
				t.Errorf("%s: threads did not hold: parent %q->%q fork %q->%q", name, pid, pid2, fid, fid2)
			}
			if useHeader && !forkFirst && pid != Primary {
				t.Errorf("%s: the parent lost the primary id: %q", name, pid)
			}
		}
	}
}

// A subagent that speaks first (after a restart, say) must not take the primary id from the main
// thread, which never carries a header.
func TestHeaderThreadsDoNotTakeThePrimaryID(t *testing.T) {
	tr := testTracker()
	if id := tr.Resolve("t\x00s", "agentS", conv("sub task"), false).ID; id != "a:agentS" {
		t.Fatalf("subagent thread = %q", id)
	}
	if id := tr.Resolve("t\x00s", "", conv("main task"), false).ID; id != Primary {
		t.Fatalf("main thread after a subagent spoke first = %q, want primary", id)
	}
}

// The agent's own compaction ends one prefix of a thread. The next request starts from a summary,
// matches nothing, and must still be the same thread — or its miss reads as a cold start of a new
// thread instead of the prefix change it is, and its keep-alive entry is orphaned.
func TestTheRequestAfterAnAgentCompactionKeepsItsThread(t *testing.T) {
	tr := testTracker()
	tr.Resolve("t\x00s", "", conv("task", "a1", "u2"), false)
	tr.Resolve("t\x00s", "agentS", conv("sub"), false)
	if id := tr.Resolve("t\x00s", "", conv("task", "a1", "u2", "summarize this conversation"), true).ID; id != Primary {
		t.Fatalf("compaction request thread = %q", id)
	}
	if id := tr.Resolve("t\x00s", "", conv("summary of everything", "continue"), false).ID; id != Primary {
		t.Fatalf("post-compaction request thread = %q, want the compacted thread's id", id)
	}
	// Control: WITHOUT the compaction mark, the same request is a new thread. Otherwise the
	// assertion above would pass for a reason that has nothing to do with compaction.
	tr2 := testTracker()
	tr2.Resolve("t\x00s", "", conv("task", "a1", "u2"), false)
	if id := tr2.Resolve("t\x00s", "", conv("summary of everything", "continue"), false).ID; id == Primary {
		t.Fatal("control: an unrelated request joined the primary thread")
	}
}

// Test 7 at the source: the OpenAI chat and Responses dialects group the same way.
func TestOpenAIAndResponsesDialectsGroupByPrefix(t *testing.T) {
	chat := func(n int, first string) []byte {
		msgs := []any{map[string]any{"role": "system", "content": "sys"}}
		for i := 0; i < n; i++ {
			if i == 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": first})
				continue
			}
			msgs = append(msgs,
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprint("c", first, i), "type": "function"}}},
				map[string]any{"role": "tool", "tool_call_id": fmt.Sprint("c", first, i), "content": "out"})
		}
		b, _ := json.Marshal(map[string]any{"model": "gpt-5", "messages": msgs})
		return b
	}
	responses := func(n int, first string) []byte {
		items := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": first}}}}
		for i := 1; i < n; i++ {
			items = append(items,
				map[string]any{"type": "reasoning", "encrypted_content": fmt.Sprint("enc", i)},
				map[string]any{"type": "function_call", "call_id": fmt.Sprint("c", first, i), "name": "shell"},
				map[string]any{"type": "function_call_output", "call_id": fmt.Sprint("c", first, i), "output": "out"})
		}
		b, _ := json.Marshal(map[string]any{"model": "gpt-5", "input": items})
		return b
	}
	for name, mk := range map[string]func(int, string) []byte{"chat": chat, "responses": responses} {
		tr := testTracker()
		ids := []string{
			tr.Resolve("t\x00s", "", mk(1, "main"), false).ID,
			tr.Resolve("t\x00s", "", mk(2, "main"), false).ID,
			tr.Resolve("t\x00s", "", mk(1, "sub"), false).ID,
			tr.Resolve("t\x00s", "", mk(3, "main"), false).ID,
			tr.Resolve("t\x00s", "", mk(2, "sub"), false).ID,
		}
		if want := "0,0,1,0,1,"; partition(ids) != want {
			t.Errorf("%s: grouping %v (%s), want %s", name, ids, partition(ids), want)
		}
		if ids[0] != Primary {
			t.Errorf("%s: the first thread is %q, not primary", name, ids[0])
		}
	}
}

// Test 5's tracker half: a request with no usable message list falls back to the session, and
// leaves no state that could merge later requests.
func TestNoMessageListFallsBackToTheSession(t *testing.T) {
	tr := testTracker()
	r := tr.Resolve("t\x00s", "", []byte(`{"previous_response_id":"r1","input":"hi"}`), false)
	if r.ID != Primary || r.Source != SourceSession {
		t.Fatalf("got %+v, want the session", r)
	}
	var nilTracker *Tracker
	if r := nilTracker.Resolve("t\x00s", "x", conv("a"), false); r.ID != Primary {
		t.Fatalf("nil tracker gave %+v", r)
	}
}

func TestValidHeaderID(t *testing.T) {
	for v, want := range map[string]bool{
		"a9b99ca1f8284c823": true, "agent_1.x:y-z": true,
		"": false, "has space": false, "new\nline": false, "nul\x00": false,
		string(make([]byte, maxHeaderID+1)): false,
	} {
		if got := ValidHeaderID(v); got != want {
			t.Errorf("ValidHeaderID(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestDisagreementIsReportedWhenTheHeaderNamesAKnownThread(t *testing.T) {
	tr := testTracker()
	tr.Resolve("t\x00s", "", conv("main"), false)
	tr.Resolve("t\x00s", "agentS", conv("sub"), false)
	// The header says agentS, but the message list continues the main thread.
	r := tr.Resolve("t\x00s", "agentS", conv("main", "a", "u"), false)
	if r.ID != "a:agentS" || !r.Disagree {
		t.Fatalf("got %+v, want the header's id with Disagree set", r)
	}
	// A NEW header thread that shares a prefix (a fork at creation) is not a disagreement.
	if r := tr.Resolve("t\x00s", "fork", conv("main", "a", "u", "b"), false); r.Disagree {
		t.Fatal("a fork's first request was reported as a disagreement")
	}
}

func TestThreadsPerSessionAreBounded(t *testing.T) {
	tr := testTracker()
	for i := 0; i < maxThreadsPerSession*3; i++ {
		tr.Resolve("t\x00s", "", conv(fmt.Sprint("aux ", i)), false)
	}
	if n := len(tr.sessions["t\x00s"].threads); n > maxThreadsPerSession {
		t.Fatalf("%d threads held, bound is %d", n, maxThreadsPerSession)
	}
}

// A new thread that is mostly an old conversation (a rewind, an edited turn) inherits that
// conversation, so its miss is not called a cold start. A genuinely new conversation (a
// subagent's task) does not, and neither does one that shares only a small leading part.
func TestANewThreadInheritsByContentMass(t *testing.T) {
	big := strings.Repeat("tool output line\n", 500)
	tr := testTracker()
	tr.Resolve("t\x00s", "", conv("task", big, "u2 "+big, "a2", "u3"), false)

	// Rewind: the user edits the latest turn. Everything before it is shared, and that is most
	// of the bytes.
	r := tr.Resolve("t\x00s", "", conv("task", big, "u2 "+big, "a2 EDITED"), false)
	if r.ID == Primary {
		t.Fatal("precondition: the rewind matched the primary thread by prefix, so the mass rule is not reached")
	}
	if from, ok := r.InheritsFrom(); !ok || from != Primary {
		t.Errorf("rewind: inherits (%q, %v), want the primary thread", from, ok)
	}
	// A subagent's first request shares nothing.
	if _, ok := tr.Resolve("t\x00s", "agentS", conv("investigate x"), false).InheritsFrom(); ok {
		t.Error("a subagent's first request inherited a thread")
	}
	// An edit right after the first message shares only that small first message.
	if r := tr.Resolve("t\x00s", "", conv("task", "different "+big, "u2"), false); r.Inherits != "" || r.inherits {
		t.Errorf("a request sharing a small prefix inherited %q", r.Inherits)
	}
	// Only a thread's FIRST request carries the mark: later turns are the thread's own.
	again := tr.Resolve("t\x00s", "", conv("task", big, "u2 "+big, "a2 EDITED", "next"), false)
	if _, ok := again.InheritsFrom(); ok {
		t.Error("the inherited thread's second request still carries the mark")
	}
}
