package offload

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// capturingModel records the message array it was asked with, so a test can assert on the REQUEST
// rather than on a model's wording. It implements both components.Model and MessagesModel.
type capturingModel struct {
	gotSystem string
	gotMsgs   []bschemas.ChatMessage
	out       string
	calls     int64
}

func (m *capturingModel) Complete(context.Context, string) (string, error) {
	// Deliberately distinguishable: if a future change makes the component fall back to the
	// flat-string path, the assertions below fail loudly instead of passing on a wrong shape.
	return "FLAT-STRING PATH — the prefix was destroyed", nil
}

func (m *capturingModel) CompleteMessages(_ context.Context, system string, msgs []bschemas.ChatMessage) (string, error) {
	m.calls++
	m.gotSystem = system
	m.gotMsgs = append([]bschemas.ChatMessage(nil), msgs...)
	return m.out, nil
}

// plainModel implements only components.Model, to prove the component DECLINES rather than
// degrading to the prefix-destroying shape.
type plainModel struct{ calls int }

func (m *plainModel) Complete(context.Context, string) (string, error) {
	m.calls++
	return "<summary>should never be reached</summary>", nil
}

type caErroringModel struct{}

func (caErroringModel) Complete(context.Context, string) (string, error) { return "", nil }
func (caErroringModel) CompleteMessages(context.Context, string, []bschemas.ChatMessage) (string, error) {
	return "", errors.New("upstream refused")
}

type caEmptyModel struct{}

func (caEmptyModel) Complete(context.Context, string) (string, error) { return "", nil }
func (caEmptyModel) CompleteMessages(context.Context, string, []bschemas.ChatMessage) (string, error) {
	return "   ", nil
}

func caMsg(role bschemas.ChatMessageRole, text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: role}
	schema.SetMessageText(&m, text)
	return m
}

// caToolPair returns an assistant message that REQUESTS a tool call plus its result, so the fixture
// exercises pairing rather than a flat list of text messages: without real ToolCalls / ToolCallID
// the atomicity logic and the wire mapping's tool branches are never reached.
func caToolPair(text, tool, args, id, out string) []bschemas.ChatMessage {
	a := caMsg(bschemas.ChatMessageRoleAssistant, text)
	a.ChatAssistantMessage = &bschemas.ChatAssistantMessage{
		ToolCalls: []bschemas.ChatAssistantMessageToolCall{
			{ID: &id, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &tool, Arguments: args}},
		},
	}
	t := caMsg(bschemas.ChatMessageRoleTool, out)
	t.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &id}
	return []bschemas.ChatMessage{a, t}
}

func caFixture() []bschemas.ChatMessage {
	body := strings.Repeat("ran pytest tests/test_handler.py, 3 failures in src/mod/file.py\n", 40)
	out := []bschemas.ChatMessage{caMsg(bschemas.ChatMessageRoleUser, "TASK: fix the failing handler")}
	out = append(out, caToolPair("reading the file", "read", `{"p":"a"}`, "call_1", body)...)
	out = append(out, caToolPair("running the tests", "bash", `{"c":"pytest"}`, "call_2", body)...)
	out = append(out, caToolPair("patching", "edit", `{"p":"a"}`, "call_3", body)...)
	out = append(out, caMsg(bschemas.ChatMessageRoleUser, "keep going"))
	return out
}

func newCacheAware(t *testing.T, yamlCfg string) *CacheAwareSummarizer {
	t.Helper()
	c, err := newCacheAwareSummarizer([]byte(yamlCfg))
	if err != nil {
		t.Fatalf("newCacheAwareSummarizer: %v", err)
	}
	s, ok := c.(*CacheAwareSummarizer)
	if !ok {
		t.Fatalf("got %T, want *CacheAwareSummarizer", c)
	}
	return s
}

func caCtx(session string) *components.Ctx {
	return &components.Ctx{Ctx: context.Background(), Session: session,
		Store: store.NewMemory(store.Options{}), MaxCachedIdx: -1}
}

