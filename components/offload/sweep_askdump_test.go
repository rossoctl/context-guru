package offload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// WHAT THESE PIN, and why the component needed it: `sweep_kept: 32` out of 34 adjudicated was the whole
// record of one probe's adjudication. Whether that judgement was RIGHT is not answerable from a count,
// and whether the PROMPT is at fault is not answerable from anything that survived the call — a prefix
// ask appends the inventory to the provider's cached transcript, so the conversation the model reasoned
// over is in no outgoing body and in no log.
//
// So the dump has to carry both halves together, and each test below fails if one of them goes missing.

func readOneDump(t *testing.T, dir string) map[string]any {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	var files []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly one dump file, got %v", files)
	}
	b, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("dump is not valid json: %v", err)
	}
	return m
}

// OFF BY DEFAULT, and this is the assertion that matters most: the dump writes full conversation
// content to disk in the clear. Nothing may reach disk without an operator naming a directory.
// Mutation that must fail this: make sweepAskDumpDir return a path when the env var is empty.
func TestAskDumpWritesNothingUnlessConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CG_SWEEP_ASK_DUMP", "") // explicitly unset

	// THE GATE ITSELF, asserted directly. Checking that one directory stayed empty does not prove
	// nothing was written — a resolver that defaulted to os.TempDir() passed that check while dumping
	// every conversation to /tmp, which a mutation run demonstrated. The property is that no path is
	// produced at all unless an operator named one.
	if got := sweepAskDumpDir(); got != "" {
		t.Fatalf("sweepAskDumpDir() = %q with the variable unset: this facility writes conversation "+
			"content to disk and must resolve to nowhere by default", got)
	}
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := askOffload(e, req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("wrote %d file(s) with dumping unconfigured: this facility puts conversation "+
			"content on disk and must never be reachable by accident", len(ents))
	}
}

// BOTH HALVES, in one file: the prompt as sent, the transcript the model reasoned over, the reply, and
// a verdict row per candidate. A dump missing the transcript cannot answer "was the judgement right";
// one missing the prompt cannot answer "is the prompt at fault".
// Mutation that must fail this: stop populating Transcript in newSweepAskDump.
func TestAskDumpCarriesThePromptTheTranscriptAndTheVerdicts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CG_SWEEP_ASK_DUMP", dir)
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := askOffload(e, req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	m := readOneDump(t, dir)

	if p, _ := m["ask_prompt"].(string); len(p) < 50 {
		t.Fatalf("ask_prompt is missing or trivial (%d bytes): the prompt under review is the point", len(p))
	}
	tr, _ := m["transcript"].([]any)
	if len(tr) != len(req.Input) {
		t.Fatalf("transcript has %d messages, the request had %d: a partial transcript cannot support "+
			"a judgement about whether a verdict was right", len(tr), len(req.Input))
	}
	// The transcript must carry TEXT, not just counts — a dump of token totals is not reviewable.
	var withText int
	for _, x := range tr {
		if mm, ok := x.(map[string]any); ok {
			if s, _ := mm["text"].(string); s != "" {
				withText++
			}
		}
	}
	if withText == 0 {
		t.Fatal("no transcript message carries its text: the dump records shape without content")
	}
	inv, _ := m["inventory"].([]any)
	if len(inv) == 0 {
		t.Fatal("inventory is empty: nothing records what the model was offered")
	}
	for _, x := range inv {
		mm := x.(map[string]any)
		if s, _ := mm["content"].(string); s == "" {
			t.Fatal("an inventory entry carries no content: the candidate being judged is not recorded")
		}
	}
	if vs, _ := m["verdicts"].([]any); len(vs) == 0 {
		t.Fatal("no verdict rows: the judgement itself is not recorded")
	}
	if r, _ := m["reply"].(string); r == "" {
		t.Fatal("the reply is not recorded")
	}
}

// THE OUTCOME IS THE COMPONENT'S ACTION, not the model's answer, and the two diverge — which is the
// row worth reading. A keep must be recorded as a keep.
// Mutation that must fail this: drop the noteVerdict call on the `!a.Drop` branch.
func TestAskDumpRecordsWhatTheComponentDidNotJustWhatTheModelSaid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CG_SWEEP_ASK_DUMP", dir)
	// A KEEP: the model says the output is still needed.
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := askOffload(e, req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	m := readOneDump(t, dir)
	vs, _ := m["verdicts"].([]any)
	if len(vs) == 0 {
		t.Fatal("no verdict rows recorded for an ask that was answered")
	}
	var kept int
	for _, x := range vs {
		mm := x.(map[string]any)
		out, _ := mm["outcome"].(string)
		drop, _ := mm["model_said_drop"].(bool)
		if out == "kept" {
			kept++
			if drop {
				t.Fatal("a row says the model asked to drop and the outcome was kept, with no " +
					"refused_obligation: the two fields are not being taken from the same decision")
			}
		}
	}
	if kept == 0 {
		t.Fatalf("the adjudicator kept every candidate and no row records a keep: outcomes are not "+
			"being noted at the branch that took them (rows: %v)", vs)
	}
}
