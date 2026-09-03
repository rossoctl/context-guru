package offload

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE INVARIANT THIS FILE EXISTS FOR
//
// commitMark is the only thing that knows whether a removal is happening, because #188 gave it
// the power to refuse. So NOTHING DERIVED FROM THE REMOVAL MAY BE RECORDED BEFORE IT RETURNS
// TRUE — not the splice, not a frozen decision, not a metric, not rep.Replay.
//
// It is an invariant rather than three bugs because the failure it produces is worse than the
// lost saving. A frozen decision with no splice behind it is a broken promise one layer up: on
// a later turn the same-session replay path reads that record and DELIBERATELY BYPASSES the
// cache-tail gate — its reasoning is that these bytes were already sent — so once a reserve
// slot frees, the compaction is spliced into a message that is by then inside the provider's
// cached prefix, forcing a full-suffix cache write at ~11.5x the read price. #188's
// reversibility fix would have introduced a reversibility-adjacent bug.
//
// Three call sites had it (extract_llm's phase 3, its cross-session hit, the sweep's phase 3),
// which is why this is a table over components rather than three tests: the next caller to get
// it wrong is the one nobody has written yet. gateExempt below is what makes that a hard
// failure instead of a silent gap.

// decisionPrefixes are the namespaces whose contents assert that a removal HAPPENED. A write to
// any of them for a removal that was refused is the defect above.
var decisionPrefixes = []string{
	store.ResultPrefix,  // cg:res:  extract_llm / sweep replayed decision
	store.XResultPrefix, // cg:xres: the cross-session copy
	store.FrozenPrefix,  // cg:frz:  mask / collapse / skeleton / ... freeze
	"cg:own:",           // stash ownership — asserts a payload of ours is in the store
	"cg:sum:",           // summarize's checkpoint
}

// spyStore refuses every rewind payload and records every other write, so a test can ask "what
// did this component claim after being told it could not store the original?".
//
// It embeds *store.Memory rather than reimplementing the Store surface: the component behavior
// under test depends on Get/Sticky/etc. working normally, and a hand-written fake that quietly
// returned nothing would make every component skip for the wrong reason.
type spyStore struct {
	*store.Memory
	puts []string
}

func (s *spyStore) Put(key string, payload []byte) {
	s.puts = append(s.puts, key)
	s.Memory.Put(key, payload)
}

// PutStash refuses everything — a reserve with no room at all — and records nothing, because a
// refused payload is not a write.
func (s *spyStore) PutStash(string, []byte) bool { return false }

// StashRoom matches PutStash, or a component that probes first would be told to proceed and
// then refused, testing a path this fixture is not aiming at.
func (s *spyStore) StashRoom(int) bool { return false }

func (s *spyStore) decisionWrites() []string {
	var out []string
	for _, k := range s.puts {
		for _, p := range decisionPrefixes {
			if strings.HasPrefix(k, p) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// wireMarkerRe matches a marker in MARSHALLED bytes, in both spellings. encoding/json
// HTML-escapes "<", and every marker follows a newline, so on the wire a marker usually exists
// only as \u003c\u003ccg:HASH\u003e\u003e — a scan for the plain form alone reports zero
// markers on a body full of them.
var wireMarkerRe = regexp.MustCompile(`(?:<|(?i:\\u003c)){2}cg:([A-Za-z0-9_-]{1,64})(?:>|(?i:\\u003e)){2}`)

func markersOnWire(t *testing.T, msgs []bschemas.ChatMessage) []string {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range wireMarkerRe.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	return out
}

// gateCase is one component driven to the commit gate. build returns the component and the
// request; the same pair is built twice, once against a healthy store and once against a
// saturated one, so the two runs cannot share mutated state.
type gateCase struct {
	name  string
	build func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx)
}

// gateExempt lists registered Offload components this table does NOT drive, with the reason.
// Adding a component without adding a case here or above fails
// TestEveryOffloadComponentIsHeldToTheCommitGate — which is the point: the invariant's whole
// weakness is the caller nobody thought about.
var gateExempt = map[string]string{
	"failed_run": "needs a superseded failed RUN (a later successful invocation of the same " +
		"command) to have anything to collapse; the shape is covered by mask, which shares " +
		"its commit path verbatim — tryMark, commitMark, SetMessageText, freeze",
	"readlifecycle": "needs a read/edit event pair across messages to classify a body as " +
		"stale; same commit path as mask",
	"skeleton": "acts only on a line-numbered source dump or a fenced block whose grammar " +
		"tree-sitter recognises, so a fixture depends on the cg_skeleton build tag",
	"smartcrush": "acts only on a JSON array of records above its floor; same commit path " +
		"as dedup",
	"agentdiet": "reached only through a reflection model call over a multi-STEP trajectory, so " +
		"its candidates are steps rather than messages; pinned by " +
		"TestAgentDietDoesNotPayForAReflectionItCannotStash and " +
		"TestAgentDietRefusalsReachTheRefusalCounter",
	"summarize": "replaces a span rather than one message, so 'left verbatim' means a whole " +
		"skipped checkpoint; pinned by TestSummarizeSkipsTheCheckpointWhenTheSpanCannotBeStashed " +
		"and TestSummarizeReplaysItsCheckpointRatherThanFlippingCachedContent",
}

func ctxFor(st store.Store) *components.Ctx {
	return &components.Ctx{Ctx: context.Background(), Session: "s", Store: st, MaxCachedIdx: -1}
}

func toolMsgs(texts ...string) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Provider: bschemas.Anthropic}
	for _, txt := range texts {
		m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
		schema.SetMessageText(&m, txt)
		req.Input = append(req.Input, m)
	}
	return req
}

