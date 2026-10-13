package offload

import (
	"strings"

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
		id := strings.ToLower(strings.TrimSpace(modelID))
		if id != "" {
			for _, m := range systemModels {
				if m != "" && strings.Contains(id, strings.ToLower(strings.TrimSpace(m))) {
					p.Role, p.SystemAllowed = bschemas.ChatMessageRoleSystem, true
					break
				}
			}
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