// marker_mode is FULL on purpose: "off" would leave the whole reversibility path — Marshal,
// PutStash, hashKey, expand.Marker — uncovered, which is where the store-refusal defect lived.
const caBaseCfg = "keep_last_turns: 2\nmin_tokens: 10\nresummarize_tokens: 6000\n" +
	"trigger:\n  min_messages: 4\n  min_request_tokens: 10\n"

// caTurn runs ONE turn and returns the forwarded request.
func caTurn(t *testing.T, s *CacheAwareSummarizer, c *components.Ctx, msgs []bschemas.ChatMessage) (*bschemas.BifrostChatRequest, *components.Report) {
	t.Helper()
	req := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), msgs...)}
	var rep components.Report
	if _, err := s.Offload(req, &rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	return req, &rep
}

// caRun drives the REAL two-turn sequence. The summary is commissioned OFF the hot path, so turn 1
// forwards UNTOUCHED and turn 2 is the turn that splices. Draining on the production channel
// (WaitForSummaryForTest) rather than sleeping is what makes this deterministic — and it is the same
// channel a real later turn waits on, so a passing test exercises production synchronisation.
func caRun(t *testing.T, s *CacheAwareSummarizer, session string, msgs []bschemas.ChatMessage) (*bschemas.BifrostChatRequest, *components.Ctx) {
	t.Helper()
	c := caCtx(session)
	t1, _ := caTurn(t, s, c, msgs)
	if len(t1.Input) != len(msgs) {
		t.Fatalf("turn 1 spliced (%d -> %d); it must forward untouched and commission the summary",
			len(msgs), len(t1.Input))
	}
	if !WaitForSummaryForTest(session, 5*time.Second) {
		t.Fatalf("the commissioned summary never landed, so turn 2 has nothing to splice")
	}
	t2, _ := caTurn(t, s, c, msgs)
	return t2, c
}

func holdsText(msgs []bschemas.ChatMessage, want string) bool {
	for i := range msgs {
		if strings.Contains(schema.MessageText(msgs[i]), want) {
			return true
		}
	}
	return false
}