// noisyLines is content collapseObviousNoise provably reduces: the SAME line repeated
// consecutively, as a retry loop dumps a traceback. Distinct lines are not noise to it — every
// unique informative line is kept verbatim, which is the whole point of that reducer — so a
// fixture of "Downloading package-N" would reach the component and be declined before the gate,
// and the invariant test's precondition catches exactly that.
func noisyLines(tag string) string {
	head := "Traceback (most recent call last):\n  File \"" + tag + ".py\", line 3\nValueError: bad\n"
	return head + strings.Repeat(head, 200)
}

func gateCases() []gateCase {
	return []gateCase{
		{"mask", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newMask([]byte("keep_recent: 0\nmin_tokens: 20\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload), toolMsgs(wideOutput("m1"), wideOutput("m2")), ctxFor(st)
		}},
		{"linecap", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newLinecap([]byte("max_line_chars: 40\nmin_size: 10\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload), toolMsgs(wideOutput("l1"), wideOutput("l2")), ctxFor(st)
		}},
		{"collapse", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newCollapse([]byte("max_tokens: 20\nhead_lines: 2\ntail_lines: 2\n"))
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("a line of log output that goes on for a while\n", 80)
			return c.(components.Offload), toolMsgs(body, body+"tail\n"), ctxFor(st)
		}},
		{"dedup", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newDedup([]byte("min_tokens: 20\n"))
			if err != nil {
				t.Fatal(err)
			}
			body := wideOutput("dup")
			return c.(components.Offload), toolMsgs(body, body), ctxFor(st)
		}},
		{"extract", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newExtract([]byte("min_tokens: 20\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload), toolMsgs(noisyLines("a"), noisyLines("b")), ctxFor(st)
		}},
		{"extract_llm", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			// In the table rather than exempt, because the metric assertions below are the whole
			// point for this one: its savings, its ratio feed and its ledger row are all computed
			// in a goroutine before phase 3 exists, and the first fix round moved only the replay
			// path's. A model is the only reason it was ever exempt.
			model := &shrinkingModel{}
			e := newTimeoutTestComponent(t, model)
			body := strings.Repeat("2026-08-31T10:00:00Z INFO worker: processed batch\n", 400)
			req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
				userMsg("summarize the worker log"), toolResultMsg(body),
			}}
			return e, req, pricedCtx("gate-table", st, model)
		}},
		{"extract_llm_sweep", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			asker := &labelAsker{verdict: "drop", needed: "none"}
			asker.cacheRead = 19595
			e := newSweepSmall(t, "")
			c := preExpiryCtx("gate-table-sweep", asker, st)
			c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}
			return e, sweepReqStocked(), c
		}},
		{"cmdfilter", func(t *testing.T, st store.Store) (components.Offload, *bschemas.BifrostChatRequest, *components.Ctx) {
			c, err := newCmdfilter([]byte("min_size: 1\n"))
			if err != nil {
				t.Fatal(err)
			}
			const pipNag = "WARNING: Running pip as the 'root' user can result in broken permissions and " +
				"conflicting behaviour with the system package manager, possibly rendering your system " +
				"unusable. It is recommended to use a virtual environment instead: " +
				"https://pip.pypa.io/warnings/venv\n" +
				"WARNING: You are using pip version 21.0.1; however, version 23.0.1 is available.\n" +
				"You should consider upgrading via the '/usr/bin/python3 -m pip install --upgrade pip' command.\n"
			req := &bschemas.BifrostChatRequest{Provider: bschemas.Anthropic,
				Input: []bschemas.ChatMessage{cmdToolMsg(pipNag)}}
			return c.(components.Offload), req, ctxFor(st)
		}},
	}
}

