package apply

import "strings"

// Claude Code appends a context-awareness block to the end of every tool_result (#556):
//
//	<system-reminder>
//	<total_tokens>14989288 tokens left</total_tokens>
//	</system-reminder>
//
// Left in the text the components see, it made a JSON output not JSON (format and toon
// declined), made two runs of one command differ in the counter (dedup declined), and broke the
// parser extract_llm's code strategy writes. So normalize splits trailing blocks off before the
// pipeline runs and the writeback puts them back unchanged: the model still reads them, and no
// component ever sees them.
const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"
)

// splitTrailingReminders returns text as body+tail, where tail is the run of complete
// <system-reminder> blocks at the very end of text, with the whitespace around them. Only a
// TRAILING run moves: a block with output after it is part of the output. A text that is
// nothing but reminders is left whole, since there is no output to compact and an empty body
// would look like a different message to every component.
//
// body+tail == text always, so writeback can rebuild an unchanged output byte for byte.
func splitTrailingReminders(text string) (body, tail string) {
	cut := len(text)
	for {
		rest := strings.TrimRight(text[:cut], " \t\r\n")
		if !strings.HasSuffix(rest, reminderClose) {
			break
		}
		open := strings.LastIndex(rest, reminderOpen)
		if open < 0 {
			break
		}
		cut = open
	}
	if cut == len(text) {
		return text, "" // no block at the end: trailing whitespace stays output
	}
	cut = len(strings.TrimRight(text[:cut], " \t\r\n"))
	if cut == 0 {
		return text, ""
	}
	return text[:cut], text[cut:]
}
