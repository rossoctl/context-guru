// Package deferred knows the format of Claude Code's deferred-tools list.
//
// Claude Code defers MCP tools: the `tools` array then carries only the built-ins and a
// placeholder, and the mcp__* names arrive in a message as a header line, one name per line,
// and a blank line. Each such list is a delta, so a request carries one per change and the
// declared set is all of them together.
//
// The format lives here, and not in the dashboard, so a second reader (a filter, a proxy
// component) uses the same header text and the same end rules. A reader that ended a list in
// a different place would count a different set of tools.
package deferred

import (
	"bytes"
	"strings"

	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/tidwall/gjson"
)

// Header starts a list of deferred tools.
//
// Claude Code also sends removals, in wording not yet captured, so a removed server stays in
// the set until the session ends.
const Header = "The following deferred tools are now available via ToolSearch."

// MaxNameLen is the API's own documented limit for a tool name. dash's maxDeclName is this
// constant.
const MaxNameLen = 128

// tagEnds are the escaped-body spellings of a newline then `</system-reminder>`.
var tagEnds = [][]byte{[]byte(`\n` + skills.ReminderEnd), []byte(`\n\u003c/system-reminder\u003e`)}

// Regions returns every deferred list in the RAW (still JSON-escaped) body, each as a
// sub-slice of body. It runs on every request, so it is byte searches only and copies nothing.
// A body without a list returns nil.
//
// A region runs from the header to a blank line (`\n\n` in the escaped body), a closing
// `</system-reminder>` line, or the end of its JSON string, whichever is first. Both JSON
// spellings of the tag count: Claude Code writes it literally, an encoder that escapes HTML
// (Go's does) writes `\u003c/system-reminder\u003e`. Text after a closing tag (a date, another
// reminder) must not reach a caller's digest. budget bounds the bytes returned in total.
func Regions(body []byte, budget int) [][]byte {
	var out [][]byte
	for rest := body; budget > 0; {
		i := bytes.Index(rest, []byte(Header))
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := len(rest)
		for _, blank := range [][]byte{[]byte(`\n\n`), []byte(`\r\n\r\n`)} {
			if j := bytes.Index(rest, blank); j >= 0 && j < end {
				end = j
			}
		}
		for _, tag := range tagEnds {
			if j := bytes.Index(rest, tag); j >= 0 && j < end {
				end = j
			}
		}
		if j := stringEnd(rest); j < end {
			end = j
		}
		end = min(end, budget)
		out = append(out, rest[:end])
		budget, rest = budget-end, rest[end:]
	}
	return out
}

// stringEnd returns the offset of the first unescaped `"` in b (the end of the JSON string
// b starts inside), or len(b). The header line itself holds escaped quotes.
func stringEnd(b []byte) int {
	for off := 0; ; {
		j := bytes.IndexByte(b[off:], '"')
		if j < 0 {
			return len(b)
		}
		i := off + j
		// Escaped when an odd number of backslashes runs up to it.
		k := i
		for k > 0 && b[k-1] == '\\' {
			k--
		}
		if (i-k)%2 == 0 {
			return i
		}
		off = i + 1
	}
}

// Texts returns the text that Claude Code itself sends and that holds a list: a user or system
// message's own text, never a tool result, which holds whatever a command printed. Messages
// without the header are skipped.
func Texts(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		if !strings.Contains(m.Raw, Header) {
			return true
		}
		if r := m.Get("role").String(); r != "user" && r != "system" {
			return true
		}
		c := m.Get("content")
		if c.Type == gjson.String {
			out = append(out, c.String())
			return true
		}
		c.ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "text" {
				out = append(out, blk.Get("text").String())
			}
			return true
		})
		return true
	})
	return out
}

// TextsResponses is Texts for a Responses body: the `instructions` string, and the text of every
// user, system or developer item of `input`. As in Texts, a function_call_output (a tool result)
// is never read.
func TextsResponses(body []byte) []string {
	var out []string
	if ins := gjson.GetBytes(body, "instructions"); ins.Type == gjson.String && strings.Contains(ins.String(), Header) {
		out = append(out, ins.String())
	}
	gjson.GetBytes(body, "input").ForEach(func(_, it gjson.Result) bool {
		if !strings.Contains(it.Raw, Header) {
			return true
		}
		if r := it.Get("role").String(); r != "user" && r != "system" && r != "developer" {
			return true
		}
		c := it.Get("content")
		if c.Type == gjson.String {
			out = append(out, c.String())
			return true
		}
		c.ForEach(func(_, blk gjson.Result) bool {
			if t := blk.Get("type").String(); t == "text" || t == "input_text" {
				out = append(out, blk.Get("text").String())
			}
			return true
		})
		return true
	})
	return out
}

// List is one parsed list: its tool names and the byte range of the list in the text.
type List struct {
	Names      []string
	Start, End int
}

// Lists parses every valid list in text. The header must start a line, and every line up to
// the end of the list must be a bare tool name (ValidName). One line that is not voids that
// whole list, because a user can quote the header. It returns how many lists it voided, so a
// caller can log a format change instead of showing "0 MCP servers".
func Lists(text string) (lists []List, voided int) {
	for from := 0; ; {
		i := strings.Index(text[from:], Header)
		if i < 0 {
			return lists, voided
		}
		start := from + i
		from = start + len(Header)
		if start > 0 && text[start-1] != '\n' {
			continue // quoted mid-line: not a list
		}
		nl := strings.IndexByte(text[start:], '\n')
		if nl < 0 {
			continue
		}
		var names []string
		end, pos, ok := start+nl, start+nl+1, true
		for pos < len(text) {
			line, next := text[pos:], len(text)
			if k := strings.IndexByte(line, '\n'); k >= 0 {
				line, next = line[:k], pos+k+1
			}
			line = strings.TrimSuffix(line, "\r") // a CRLF client: the name must not fail ValidName
			if line == "" || line == skills.ReminderEnd {
				break
			}
			if !ValidName(line) {
				ok = false
				break
			}
			names = append(names, line)
			end, pos = pos+len(line), next
		}
		if !ok || len(names) == 0 {
			voided++
			continue
		}
		lists = append(lists, List{Names: names, Start: start, End: end})
	}
}

// ValidName reports whether s is a bare tool name: the API's own charset and length.
func ValidName(s string) bool {
	if s == "" || len(s) > MaxNameLen {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
