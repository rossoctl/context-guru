package agent

import (
	"strings"

	"github.com/rossoctl/context-guru/agent/claudecode"
	"github.com/rossoctl/context-guru/agent/codex"
)

// The rules below name tools and injected text that one client sends. They sit here, and in the
// client's own package, so library code asks the Client and names no client itself.
//
// Fallback, for every rule here: a client with no rule of its own gets Claude Code's rule. This is
// what the code did before the rules moved. It is also what a zero Client gets, which is what a
// reader with no request at hand (a stored row, an offline transcript) passes. No source shows
// that Codex, OpenCode, Pi or Bob send different text or tool names, so none has its own rule yet.
// A client gets its own rule only when a source and version support it (CLAUDE.md, Client-specific code).

var (
	claudeSubagentTools = set(claudecode.SubagentTools)
	claudeBuiltinTools  = set(claudecode.BuiltinTools)
	claudeBuiltinTypes  = set(claudecode.BuiltinSubagentTypes)
	claudeAsk           = set(claudecode.AskHumanTools)
	codexAsk            = set(codex.AskHumanTools)
	claudeQuestion      = set(claudecode.QuestionTools)
	codexQuestion       = set(codex.QuestionTools)
	// unknownAsk is for a request whose client is not named: a tool of either client that waits on a
	// person reads as a wait on a person, as it did before the rules moved.
	unknownAsk = set(append(append([]string(nil), claudecode.AskHumanTools...), codex.AskHumanTools...))
)

func set(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// IsSubagentTool reports whether a call to tool starts a subagent. Claude Code: Agent and Task.
func (c Client) IsSubagentTool(tool string) bool { return claudeSubagentTools[tool] }

// SubagentTypeField is the argument of a subagent call that names the subagent's type.
func (c Client) SubagentTypeField() string { return claudecode.SubagentTypeField }

// IsBuiltinSubagentType reports whether t is a subagent type that ships with the client.
func (c Client) IsBuiltinSubagentType(t string) bool { return claudeBuiltinTypes[t] }

// IsBuiltinTool reports whether tool is a tool the client ships, one a user cannot remove without
// breaking the agent. Claude Code's list includes its own ToolSearch.
func (c Client) IsBuiltinTool(tool string) bool { return claudeBuiltinTools[tool] }

// AsksHuman reports whether a call to tool waits on a person, not on the tool. Claude Code:
// AskUserQuestion and ExitPlanMode. Codex: request_user_input. A request whose client is not named
// (no User-Agent, a zero Client) takes both lists. The fallback for any other named client is the
// same union, so its behavior does not change.
func (c Client) AsksHuman(tool string) bool {
	switch c.Name {
	case ClientClaude:
		return claudeAsk[tool]
	case ClientCodex:
		return codexAsk[tool]
	}
	return unknownAsk[tool]
}

// IsQuestionTool reports whether a call to tool asks the person a question, not for an approval. It
// is one of the AsksHuman tools. The fallback is the same as AsksHuman's.
func (c Client) IsQuestionTool(tool string) bool {
	switch c.Name {
	case ClientClaude:
		return claudeQuestion[tool]
	case ClientCodex:
		return codexQuestion[tool]
	}
	return claudeQuestion[tool] || codexQuestion[tool]
}

// UserID is the per-person id the request body carries, or "". Only Claude Code sends it, in
// metadata.user_id. A body with none gives "": the same answer as the fallback for any client.
func (c Client) UserID(body []byte) string { return claudecode.UserID(body) }

// InjectedText is what a client injects into the transcript as if the user had typed it. The
// two lists differ in how they match, on purpose: a prefix marks a whole message as injected, and a
// tag encloses injected content inside a message that may also hold real user text.
type InjectedText struct {
	// Prefixes mark a message that starts with one of them as injected. Match after trimming
	// whitespace, ignoring case.
	Prefixes []string
	// Tags are element names (without the angle brackets) that enclose injected content.
	Tags []string
	// whole matches a message that is injected by its exact shape, not by a prefix.
	whole func(normalized string) bool
}

// Injected is the client's injected-text rule. Claude Code: see claudecode.InjectedPrefixes and
// claudecode.WrapperTags.
func (c Client) Injected() InjectedText {
	return InjectedText{Prefixes: claudecode.InjectedPrefixes, Tags: claudecode.WrapperTags, whole: claudecode.IsSkillReloadNotice}
}

// HasPrefix reports whether text starts, after trimming white space, with a prefix marker, or is
// injected by its exact shape.
func (t InjectedText) HasPrefix(text string) bool {
	n := strings.TrimSpace(text)
	for _, p := range t.Prefixes {
		if len(n) >= len(p) && strings.EqualFold(n[:len(p)], p) {
			return true
		}
	}
	return t.whole != nil && t.whole(n)
}

// HasTag reports whether text holds the opening of an enclosing tag anywhere, case-sensitive.
func (t InjectedText) HasTag(text string) bool {
	for _, tag := range t.Tags {
		if strings.Contains(text, "<"+tag) {
			return true
		}
	}
	return false
}
