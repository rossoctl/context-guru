package apply

import "strings"

// Claude Code can end a tool_result with one or more <system-reminder> blocks. 2.1.241 put
// its context-awareness counter there, on every tool_result (#556):
//
//	<system-reminder>
//	<total_tokens>14989288 tokens left</total_tokens>
//	</system-reminder>
//
// 2.1.290 to 2.1.299 send that counter as a separate role:"system" message instead (#518, see
// proxy/agentcompaction.go), but other trailing reminders still occur: an instruction
// attribution, the TodoWrite nag, an auto-mode notice (0.23% of ~300k captured tool outputs).
//
// Left in the text the components see, such a block made a JSON output not JSON (format and toon
// declined), made two runs of one command differ in the counter (dedup declined), and broke the
// parser extract_llm's code strategy writes. So normalize splits trailing blocks off before the
// pipeline runs and the writeback puts them back unchanged: the model still reads them, and no
// component sees them in a tool output.
//
// Only a tool_result's own text is split. A reminder in a separate text block beside the
// tool_result, or in a plain user message, stays where it is and the components see it. Those
// texts are never written back, so nothing there can be lost or doubled.
const (
	reminderOpen  = "<system-reminder>"
	reminderClose = "</system-reminder>"

	// maxReminderBytes bounds one block. The counter is ~90 bytes and the other Claude Code
	// blocks are under 1 KiB. A larger "block" is output that quotes the tags, and moving it into
	// the tail would hide it from every component.
	maxReminderBytes = 4096
)

// splitTrailingReminders returns text as body+tail, where tail is the run of complete
// <system-reminder> blocks at the very end of text. The whitespace BEFORE the run stays in the
// body and the whitespace AFTER it goes with the tail. Only a TRAILING run moves: a block with
// output after it is part of the output. A text that is nothing but reminders (or blank space
// and reminders) is left whole, since there is no output to compact and an empty body would look
// like a different message to every component. A block over maxReminderBytes, or one with a
// close tag inside it, is not a reminder: the open tag LastIndex found belongs to output that
// quotes the tags, and the split stops there.
//
// body+tail == text always, so writeback can rebuild an unchanged output byte for byte.
//
// The whitespace before the run is OUTPUT, not packaging (#752). The body is what an offloader
// stashes and `expand` gives back, and an output that ends in a newline — a file read does —
// must come back with it. Moving that newline into the tail made every stash of a
// reminder-carrying tool_result one byte short of the output the request held.
// writeTail splices the reminder tail back onto what the components left in a tool message.
// cur is the message's current text. An unchanged message (cur == the body the split produced)
// rebuilds the original text exactly. A rewritten one gets the separator that stood between the
// output and the reminder run back before the tail: that separator is output (#752), so it lives
// in the body — and a rewrite that replaced the body swallowed it, which would glue the reminder
// onto the rewrite's last character. Either way the wire bytes are what they were before #752
// moved the separator into the body: rewritten text + separator + reminder.
func (s slot) writeTail(cur string) string {
	if s.tail == "" {
		return cur
	}
	body := s.preText[:len(s.preText)-len(s.tail)]
	if cur == body {
		return cur + s.tail // unchanged: the original bytes, exactly
	}
	return cur + trailingWhitespace(body) + s.tail
}

// trailingWhitespace returns the run of spaces, tabs and newlines at the end of s.
func trailingWhitespace(s string) string {
	i := len(s)
	for i > 0 && strings.IndexByte(" \t\r\n", s[i-1]) >= 0 {
		i--
	}
	return s[i:]
}

func splitTrailingReminders(text string) (body, tail string) {
	cut := len(text)
	for {
		rest := strings.TrimRight(text[:cut], " \t\r\n")
		if !strings.HasSuffix(rest, reminderClose) {
			break
		}
		open := strings.LastIndex(rest, reminderOpen)
		if open < 0 || len(rest)-open > maxReminderBytes ||
			strings.Contains(rest[open+len(reminderOpen):len(rest)-len(reminderClose)], reminderClose) {
			break
		}
		cut = open
	}
	if cut == len(text) {
		return text, "" // no block at the end: trailing whitespace stays output
	}
	if cut == 0 || strings.TrimSpace(text[:cut]) == "" {
		return text, "" // reminders (and blank space) only: no output to split off
	}
	return text[:cut], text[cut:]
}
