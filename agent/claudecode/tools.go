package claudecode

import "github.com/tidwall/gjson"

// SubagentTools are the tools whose call starts a subagent. Task is the older name of Agent. The
// call's `prompt` argument becomes the subagent's first role:user message, and its `subagent_type`
// argument (SubagentTypeField) names the kind of subagent.
var SubagentTools = []string{"Agent", "Task"}

// SubagentTypeField is the argument of a SubagentTools call that names the subagent's type.
const SubagentTypeField = "subagent_type"

// BuiltinSubagentTypes are the subagent types that ship with the client. Any other type is a name
// the user chose.
var BuiltinSubagentTypes = []string{"general-purpose", "Explore", "Plan", "statusline-setup", "claude-code-guide"}

// ToolSearch is Claude Code's own client-side tool search tool.
const ToolSearch = "ToolSearch"

// BuiltinTools is Claude Code's own client-side tool set. A name on this list is a tool the client
// ships, so a user cannot remove its declaration without breaking the agent.
//
// The list errs toward EXCLUDING an unrecognised name. A name that merely looks built-in (Workflow
// and ScheduleWakeup on one deployment) can belong to another agent's own tool, and filing it here
// would hide a genuinely removable declaration behind a danger warning. A name that was verified
// against a live captured request, such as DesignSync (Claude Code's claude.ai/design bridge), is
// listed. When a new Claude Code tool appears, add it here.
var BuiltinTools = []string{
	"Agent", "Artifact", "ArtifactCheck", "ArtifactComments",
	"ArtifactData", "AskUserQuestion", "Bash",
	"BashOutput", "CronCreate", "CronDelete", "CronList",
	"DesignSync", "Edit", "EndConversation", "EnterWorktree",
	"ExitPlanMode", "ExitWorktree", "Glob", "Grep",
	"KillShell", "ListAgents", "ListMcpResourcesTool", "LSP",
	"Monitor", "NotebookEdit", "PowerShell", "Read",
	"ReadMcpResourceDirTool", "ReadMcpResourceTool", "SendMessage",
	"SendFeedback", "Skill", "SlashCommand", "TaskCreate",
	"TaskGet", "TaskList", "TaskOutput", "TaskStop",
	"TaskUpdate", "TodoWrite", "ToolSearch", "WebFetch", "WebSearch", "Write",
}

// AskHumanTools are the tools whose call waits on a person, not on the tool itself.
var AskHumanTools = []string{"AskUserQuestion", "ExitPlanMode"}

// QuestionTools are the AskHumanTools that ask a question. The others (ExitPlanMode) ask for an
// approval.
var QuestionTools = []string{"AskUserQuestion"}

// UserID is the `metadata.user_id` of a request body: Claude Code's one per-person id. It is
// a JSON object in a string ({"device_id":...,"session_id":...}); the session package parses it. ""
// when the body has none (any other client).
func UserID(body []byte) string { return gjson.GetBytes(body, "metadata.user_id").String() }
