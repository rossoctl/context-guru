package offload

import (
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// One row per model family. The shipped registry grants system to nothing, so the table also
// carries a promoted list, to prove the allow-list is the only way to the system role.
func TestResolveAskPerModelFamily(t *testing.T) {
	const user, system = bschemas.ChatMessageRoleUser, bschemas.ChatMessageRoleSystem
	promoted := []string{"qwen3"}
	for _, tc := range []struct {
		name, model string
		allow       []string
		ch          askChannel
		role        bschemas.ChatMessageRole
		allowed     bool
		opening     bool
	}{
		{"opus, no allow-list", "claude-opus-5", nil, askViaMessages, user, false, true},
		{"sonnet, no allow-list", "aws/claude-sonnet-5", nil, askViaMessages, user, false, true},
		{"haiku, no allow-list", "claude-haiku-4-5", nil, askViaMessages, user, false, true},
		{"fable, no allow-list", "claude-fable-5-1", nil, askViaMessages, user, false, true},
		{"open model, no allow-list", "Qwen/Qwen3.6-27B", nil, askViaMessages, user, false, true},
		{"unknown model", "some-model-nobody-has-checked", nil, askViaMessages, user, false, true},
		{"empty model id", "", promoted, askViaMessages, user, false, true},
		{"promoted model gets system", "Qwen/Qwen3.6-27B", promoted, askViaMessages, system, true, false},
		{"match is case-insensitive", "QWEN3-coder", promoted, askViaMessages, system, true, false},
		{"unpromoted claude stays user", "claude-opus-5", promoted, askViaMessages, user, false, true},
		{"prefix ask never sends system", "Qwen/Qwen3.6-27B", promoted, askViaPrefix, user, false, true},
		{"prefix ask, claude", "claude-sonnet-5", nil, askViaPrefix, user, false, true},
		{"standalone has no opening", "claude-sonnet-5", nil, askStandalone, user, false, false},
		{"standalone ignores the allow-list", "Qwen/Qwen3.6-27B", promoted, askStandalone, user, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAsk(tc.allow, tc.model, tc.ch)
			if got.Role != tc.role || got.SystemAllowed != tc.allowed {
				t.Errorf("role=%q allowed=%v, want role=%q allowed=%v",
					got.Role, got.SystemAllowed, tc.role, tc.allowed)
			}
			if (got.Opening == askOpeningLine) != tc.opening || (got.Opening != "" && !tc.opening) {
				t.Errorf("opening=%q, want present=%v", got.Opening, tc.opening)
			}
		})
	}
}

func TestAskFrameLeadsWithTheOpening(t *testing.T) {
	if got := resolveAsk(nil, "claude-sonnet-5", askViaPrefix).Frame("Q"); got != askOpeningLine+"\n\nQ" {
		t.Errorf("framed question = %q", got)
	}
	if got := resolveAsk(nil, "x", askStandalone).Frame("Q"); got != "Q" {
		t.Errorf("a standalone question must stay as written, got %q", got)
	}
}

// The summarizer's own yaml prompt and the shared line must not drift apart: the yaml is
// overridable (profiles_path) and stays the summarizer's text, so this is the only link.
func TestSummarizerUserPromptOpensWithTheSharedLine(t *testing.T) {
	p, err := loadSummarizerProfiles(summarizerProfilesYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Prompts.User, askOpeningLine) {
		t.Errorf("summarizer user prompt starts %q, want the shared opening line", p.Prompts.User[:60])
	}
}

// CALLER: the summarizer resolves its role through the helper. A promoted model reaches the system
// channel only if roleFor honours the helper's allow-list rule.
func TestSummarizerRoleForUsesTheHelper(t *testing.T) {
	p := &summarizerProfiles{SystemModels: []string{"qwen3"}}
	for _, tc := range []struct {
		id      string
		role    bschemas.ChatMessageRole
		allowed bool
	}{
		{"Qwen/Qwen3.6-27B", bschemas.ChatMessageRoleSystem, true},
		{"claude-opus-5", bschemas.ChatMessageRoleUser, false},
	} {
		role, ok := p.roleFor(tc.id)
		want := resolveAsk(p.SystemModels, tc.id, askViaMessages)
		if role != want.Role || ok != want.SystemAllowed || role != tc.role || ok != tc.allowed {
			t.Errorf("%s: roleFor=(%q,%v) helper=(%q,%v) want=(%q,%v)",
				tc.id, role, ok, want.Role, want.SystemAllowed, tc.role, tc.allowed)
		}
	}
}

// CALLER: the sweep opens its prefix ask with the shared line, followed by the contract.
func TestSweepQuestionOpensWithTheSharedLine(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReqStocked(), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("no ask was made, so the assertion below is vacuous (gates: %v)", rep.Gates)
	}
	ask := asker.ask()
	if !strings.HasPrefix(ask, askOpeningLine+"\n\n") {
		t.Errorf("sweep question does not open with the shared line: %.120q", ask)
	}
	if !strings.Contains(ask, "TOOL OUTPUTS UNDER CONSIDERATION") {
		t.Errorf("the contract and inventory must still follow the line: %.120q", ask)
	}
}
