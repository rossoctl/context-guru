package dash

// How a user actually gets rid of a capability they are paying to carry, and which of the
// things they carry are safe to get rid of at all.
//
// The Inventory tab could always say "this costs you 4,200 tokens a request and you have never
// once invoked it". It could not say what to DO about that, so the reader was left to guess —
// and the guesses are all wrong in the same expensive way. The mechanisms below were verified
// against current Claude Code behaviour rather than inferred, because a confidently wrong
// command in a dashboard is worse than no command: it gets pasted.
//
// THE ONE SEMANTIC EVERYTHING HERE RESTS ON. In `permissions.deny`, a BARE tool name
// ("WebSearch") removes the tool from the model's context entirely — which is the token saving
// this page is about. A SCOPED rule ("Bash(rm *)", "Skill(deploy *)") leaves the declaration
// in the prompt and only blocks the call, so it saves nothing at all. Every snippet this file
// emits is therefore the bare form, and the distinction is stated to the reader rather than
// assumed, because "I denied it and my prompt did not shrink" is the obvious next bug report.
//
// Two consequences worth keeping in view:
//
//   - `disallowedTools` is NOT a settings.json key. It exists as a CLI flag, an Agent SDK
//     option and subagent/skill frontmatter. The settings.json form is `permissions.deny`.
//     Emitting a top-level "disallowedTools" block would be a silent no-op.
//   - Denying a skill with `Skill(name)` does not shrink the prompt either: the skill stays in
//     the "following skills are available" listing. Skills need `skillOverrides` or the
//     plugin turned off, so they get their own mechanism rather than the tool one.

// builtinTools is Claude Code's own client-side tool set.
//
// It exists because the stored taxonomy cannot tell these apart from a user's own tools: both
// are KindTool ("a plain client-side tool — Claude Code's built-ins, an agent's own"), so an
// allowlist applied on READ is the only classification available without re-capturing history.
// That is also why it is a list of names and not a heuristic: a heuristic that guessed wrong
// would either bury a removable MCP tool among the built-ins or invite somebody to delete Read.
//
// The names are the CANONICAL ones a permission rule matches, which are not always the labels
// shown in a transcript. Two corrections worth recording, because both are easy to get wrong:
// there is no `Task` tool — the subagent spawner is `Agent`, and the task-list tools are
// TaskCreate/TaskGet/TaskList/TaskOutput/TaskStop/TaskUpdate; and the tool displayed as
// "Stop Task" is `TaskStop`. A rule written as a display label silently never matches.
// HOW THIS IS KEPT CURRENT, because a wrong classification here gets pasted: every name is one
// CLAUDE CODE itself declares, checked against a live session rather than harvested from
// captured traffic. That distinction is the whole discipline of this list. A dashboard's
// tool_declarations table also holds the tools of every OTHER agent that has been through the
// proxy, and a name that merely looks built-in (DesignSync, Workflow, ScheduleWakeup on this
// deployment) belongs to one of those — filing it here would hide a genuinely removable
// declaration behind the danger warning and defeat the page.
//
// So the list errs toward EXCLUDING an unrecognised name. The cost of a false negative is that
// one tool appears in the actionable list without a warning; the cost of a false positive is
// that the page stops finding the savings it exists to find. When a new Claude Code tool
// appears, add it here — a name absent from this list is reported as `client_tool`, which says
// "check what declares this" rather than either "safe to remove" or "do not touch".
var builtinTools = map[string]bool{
	"Agent": true, "Artifact": true, "AskUserQuestion": true, "Bash": true,
	"BashOutput": true, "CronCreate": true, "CronDelete": true, "CronList": true,
	"Edit": true, "EndConversation": true, "EnterWorktree": true, "ExitPlanMode": true,
	"ExitWorktree": true, "Glob": true, "Grep": true, "KillShell": true, "LSP": true,
	"Monitor": true, "NotebookEdit": true, "PowerShell": true, "Read": true,
	"SendMessage": true, "Skill": true, "SlashCommand": true, "TaskCreate": true,
	"TaskGet": true, "TaskList": true, "TaskOutput": true, "TaskStop": true,
	"TaskUpdate": true, "TodoWrite": true, "WebFetch": true, "WebSearch": true, "Write": true,
}

