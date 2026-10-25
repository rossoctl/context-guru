package offload

import (
	"strings"
	"sync"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// askOpeningLine is the first line of any question that is APPENDED to a transcript as a user
// message. A model that has just read thousands of tokens of trajectory reads a trailing user turn
// as "the next thing to work on"; this line says it is not. It is the line the summarizer's `user`
// prompt has always opened with (summarizer_model_profiles.yaml), kept in one place so the sweep
// opens its question the same way.
const askOpeningLine = "[OPERATOR INSTRUCTION — this is not a task request from the user]"

// askChannel is how a question reaches the model.
type askChannel int

const (
	// askViaMessages sends [transcript..., question] as a message array through a MessagesModel.
	// Any role the registry allows can be sent.
	askViaMessages askChannel = iota
	// askViaPrefix appends the question to the last SENT body (components.PrefixAsker). That path
	// can only append a trailing USER message, whatever the registry says.
	askViaPrefix
	// askStandalone sends the question as its own request. No transcript precedes it, so there is
	// no momentum to disclaim and no role to choose.
	askStandalone
)

// askPlacement is where a question goes for one model and channel.
type askPlacement struct {
	// Role of the appended question message.
	Role bschemas.ChatMessageRole
	// SystemAllowed reports that the role came from the system_models ALLOW-LIST. False means the
	// registry does not permit a system-role question for this model, which is the state an
	// explicit `instruction_role: system` is refused for.
	SystemAllowed bool
	// Opening is the line that starts the question, or "" when none is needed.
	Opening string
}

// resolveAsk is the one rule for the role and opening of an appended question.
//
// The first substring of systemModels found in modelID (case-insensitive) grants role: system. No
// match means role: user, unconditionally. There is no configurable fallback role: a global switch
// that re-grants `system` to every unmatched model at once is the opposite of an allow-list. See
// summarizer_model_profiles.yaml for the measurement behind the empty list.
func resolveAsk(systemModels []string, modelID string, ch askChannel) askPlacement {
	p := askPlacement{Role: bschemas.ChatMessageRoleUser}
	if ch == askStandalone {
		return p
	}
	if ch == askViaMessages {
		if matchesModelList(systemModels, modelID) {
			p.Role, p.SystemAllowed = bschemas.ChatMessageRoleSystem, true
		}
	}
	// The system channel is authoritative by itself; a user message has to say so in its text.
	if p.Role == bschemas.ChatMessageRoleUser {
		p.Opening = askOpeningLine
	}
	return p
}

// Frame puts the opening line, when there is one, in front of the question text.
func (p askPlacement) Frame(question string) string {
	if p.Opening == "" {
		return question
	}
	return p.Opening + "\n\n" + question
}

// matchesModelList reports whether modelID contains any entry of list, case-insensitive, over an
// ALLOW-list: an empty list or empty id matches nothing.
func matchesModelList(list []string, modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	for _, m := range list {
		if m != "" && strings.Contains(id, strings.ToLower(strings.TrimSpace(m))) {
			return true
		}
	}
	return false
}

var (
	shippedAskProfilesOnce sync.Once
	shippedAskProfiles     *summarizerProfiles
)

// askStripsThinkingShipped is askStripsThinking over the SHIPPED registry, for a caller that has
// no profiles of its own: the sweep's prefix ask (extract_sweep.go) declines on it. It fails open: an unreadable registry means "no strip".
func askStripsThinkingShipped(modelID string, msgs []bschemas.ChatMessage) bool {
	shippedAskProfilesOnce.Do(func() { shippedAskProfiles, _ = loadSummarizerProfiles(summarizerProfilesYAML) })
	if shippedAskProfiles == nil {
		return false
	}
	return askStripsThinking(shippedAskProfiles.ThinkingLastTurnModels, modelID, msgs)
}

// askStripsThinking reports whether appending a user question to msgs would drop thinking blocks
// from the provider's cached prefix: modelID keeps only the LAST turn's thinking (lastTurnOnly is
// the registry's thinking_last_turn_models list) and an earlier message carries a thinking block.
// A caller that gets true must not ask over the cached transcript. Unmatched models are "keep all".
func askStripsThinking(lastTurnOnly []string, modelID string, msgs []bschemas.ChatMessage) bool {
	return matchesModelList(lastTurnOnly, modelID) && thinkingWouldBeStripped(msgs)
}

// hasThinkingBlock reports whether an assistant message carries a thinking block — plain
// (`Reasoning`/a `reasoning.text` detail, bifrost's normalized form of Anthropic's `thinking`)
// or encrypted (`reasoning.encrypted`, bifrost's form of `redacted_thinking`). Either shape is
// what Anthropic's own preservation rule keys on; this component does not need to tell them
// apart; strips/keeps the whole block together.
func hasThinkingBlock(m bschemas.ChatMessage) bool {
	return m.ChatAssistantMessage != nil &&
		(m.Reasoning != nil || len(m.ReasoningDetails) > 0)
}

// thinkingWouldBeStripped reports whether appending a non-tool-result message to msgs would,
// on a last-turn-only model, strip every thinking block before the LAST message from the
// backend's own cached context — see summarizerProfiles.thinkingLastTurnOnly for the citation
// and the live-measured gap this check exists to close. The last message itself is excluded:
// appending AFTER it is exactly the turn whose thinking (if any) survives on either kind of
// model, so it carries no risk of its own.
func thinkingWouldBeStripped(msgs []bschemas.ChatMessage) bool {
	if len(msgs) == 0 {
		return false
	}
	for _, m := range msgs[:len(msgs)-1] {
		if hasThinkingBlock(m) {
			return true
		}
	}
	return false
}