// TestNoStateIsRecordedBeforeTheCommitGate is the invariant.
//
// Each case runs twice. The HEALTHY run is a precondition, not decoration: it proves the
// fixture actually reaches the gate and produces a marker and a decision. Without it a
// component that silently stopped acting — a changed default, a floor raised — would make the
// saturated run pass by doing nothing at all, and a vacuous pass is indistinguishable from a
// real one in the output.
func TestNoStateIsRecordedBeforeTheCommitGate(t *testing.T) {
	for _, tc := range gateCases() {
		t.Run(tc.name, func(t *testing.T) {
			// --- Precondition: with a store that CAN hold payloads, this fixture removes
			// something, stamps a resolvable marker, and records a decision.
			healthy := store.NewMemory(store.Options{MaxEntries: 400})
			comp, req, c := tc.build(t, healthy)
			rep := components.Report{Component: tc.name}
			if _, err := comp.Offload(req, &rep, c); err != nil {
				t.Fatalf("healthy run: %v", err)
			}
			healthyMarkers := markersOnWire(t, req.Input)
			if len(healthyMarkers) == 0 {
				t.Fatalf("the fixture stamped NO marker against a healthy store, so it never "+
					"reached the commit gate and the saturated run below would pass vacuously "+
					"(component %s: check its config and the candidate size)", tc.name)
			}
			for _, id := range healthyMarkers {
				if _, ok := expand.Resolve(healthy, id); !ok {
					t.Fatalf("healthy run: marker <<cg:%s>> does not resolve; the fixture is "+
						"broken before the invariant is even tested", id)
				}
			}

			// --- The invariant: told it cannot store originals, the component must record
			// nothing about a removal it did not make.
			spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
			comp, req, c = tc.build(t, spy)
			before := make([]string, len(req.Input))
			for i := range req.Input {
				before[i] = schema.MessageText(req.Input[i])
			}
			rep = components.Report{Component: tc.name}
			valueBefore, savedBefore := extractGrossValue(tc.name), extractGrossSaved(tc.name)
			if _, err := comp.Offload(req, &rep, c); err != nil {
				t.Fatalf("saturated run: %v", err)
			}
			if got := markersOnWire(t, req.Input); len(got) != 0 {
				t.Errorf("%d marker(s) reached the wire with no payload behind them: %v. A "+
					"refused removal must leave the message VERBATIM", len(got), got)
			}
			// METRICS, not only store writes. A saving booked for a removal that did not happen
			// is the same invariant one layer over: /stats reports value the run never delivered,
			// and for extract_llm the ratio tracker it also feeds prices FUTURE calls, so the
			// mis-recording outlives the turn that caused it. Asserted for every case in the
			// table rather than per component, because "which components record savings" is
			// exactly the kind of fact that changes without the test being revisited.
			if got := extractGrossSaved(tc.name) - savedBefore; got != 0 {
				t.Errorf("%d saved tokens were booked for removals that did not happen: the run "+
					"reports a saving it did not deliver", got)
			}
			if got := extractGrossValue(tc.name); got > valueBefore {
				t.Errorf("gross value rose from %v to %v for removals that did not happen",
					valueBefore, got)
			}
			for _, call := range rep.Calls {
				if call.Accepted {
					t.Error("a ledger row says accepted=true while every candidate went upstream " +
						"verbatim")
				}
				if call.SavedTokens != 0 {
					t.Errorf("a ledger row claims %d saved tokens for candidates left verbatim",
						call.SavedTokens)
				}
			}
			if got := spy.decisionWrites(); len(got) != 0 {
				t.Errorf("the component recorded %d decision(s) for a removal that did not "+
					"happen: %v.\nOn a later turn the same-session replay path reads such a "+
					"record and bypasses the cache-tail gate, splicing into a message that is "+
					"by then inside the provider's cached prefix — a full-suffix cache write "+
					"at ~11.5x the read price", len(got), got)
			}
			for i := range req.Input {
				if got := schema.MessageText(req.Input[i]); got != before[i] {
					t.Errorf("message %d was rewritten although its original could not be "+
						"stored:\n got %q\nwant %q", i, got, before[i])
				}
			}
		})
	}
}

// TestEveryOffloadComponentIsHeldToTheCommitGate is the completeness half, and it is the part
// that survives the next contributor.
//
// The invariant above is only as good as its table, and a table is exactly the thing a new
// component is not added to. So every registered Offload must be either driven above or listed
// in gateExempt with a reason — the same mechanism proxy/promexport_coverage_test.go uses to
// stop a new /stats field from quietly never reaching Prometheus.
func TestEveryOffloadComponentIsHeldToTheCommitGate(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range gateCases() {
		covered[tc.name] = true
	}
	for _, name := range components.Names() {
		c, err := components.New(name, nil)
		if err != nil {
			continue // not constructible with an empty config; not an Offload question
		}
		if _, ok := c.(components.Offload); !ok {
			continue
		}
		if covered[name] {
			if why, dup := gateExempt[name]; dup {
				t.Errorf("%s is both driven by gateCases and listed in gateExempt (%q); one of "+
					"the two is stale", name, why)
			}
			continue
		}
		if why := gateExempt[name]; strings.TrimSpace(why) == "" {
			t.Errorf("Offload %q is neither driven by TestNoStateIsRecordedBeforeTheCommitGate "+
				"nor listed in gateExempt with a reason.\nEvery offloader calls commitMark, and "+
				"commitMark can REFUSE — so this component may be recording a frozen decision, "+
				"a metric or a splice for a removal that never happened. Add a gateCase, or add "+
				"an entry to gateExempt naming the test that pins it instead.", name)
		}
	}
}