// ⭐ THE PROPERTY THE WHOLE DESIGN RESTS ON.
//
// The saving comes from the backend recognising a prefix it already has. That only happens if the
// request is the conversation UNCHANGED with the instruction appended — every other summarizer here
// rebuilds the prompt, and rebuilding is what destroys the match. So the assertion is on BYTES:
// marshal each input message and the corresponding sent message and require them equal, in order,
// then require exactly one extra message at the end. The wire bytes are what the backend hashes, so
// byte equality is the property that actually matters.
func TestCacheAwareSendsTheConversationUnchangedPlusOneMessage(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	s.modelClient = model

	in := caFixture()
	if _, _ = caRun(t, s, "ca-bytes", in); model.calls != 1 {
		t.Fatalf("model called %d times, want exactly 1", model.calls)
	}
	if got, want := len(model.gotMsgs), len(in)+1; got != want {
		t.Fatalf("sent %d messages, want %d (the conversation plus exactly one instruction)", got, want)
	}
	for i := range in {
		wantB, err := json.Marshal(in[i])
		if err != nil {
			t.Fatal(err)
		}
		gotB, err := json.Marshal(model.gotMsgs[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(wantB) != string(gotB) {
			t.Fatalf("message %d was MODIFIED before sending — that forfeits the prefix match this "+
				"component exists for.\n want: %s\n got:  %s", i, wantB, gotB)
		}
	}
	last := model.gotMsgs[len(model.gotMsgs)-1]
	if last.Role != bschemas.ChatMessageRoleUser {
		t.Errorf("appended instruction role = %q, want user", last.Role)
	}
	if !strings.Contains(schema.MessageText(last), "OPERATOR INSTRUCTION") {
		t.Errorf("appended message is not the user-variant instruction: %.80q", schema.MessageText(last))
	}
	// system is empty on purpose: a leading block the parent never sent changes the prefix in the
	// costliest position.
	if model.gotSystem != "" {
		t.Errorf("system = %q, want empty", model.gotSystem)
	}
}

// The instruction role must be the configured one, and the two variants must differ in the way the
// registry describes: the user variant has to say in TEXT that this is an operator instruction and
// that the task must not be continued, because the system channel carries both for free.
func TestCacheAwareInstructionRoleAndPromptVariant(t *testing.T) {
	for _, tc := range []struct {
		role     string
		wantRole bschemas.ChatMessageRole
		mustHave string
	}{
		{"system", bschemas.ChatMessageRoleSystem, "Summarize the conversation above."},
		{"user", bschemas.ChatMessageRoleUser, "OPERATOR INSTRUCTION"},
	} {
		// A pinned `system` needs a model the registry ALLOWS, or Offload declines by design.
		// system_models is empty on the embedded registry, so the only way to reach the system
		// channel at all is an override that promotes a model — which is also the documented
		// route for a deployment that has verified its own path.
		cfg := caBaseCfg + "instruction_role: " + tc.role + "\n"
		if tc.role == "system" {
			cfg += "model_id: Qwen/Qwen3.6-27B\nprofiles_path: " +
				caWriteProfiles(t, "qwen3") + "\n"
		}
		s := newCacheAware(t, cfg)
		model := &capturingModel{out: "<summary>ok</summary>"}
		s.modelClient = model
		caRun(t, s, "ca-role-"+tc.role, caFixture())
		if model.calls == 0 {
			t.Fatalf("%s: no call was made, so the assertions below are vacuous", tc.role)
		}
		last := model.gotMsgs[len(model.gotMsgs)-1]
		if last.Role != tc.wantRole {
			t.Errorf("instruction_role %s: got role %q, want %q", tc.role, last.Role, tc.wantRole)
		}
		if !strings.Contains(schema.MessageText(last), tc.mustHave) {
			t.Errorf("instruction_role %s: prompt missing %q", tc.role, tc.mustHave)
		}
		if s.InstructionRole() != string(tc.wantRole) {
			t.Errorf("InstructionRole() = %q, want %q", s.InstructionRole(), tc.wantRole)
		}
	}
}

// ⛔ A client that cannot send a message array must make the component DECLINE, never fall back to
// Complete(). The fallback would report this method's latency while paying the rebuild cost — i.e.
// measure the opposite of the hypothesis — and it would do so silently.
func TestCacheAwareDeclinesRatherThanFlatteningThePrompt(t *testing.T) {
	before := CacheAwareSummarizerDeclined()
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	plain := &plainModel{}
	s.modelClient = plain

	in := caFixture()
	req, rep := caTurn(t, s, caCtx("ca-decline"), in)
	if !rep.Skipped {
		t.Error("did not skip on a client with no CompleteMessages")
	}
	if plain.calls != 0 {
		t.Errorf("fell back to Complete() %d times — that is the prefix-destroying path", plain.calls)
	}
	if len(req.Input) != len(in) {
		t.Errorf("transcript was modified on a decline: %d -> %d", len(in), len(req.Input))
	}
	if CacheAwareSummarizerDeclined() != before+1 {
		t.Errorf("declined counter did not increment; a declining arm would be indistinguishable "+
			"from `off` (before=%d after=%d)", before, CacheAwareSummarizerDeclined())
	}
}

// The SHIPPED registry grants the system channel to nothing: system_models is empty, so every
// model — including the ones whose own templates were verified to accept a trailing system
// message — resolves to `user`.
//
// ⛔ The claude ids are the reason the allow-list exists, and they are asserted here rather than
// left to the generic case. `claude-opus-5` resolved to `system` off a profile that described the
// NATIVE Anthropic API, was reached over an OpenAI-shaped gateway that lifts a mid-array system
// message into the top-level field, and 400'd on every call while the component reported only a
// counter. A future entry re-granting it must fail this test loudly.
func TestCacheAwareAutoRoleIsUserForEveryShippedModel(t *testing.T) {
	for _, id := range []string{
		"aws/claude-opus-5", "aws/claude-sonnet-5", "claude-opus-5", "claude-sonnet-5",
		"claude-opus-4-8", "claude-fable-5-1", "claude-mythos-5", "claude-haiku-4-5",
		"Qwen/Qwen3.6-27B", "meta-llama/Llama-3.3-70B-Instruct", "meta-llama/Llama-3.1-8B-Instruct",
		"deepseek-ai/DeepSeek-V3", "mistralai/Mistral-7B-Instruct-v0.3", "openai/gpt-oss-120b",
		"google/gemma-2-27b-it", "meta-llama/Llama-2-7b-chat-hf", "codellama/CodeLlama-34b",
		"Qwen/Qwen2-7B-Instruct", "some-model-nobody-has-checked", "",
	} {
		s := newCacheAware(t, caBaseCfg+"instruction_role: auto\nmodel_id: \""+id+"\"\n")
		if got := s.InstructionRole(); got != "user" {
			t.Errorf("model_id %q: resolved role %q, want user — system_models is empty, so "+
				"nothing may resolve to the system channel", id, got)
		}
	}
}

// A promotion works, and only for the id it names. This is the escape hatch a deployment uses
// after verifying its OWN path, and the test that keeps the allow-list from being decorative.
func TestCacheAwareSystemModelsAllowListPromotesOnlyWhatItNames(t *testing.T) {
	path := caWriteProfiles(t, "qwen3")
	for _, tc := range []struct{ id, want string }{
		{"Qwen/Qwen3.6-27B", "system"},
		{"aws/claude-opus-5", "user"},
		{"Qwen/Qwen2-7B-Instruct", "user"},
	} {
		s := newCacheAware(t, caBaseCfg+"instruction_role: auto\nmodel_id: \""+tc.id+
			"\"\nprofiles_path: "+path+"\n")
		// auto re-resolves per request when profiles_path is set, so drive a turn rather than
		// reading the construction-time answer.
		s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
		model := s.modelClient.(*capturingModel)
		caRun(t, s, "ca-allow-"+tc.id, caFixture())
		if model.calls == 0 {
			t.Fatalf("%s: no call was made, so the assertion below is vacuous", tc.id)
		}
		last := model.gotMsgs[len(model.gotMsgs)-1]
		if string(last.Role) != tc.want {
			t.Errorf("model_id %q with qwen3 promoted: instruction role %q, want %q",
				tc.id, last.Role, tc.want)
		}
	}
}

// caWriteProfiles writes a registry identical to the embedded one except that system_models names
// the given match strings, so a test can exercise the system channel the shipped file grants to
// nothing.
func caWriteProfiles(t *testing.T, allow ...string) string {
	t.Helper()
	body := string(summarizerProfilesYAML)
	list := "system_models: ["
	for i, a := range allow {
		if i > 0 {
			list += ", "
		}
		list += "\"" + a + "\""
	}
	list += "]"
	replaced := strings.Replace(body, "system_models: []", list, 1)
	if replaced == body {
		t.Fatal("the embedded registry no longer contains `system_models: []`; this helper " +
			"silently stopped promoting anything and every system-role assertion became vacuous")
	}
	path := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(path, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The output shape must be [head, summary, tail], and the spliced summary must never be
// system-role: a system message anywhere but index 0 is rejected by the provider.
func TestCacheAwareSpliceShape(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}
	in := caFixture()
	out, _ := caRun(t, s, "ca-shape", in)
	if len(out.Input) < 3 || len(out.Input) >= len(in) {
		t.Errorf("spliced to %d messages, want between 3 and %d", len(out.Input), len(in)-1)
	}
	if !holdsText(out.Input, "TASK:") {
		t.Errorf("head not preserved: %v", roles(out.Input))
	}
	for i := range out.Input {
		if out.Input[i].Role == bschemas.ChatMessageRoleSystem {
			t.Errorf("system-role message at index %d — the provider rejects this", i)
		}
	}
	if !holdsText(out.Input, "cache-aware summary") {
		t.Error("summary message missing its wrapper")
	}
	// The kept tail must never BEGIN with a tool result whose call was summarized away.
	if out.Input[2].Role == bschemas.ChatMessageRoleTool {
		t.Error("the kept tail begins with a tool result — its call was summarized away")
	}
}

// ⭐ BLOCKER: without a checkpoint this component invalidates the cache it is named for. Turn 3 must
// REUSE the summary byte-identically — no second model call — so the forwarded prefix is stable.
func TestCacheAwareReusesItsSummaryRatherThanReDerivingIt(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>explored the handler.</summary>"}
	s.modelClient = model

	in := caFixture()
	t2, c := caRun(t, s, "ca-reuse", in)
	first := schema.MessageText(t2.Input[1])
	if model.calls != 1 {
		t.Fatalf("model called %d times through turn 2, want 1", model.calls)
	}
	// Turn 3: one more exchange appended, still well under resummarize_tokens.
	in3 := append(append([]bschemas.ChatMessage(nil), in...),
		caMsg(bschemas.ChatMessageRoleAssistant, "one more step"))
	t3, _ := caTurn(t, s, c, in3)
	if model.calls != 1 {
		t.Errorf("model called %d times by turn 3, want 1 — turn 3 must REUSE the checkpoint", model.calls)
	}
	if got := schema.MessageText(t3.Input[1]); got != first {
		t.Errorf("the spliced summary changed between turns, so the forwarded prefix changed and the "+
			"cache this component exists to protect was invalidated:\n t2: %.60q\n t3: %.60q", first, got)
	}
}

// ⛔ A pinned `system` for a model no profile verifies is the silent-failure path the registry
// exists to prevent. It must DECLINE, visibly, rather than risk a summary that is really the
// model's next turn.
func TestCacheAwareDeclinesAPinnedSystemRoleForAnUnverifiedModel(t *testing.T) {
	before := CacheAwareSummarizerUnverifiedSystem()
	s := newCacheAware(t, caBaseCfg+"instruction_role: system\nmodel_id: some-model-nobody-checked\n")
	model := &capturingModel{out: "<summary>ok</summary>"}
	s.modelClient = model
	_, rep := caTurn(t, s, caCtx("ca-unverified"), caFixture())
	if !rep.Skipped {
		t.Error("did not decline a pinned system role for an unverified model")
	}
	if model.calls != 0 {
		t.Errorf("paid for %d model call(s) on a configuration that cannot be trusted", model.calls)
	}
	if CacheAwareSummarizerUnverifiedSystem() != before+1 {
		t.Error("the decline was not counted, so it would be invisible at /stats")
	}
}

// Fail-open on a model error. The call is DETACHED, so Offload returns nil and the transcript is
// untouched; the failure shows up in the counter and in the started/committed gap, which is the only
// place a degraded summarizer surfaces when nothing on the hot path waits for it.
func TestCacheAwareFailsOpenAndCountsTheError(t *testing.T) {
	beforeErr := CacheAwareSummarizerErrors()
	startedBefore, committedBefore, _, _ := CacheAwareAsyncStats()
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = caErroringModel{}

	in := caFixture()
	c := caCtx("ca-failopen")
	t1, _ := caTurn(t, s, c, in)
	if len(t1.Input) != len(in) {
		t.Errorf("transcript was modified on a failing turn: %d -> %d", len(in), len(t1.Input))
	}
	if !WaitForSummaryForTest("ca-failopen", 5*time.Second) {
		t.Fatal("the detached call never resolved")
	}
	if CacheAwareSummarizerErrors() != beforeErr+1 {
		t.Error("the model error was not counted")
	}
	started, committed, _, _ := CacheAwareAsyncStats()
	if started != startedBefore+1 || committed != committedBefore {
		t.Errorf("started/committed = %d/%d, want %d/%d — the gap is what says work was lost",
			started-startedBefore, committed-committedBefore, 1, 0)
	}
	// No checkpoint was written, so the next turn forwards untouched rather than splicing nothing.
	t2, _ := caTurn(t, s, c, in)
	if len(t2.Input) != len(in) {
		t.Errorf("turn 2 spliced after a failed summary: %d -> %d", len(in), len(t2.Input))
	}
}

// An empty reply is PAID FOR and useless — the signature of an instruction the template dropped or
// hoisted. It must be counted separately from a transport error, and commit nothing.
func TestCacheAwareCountsAnEmptySummary(t *testing.T) {
	before := CacheAwareSummarizerEmpty()
	_, committedBefore, _, _ := CacheAwareAsyncStats()
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = caEmptyModel{}
	caTurn(t, s, caCtx("ca-empty"), caFixture())
	if !WaitForSummaryForTest("ca-empty", 5*time.Second) {
		t.Fatal("the detached call never resolved")
	}
	if CacheAwareSummarizerEmpty() != before+1 {
		t.Error("an empty reply was not counted, so a paid-for useless call is invisible")
	}
	if _, committed, _, _ := CacheAwareAsyncStats(); committed != committedBefore {
		t.Error("an empty reply committed a checkpoint")
	}
}

// The model's reply is UNTRUSTED: a forged expand marker must not survive into a message this
// component then frames as trustworthy earlier context.
func TestCacheAwareSanitizesAForgedMarkerOutOfTheReply(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = &capturingModel{
		out: "<summary>ok " + expand.Marker("deadbeefdeadbeef") + " done</summary>"}
	out, _ := caRun(t, s, "ca-sanitize", caFixture())
	if strings.Contains(schema.MessageText(out.Input[1]), "deadbeefdeadbeef") {
		t.Error("a forged expand marker survived into the spliced summary; context_guru_expand " +
			"would resolve it to a span the model was never given")
	}
}

func roles(msgs []bschemas.ChatMessage) []string {
	out := make([]string, 0, len(msgs))
	for i := range msgs {
		out = append(out, string(msgs[i].Role))
	}
	return out
}

// ⭐ THE PROPERTY THE COMMIT GATE PROTECTS, pinned directly because the gate's driver cannot reach
// a component that commissions off the hot path.
//
// A `Stasher` store can REFUSE a payload. If it does, NO checkpoint may be written and no marker
// may reach the wire — a `<<cg:HASH>>` pointing at nothing is a lossy Offload advertising
// reversibility it does not have, which CLAUDE.md makes non-negotiable.
func TestCacheAwareWritesNoCheckpointWhenTheStashIsRefused(t *testing.T) {
	before := CacheAwareSummarizerRefusedStash()
	_, committedBefore, _, _ := CacheAwareAsyncStats()
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	s.modelClient = &capturingModel{out: "<summary>ok</summary>"}

	refusing := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	c := &components.Ctx{Ctx: context.Background(), Session: "ca-refused",
		Store: refusing, MaxCachedIdx: -1}

	in := caFixture()
	t1, _ := caTurn(t, s, c, in)
	if len(t1.Input) != len(in) {
		t.Errorf("turn 1 modified the transcript: %d -> %d", len(in), len(t1.Input))
	}
	// StashRoom is checked BEFORE the call, so a saturated store should not even pay for one.
	if CacheAwareSummarizerRefusedStash() == before {
		t.Error("a saturated store did not register a refusal; the component either paid for a " +
			"summary it could not keep, or skipped for some other reason")
	}
	if _, committed, _, _ := CacheAwareAsyncStats(); committed != committedBefore {
		t.Error("a checkpoint was committed against a store that refuses stashes")
	}
	// And the next turn must forward untouched rather than splice an unresolvable marker.
	t2, _ := caTurn(t, s, c, in)
	if len(t2.Input) != len(in) {
		t.Errorf("turn 2 spliced after a refused stash: %d -> %d", len(in), len(t2.Input))
	}
	for _, m := range t2.Input {
		if strings.Contains(schema.MessageText(m), "<<cg:") {
			t.Error("a marker reached the wire with no stash behind it")
		}
	}
}
