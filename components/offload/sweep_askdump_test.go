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
	e := newSweep(t, "econ_trigger: true\n")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
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
	e := newSweep(t, "econ_trigger: true\n")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
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
	e := newSweep(t, "econ_trigger: true\n")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
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

// The econ terms travel with the judgement, so a verdict can be read against the economics that paid
// for it rather than in isolation.
func TestAskDumpCarriesTheEconTermsThatBoughtTheAsk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CG_SWEEP_ASK_DUMP", dir)
	asker := &labelAsker{verdict: "drop", needed: "none"}
	e := newSweep(t, "econ_trigger: true\nreward_premium: 20\n")
	req := sweepReqStocked()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	// TRIGGER ONE OFF, so the econ decision is the only thing that can authorise this ask. Without
	// this the pre-expiry trigger fires, there IS no break-even, and the dump correctly carries no
	// econ block — which the sibling test below asserts.
	c.IdleMs = 30 * 1000
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	m := readOneDump(t, dir)
	if got, _ := m["trigger"].(string); got != "econ" {
		t.Fatalf("trigger = %q, want \"econ\": the fixture is not exercising the break-even", got)
	}
	ec, ok := m["econ"].(map[string]any)
	if !ok {
		t.Fatal("no econ block: the judgement cannot be read against the decision that bought it")
	}
	if p, _ := ec["premium"].(float64); p != 20 {
		t.Fatalf("econ.premium = %v, want 20 — the dump is not reading the configured premium", p)
	}
	if rt, _ := ec["req_tokens"].(float64); rt <= 0 {
		t.Fatalf("econ.req_tokens = %v: the terms are not being carried from the econ decision", rt)
	}
}

// AN ABSENT ECON BLOCK IS NOT A ZERO ONE. The pre-expiry trigger never evaluates the break-even, so
// there is no need, no have and no premium on those asks — and a zero-valued block would read as
// "premium was 0", which is a different claim. Absent-versus-zero has produced three separate
// misreadings in this component's history, so it is pinned rather than trusted to convention.
// Mutation that must fail this: make Econ a value type again.
func TestAskDumpOmitsTheEconBlockWhenNoBreakEvenWasEvaluated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CG_SWEEP_ASK_DUMP", dir)
	asker := &labelAsker{verdict: "drop", needed: "none"}
	e := newSweep(t, "econ_trigger: true\nreward_premium: 20\n")
	req := sweepReqStocked()
	// Inside the pre-expiry window: trigger one fires and the break-even is never reached.
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := e.Offload(req, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	m := readOneDump(t, dir)
	if got, _ := m["trigger"].(string); got != "pre_expiry" {
		t.Fatalf("trigger = %q, want \"pre_expiry\"", got)
	}
	if _, present := m["econ"]; present {
		t.Fatal("an econ block is present on a pre-expiry ask, where no break-even was evaluated: a " +
			"reader would take premium=0 as the configured premium")
	}
}
