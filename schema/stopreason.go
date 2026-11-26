package schema

// ToolStopReasons are the stop reasons that say "the response called a tool", in each provider's own
// words: Anthropic tool_use, OpenAI Chat tool_calls, and function_call (the older Chat form). A
// Responses response has no such reason. It carries its calls as output items.
//
// This is the one list of the words. Each reader decides what else it counts for itself, and says
// why: kvcache also treats stop_sequence, length and content_filter as "still working".
var ToolStopReasons = []string{"tool_use", "tool_calls", "function_call"}

// IsToolStopReason reports whether a stop reason says the response called a tool.
func IsToolStopReason(reason string) bool {
	for _, r := range ToolStopReasons {
		if r == reason {
			return true
		}
	}
	return false
}