// IsBuiltinTool reports whether a KindTool declaration is one of Claude Code's own.
func IsBuiltinTool(kind, name string) bool { return kind == KindTool && builtinTools[name] }

// Removal is how to stop carrying one declared capability, in the form the user can act on.
type Removal struct {
	// Kind is the mechanism class, for the UI to group and style by:
	// mcp_server | mcp_tool | builtin | skill | plugin_skill | provider | unknown.
	Kind string `json:"kind"`
	// Command is a shell command to run, or "" when the change is a config edit only.
	Command string `json:"command,omitempty"`
	// Settings is a settings.json fragment to merge, or "" when a command is enough.
	Settings string `json:"settings,omitempty"`
	// SettingsPath is where that fragment goes.
	SettingsPath string `json:"settings_path,omitempty"`
	// Effect says whether doing this actually shrinks the prompt, which is the only reason
	// this page exists. Some perfectly valid ways to block a capability save nothing.
	Effect string `json:"effect"`
	// Note carries the caveat for this specific kind. Never empty for a kind that has one.
	Note string `json:"note,omitempty"`
	// Danger marks a capability whose removal will break the agent. Built-ins only.
	Danger bool `json:"danger,omitempty"`
}

// userSettings is where a rule can always be added: list-valued permission keys MERGE across
// every settings file, so a deny written at user scope takes effect regardless of what a
// project file says.
const userSettings = "~/.claude/settings.json"

