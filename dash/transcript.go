package dash

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Reading Claude Code's own session transcripts (~/.claude/projects/<encoded-cwd>/<session>.jsonl).
//
// Its one use now is the project picker: projectgroups.go reads each session's working folders from
// here to group them into projects. The history import that also read these files is removed, so no
// figure on the dashboard comes from a transcript.

// TranscriptTurn is one billed request, as another program recorded it.
type TranscriptTurn struct {
	TS        int64 // epoch ms
	SessionID string
	// MessageID is the provider's id for the response. Claude Code writes one transcript line per
	// content block of a streamed message, every one carrying the same usage, so the id is what
	// makes a billed request a single turn instead of one per block. Empty on an entry that has
	// none, which is then taken as its own turn.
	MessageID string
	Model     string
	// Project is the directory the agent was started in, sanitised exactly as a live request's is.
	Project string
	// The four usage counters, named as this package names them rather than as the transcript
	// does, so a caller does not have to know the other program's vocabulary.
	FreshInput int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	// Called is every capability this turn invoked: tool names as sent, and SKILLS BY THEIR OWN
	// NAME rather than by the tool that ran them. See transcriptCalled.
	Called []string
	// Skills is the subset of Called that arrived through the `Skill` tool.
	//
	// CARRIED AS A FACT, not re-derived from the name. The obvious test is a colon —
	// `superpowers:brainstorming` has one — but a marketplace skill does not: `artifact-design`
	// and `dataviz` are both skills and both bare. Guessing from the shape of the name files them
	// as tools, which puts them in the wrong half of the inventory. The transcript already knows,
	// because the invocation came through one specific tool, so the answer is recorded rather
	// than inferred.
	Skills map[string]bool
}

// transcriptEntry is the subset of a transcript line this code reads. Deliberately partial: the
// entries carry a dozen fields that are no business of ours, and naming only what is used means a
// new field upstream cannot break the parse.
type transcriptEntry struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheReadInputToks  int64 `json:"cache_read_input_tokens"`
			CacheCreationInToks int64 `json:"cache_creation_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Skill string `json:"skill"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// transcriptMaxLine is the largest line the reader will take.
//
// A transcript line carries a whole turn, tool outputs included, and routinely runs to hundreds of
// kilobytes. bufio's default 64 KiB would truncate one and then treat the remainder as a fresh
// line, so the entries after a long turn would silently stop parsing — a reader that loses data
// quietly rather than loudly. 16 MiB is past the largest line observed on a real machine by two
// orders of magnitude.
const transcriptMaxLine = 16 << 20

// ReadTranscript returns every billed request in one session transcript.
//
// Errors only when the file cannot be opened. A line that does not parse, an entry type it does
// not know, and a torn final line are all expected states of a log another program is still
// writing, so each is skipped and the rest is returned — a partial read of a live session is the
// correct answer, not a failure.
func ReadTranscript(path string) ([]TranscriptTurn, error) {
	turns, _, err := ReadTranscriptChecked(path)
	return turns, err
}

// ReadTranscriptChecked is ReadTranscript plus whether the read stopped early. A line past
// transcriptMaxLine aborts the scanner, and everything after it is lost; returning that as a flag
// lets the importer say "this file was cut short" rather than quietly undercounting.
//
// ONE TURN PER MESSAGE ID. Claude Code writes a line per content block of a streamed message
// (text, then each tool_use), all with the same message.id and the same usage, milliseconds
// apart. Counting each line billed one request several times: measured on a real transcript,
// 7,680 usage lines for 5,038 distinct ids, cache_read overstated by 52%. The last line of an id
// carries the final usage; Called is the union of the tool_use blocks across all of its lines.
//
// Only a regular file is read. A symlink under the projects directory could point anywhere.
func ReadTranscriptChecked(path string) (turns []TranscriptTurn, truncated bool, err error) {
	if fi, lerr := os.Lstat(path); lerr != nil {
		return nil, false, lerr
	} else if !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), transcriptMaxLine)

	var out []TranscriptTurn
	byID := map[string]int{}
	for sc.Scan() {
		var e transcriptEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type != "assistant" || e.Message == nil {
			continue
		}
		called, skills := transcriptCalled(&e)
		u := e.Message.Usage
		var total int64
		if u != nil {
			total = u.InputTokens + u.CacheReadInputToks + u.CacheCreationInToks + u.OutputTokens
		}
		// A line of a known id may carry only tool_use blocks (no usage yet): its calls still
		// belong to that turn.
		if i, ok := byID[e.Message.ID]; ok && e.Message.ID != "" {
			t := &out[i]
			for _, n := range called {
				if !containsStr(t.Called, n) {
					t.Called = append(t.Called, n)
				}
			}
			for n := range skills {
				t.Skills[n] = true
			}
			if total > 0 {
				t.FreshInput, t.CacheRead = u.InputTokens, u.CacheReadInputToks
				t.CacheWrite, t.Output = u.CacheCreationInToks, u.OutputTokens
			}
			continue
		}
		// NOT A BILLED REQUEST. A turn with no usage at all is synthetic or interrupted, and
		// counting it would inflate the request total with entries that cost nothing.
		if total == 0 {
			continue
		}
		ts, perr := time.Parse(time.RFC3339Nano, e.Timestamp)
		if perr != nil {
			// A turn with no usable timestamp cannot be placed on any axis, and guessing one
			// would put real spend on a day it did not happen.
			continue
		}
		if e.Message.ID != "" {
			byID[e.Message.ID] = len(out)
		}
		out = append(out, TranscriptTurn{
			TS:        ts.UnixMilli(),
			SessionID: e.SessionID,
			MessageID: e.Message.ID,
			Model:     e.Message.Model,
			// Through the same gate a live request's directory goes through: it is a
			// client-chosen string either way, and a folder named after a token must not reach
			// the database from this path either.
			Project:    ProjectPath(e.Cwd),
			FreshInput: u.InputTokens,
			CacheRead:  u.CacheReadInputToks,
			CacheWrite: u.CacheCreationInToks,
			Output:     u.OutputTokens,
			Called:     called,
			Skills:     skills,
		})
	}
	return out, sc.Err() != nil, nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// transcriptCalled lists what a turn invoked, with skills under their own names.
//
// EVERY SKILL IS INVOKED THROUGH ONE TOOL. Claude Code runs them all through a built-in called
// `Skill`, passing the skill's name in `input.skill` — so reading the tool name records ten
// different skills as ten calls to "Skill". That is the mistake the first pass of this analysis
// made, and it matters more here than anywhere: "which of my skills have I ever used" is the
// question this import exists to answer, and collapsing them all into one name answers it wrong
// while looking like it worked.
func transcriptCalled(e *transcriptEntry) ([]string, map[string]bool) {
	var out []string
	seen := map[string]bool{}
	skills := map[string]bool{}
	for _, b := range e.Message.Content {
		if b.Type != "tool_use" || b.Name == "" {
			continue
		}
		name := b.Name
		if name == "Skill" && b.Input.Skill != "" {
			name = b.Input.Skill
			// Recorded here, where it is known for certain, rather than guessed from the name
			// later: a marketplace skill is a bare word and indistinguishable from a tool.
			skills[name] = true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, skills
}
