package schema

import (
	"strings"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// The agent's launch directory, read out of the request body.
//
// WHY THIS EXISTS. Several projects route through one proxy, so every cost the dashboard reports
// is a blend across them unless each request says which folder it came from. Nothing else in the
// request identifies a project: the session id is a hash of the conversation head, and one session
// can span directories.
//
// WHERE IT COMES FROM. Claude Code states it in the prompt it sends, as a line reading
// `Primary working directory: /abs/path`. That is a CLIENT CONVENTION, not a protocol guarantee —
// a different agent, or a future version of this one, simply will not carry it, and then this
// returns "" and the request is recorded as unattributed. That is the correct degradation: an
// invented project is worse than an absent one.

// wdMarker is the line prefix Claude Code writes. Held as a constant because it is the single
// point of coupling to another program's prompt format.
const wdMarker = "Primary working directory: "

// wdMaxLen caps what may be extracted. A path is client-chosen free text of unbounded length and
// it reaches a database column and an event feed, so the limit is a storage property. Generous
// enough for any real nesting: the deepest directory measured in real traffic is 94 characters.
const wdMaxLen = 512

// WorkingDirOf returns the agent's launch directory, or "" when the request does not state one.
//
// Scans every message rather than indexing the one the marker occupies today (messages[1]): a host
// may inject a message ahead of it and the environment block sometimes rides in the system prompt,
// and a dimension that silently stops recording when a client reorders its preamble is worse than
// one pass over an already-normalized slice.
//
// The FIRST marker wins, because a transcript accumulates. Later turns quote other directories —
// this function's own test file does — and the last match would attribute a request to whatever
// folder was most recently mentioned in conversation.
func WorkingDirOf(msgs []schemas.ChatMessage) string {
	for _, m := range msgs {
		text := MessageText(m)
		if text == "" {
			continue
		}
		at := strings.Index(text, wdMarker)
		if at < 0 {
			continue
		}
		line := text[at+len(wdMarker):]
		if cut := strings.IndexAny(line, "\r\n"); cut >= 0 {
			line = line[:cut]
		}
		if dir := cleanWorkingDir(line); dir != "" {
			return dir
		}
	}
	return ""
}

// cleanWorkingDir turns the rest of a marker line into a project key, or "" if it is not one.
//
// Two transformations and one refusal, each for a measured reason:
//
//   - The ` (was /old/path)` suffix is STRIPPED. Claude Code appends it when the working directory
//     changes mid-session, and it is common rather than rare — 57 occurrences for a single folder
//     in the traffic measured. Kept, it would make `/a (was /b)` and `/a` two different projects,
//     so one folder would fragment across the tables.
//   - A trailing slash is removed, so `/a/b/` and `/a/b` are one key rather than two.
//   - Anything that is not an absolute path is REFUSED. The marker also appears inside prose and
//     inside a table of instructions, which is not hypothetical: of 589 occurrences measured, 47
//     were text like `  no  Contents of /  YES  # Environment` or a bare `[^`. A parser that
//     trusts the marker records those as projects, and they would appear in the picker as real
//     folders. Requiring a leading `/` and no interior whitespace rejected all 47 and accepted
//     all 542 real paths.
//
// The whitespace rule has a known cost: a directory whose name contains a space is refused and the
// request is recorded as unattributed. That is the safe direction — a missing project is visibly
// missing, while a garbage project silently pollutes every per-project figure — and no such path
// occurs in the traffic measured.
func cleanWorkingDir(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " (was "); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" || s == "/" {
		return s
	}
	if !strings.HasPrefix(s, "/") || len(s) > wdMaxLen {
		return ""
	}
	if strings.ContainsAny(s, " \t") {
		return ""
	}
	return strings.TrimRight(s, "/")
}