// RemovalFor returns how to stop carrying one declared capability.
//
// server is the MCP server half of an mcp__ name, already split out by the caller.
func RemovalFor(kind, name, server string) Removal {
	switch {
	case kind == KindMCPTool:
		// A plugin-bundled server's tools are named mcp__plugin_<plugin>_<server>__<tool>, so
		// the "server" half here can be a synthesised plugin path rather than a server a user
		// ever typed. `claude mcp remove` takes the name the user ADDED, which for a bundled
		// server is not this string — so the server-level command is offered only when the
		// name does not look plugin-bundled, and the tool-level deny (which matches the wire
		// name exactly, whatever its shape) is always safe.
		if isPluginServer(server) {
			return Removal{
				Kind:         "mcp_tool",
				Settings:     denySnippet(name),
				SettingsPath: userSettings,
				Effect:       "Removes this tool from the prompt entirely.",
				Note: "This tool comes from a PLUGIN-bundled MCP server (" + server + "), so " +
					"`claude mcp remove` does not apply — the server was never added by hand. To " +
					"drop the whole server at once, disable its plugin, or deny `mcp__" + server +
					"__*`.",
			}
		}
		return Removal{
			Kind:         "mcp_tool",
			Command:      "claude mcp remove " + server,
			Settings:     denySnippet(name),
			SettingsPath: userSettings,
			Effect: "The command removes the whole `" + server + "` server and every tool it " +
				"declares. The settings fragment removes just this one tool and keeps the rest.",
			Note: "`claude mcp remove` takes an optional `-s local|user|project`; with no scope it " +
				"removes the server from whichever scope it is defined in. There is no " +
				"`claude mcp disable` — to keep the config and turn the server off, use the " +
				"`/mcp` panel.",
		}
	case kind == KindSkill:
		// A plugin skill is named "<plugin>:<skill>", and skillOverrides does NOT apply to it.
		if plugin, _, ok := splitPluginSkill(name); ok {
			return Removal{
				Kind:    "plugin_skill",
				Command: "claude plugin disable " + plugin,
				Effect: "Removes every skill and MCP server this plugin contributes, so the " +
					"listing entry stops being sent.",
				Note: "`skillOverrides` does not work on a plugin's skills — the plugin is the " +
					"unit. Reloading is needed for it to take effect: `/reload-plugins`, or " +
					"restart. To remove it altogether rather than switch it off, " +
					"`claude plugin uninstall " + plugin + "`.",
			}
		}
		return Removal{
			Kind:         "skill",
			Settings:     "{\n  \"skillOverrides\": {\n    " + jsonString(name) + ": \"off\"\n  }\n}",
			SettingsPath: userSettings,
			Effect:       "Hides the skill from the listing, so its entry stops being sent.",
			Note: "Denying a skill with `Skill(" + name + ")` instead would NOT save anything — a " +
				"scoped rule blocks the call but leaves the entry in the prompt. Deleting " +
				"`~/.claude/skills/" + name + "/` also works and is permanent.",
		}
	case name == "EndConversation":
		// Documented exception: a deny rule cannot remove this one while any other tool
		// remains, so the usual "removes it from the prompt" promise would be false.
		return Removal{
			Kind:   "builtin",
			Effect: "Cannot be removed while any other tool remains.",
			Danger: true,
			Note: "Claude Code refuses to drop this tool unless it is the last one, so a deny " +
				"rule for it saves nothing. Its weight is unavoidable.",
		}
	case IsBuiltinTool(kind, name):
		return Removal{
			Kind:         "builtin",
			Command:      "claude --disallowedTools \"" + name + "\"",
			Settings:     denySnippet(name),
			SettingsPath: userSettings,
			Effect:       "Removes the tool from the prompt entirely.",
			Danger:       true,
			Note: "This is one of Claude Code's OWN tools. Removing it does not degrade the agent " +
				"gracefully — it takes away a capability the model is expected to have, and " +
				"anything that depended on it fails. The token saving is real and it is almost " +
				"never worth it.",
		}
	case kind == KindTool:
		// Not one of Claude Code's built-ins, but still a client-side tool: some other agent or
		// SDK application declared it. Same mechanism, different kind, because the UI must not
		// file it under "built-ins, do not touch" — it is very likely the most removable thing
		// on the page.
		return Removal{
			Kind:         "client_tool",
			Command:      "claude --disallowedTools \"" + name + "\"",
			Settings:     denySnippet(name),
			SettingsPath: userSettings,
			Effect:       "Removes the tool from the prompt entirely.",
			Note: "This is a client-side tool that is not one of Claude Code's built-ins, so it " +
				"comes from whatever agent sent the request. Check what declares it before " +
				"removing it.",
		}
	case kind == KindServerTool:
		return Removal{
			Kind:   "provider",
			Effect: "Not removable from Claude Code settings.",
			Note: "This is a provider-side tool, declared by the application making the request " +
				"rather than by Claude Code. `web_search` and `web_fetch` map to Claude Code's " +
				"own WebSearch/WebFetch and can be denied by those names; `code_execution` and " +
				"`memory` are Claude API server tools with no Claude Code setting at all. If you " +
				"see them here, the request came from an API or SDK application.",
		}
	}
	return Removal{Kind: "unknown", Effect: "No known removal mechanism for this kind."}
}

// denySnippet is the permissions.deny fragment, with the BARE tool name — the form that
// actually removes the declaration from the prompt.
func denySnippet(name string) string {
	return "{\n  \"permissions\": {\n    \"deny\": [" + jsonString(name) + "]\n  }\n}"
}

// jsonString quotes a name for embedding in the snippets above. Hand-rolled rather than
// encoding/json because these are single short identifiers and the output has to sit inside a
// hand-formatted fragment; it escapes the two characters that can appear in a tool name and
// would break the JSON.
func jsonString(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(append(out, '"'))
}

// isPluginServer reports whether an MCP server name looks like a plugin-bundled one, whose
// wire form is mcp__plugin_<plugin>_<server>__<tool>. Such a server was never added by hand,
// so `claude mcp remove` has no name to take.
func isPluginServer(server string) bool {
	const p = "plugin_"
	return len(server) > len(p) && server[:len(p)] == p
}

// splitPluginSkill splits a "<plugin>:<skill>" skill name. A personal or project skill has no
// colon and is managed by skillOverrides; a plugin's skill is managed by its plugin.
func splitPluginSkill(name string) (plugin, skill string, ok bool) {
	for i := 0; i < len(name); i++ {
		if name[i] == ':' {
			if i == 0 || i == len(name)-1 {
				return "", "", false
			}
			return name[:i], name[i+1:], true
		}
	}
	return "", "", false
}
