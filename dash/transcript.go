package dash

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Reading Claude Code's own session transcripts, so the dashboard can report on traffic that
// predates its own installation.
//
// WHY THIS EXISTS. A user installs this proxy after months of working with Claude Code and the
// dashboard starts from zero: every request before install day is invisible, so the first thing
// they see is an empty page and the capability report has no evidence to reason from. Claude Code
// writes a transcript of every session under ~/.claude/projects/<encoded-cwd>/<session>.jsonl,
// and those files exist whether or not this proxy ever ran. Measured on one real machine: 4,053
// requests over 12 days that the proxy never saw, against 3,959 that it did.
//
// THE SOURCE IS DELIBERATELY NOT OURS, and that is the only reason it can answer the question.
// Every other figure in this dashboard comes from traffic that passed through the proxy; a request
// made before the proxy existed can only be recovered from something else's record of it. Three
// consequences follow, and they shape everything below:
//
//   - The format is not ours to rely on. A transcript holds entry types this code has never heard
//     of, and more will appear. Anything unrecognised is skipped, never fatal.
//   - The file may be mid-write. A session still running has a torn last line, which is normal
//     rather than corrupt.
//   - It only exists on the user's own machine. A hosted proxy has nothing to read, the same limit
//     skillDescriptions documents, and must not pretend otherwise.
//
// WHAT IT CANNOT GIVE US, which decides what the import may claim. A transcript records the usage
// the provider billed, the model, the working directory and every tool CALLED — but not the tools
// DECLARED on each request. Measured: a declaration list appears twice in 6,000 entries, as a
// `prompt_snapshot` attachment, not per request. So an imported row can answer "what did this
// cost" and can never answer "what was I carrying and not using", which is the one thing the
// waste report is about. The import informs the inventory; it must never produce a verdict.

// TranscriptTurn is one billed request, as another program recorded it.
type TranscriptTurn struct {
	TS        int64 // epoch ms
	SessionID string
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), transcriptMaxLine)

	var out []TranscriptTurn
	for sc.Scan() {
		var e transcriptEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type != "assistant" || e.Message == nil || e.Message.Usage == nil {
			continue
		}
		u := e.Message.Usage
		total := u.InputTokens + u.CacheReadInputToks + u.CacheCreationInToks + u.OutputTokens
		// NOT A BILLED REQUEST. A turn with no usage at all is synthetic or interrupted, and
		// counting it would inflate the request total with entries that cost nothing — which
		// would then understate the per-request cost of everything else.
		if total == 0 {
			continue
		}
		called, skills := transcriptCalled(&e)
		ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			// A turn with no usable timestamp cannot be placed on any axis, and guessing one
			// would put real spend on a day it did not happen.
			continue
		}
		out = append(out, TranscriptTurn{
			TS:        ts.UnixMilli(),
			SessionID: e.SessionID,
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
	// A scanner error mid-file still yields what was read: half a transcript is worth more than
	// none, and the commonest cause is a line longer than the buffer in a file we do not own.
	return out, nil
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

// TranscriptFiles lists every session transcript on this machine, oldest first.
//
// `home` overrides the home directory, for tests. A missing projects directory is the normal state
// of a machine where Claude Code has not run, not an error worth failing an import over.
func TranscriptFiles(home string) []string {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = h
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	// Oldest first, so a progress report reads chronologically and an interrupted import has
	// covered a contiguous span rather than a scatter.
	sort.Slice(paths, func(i, j int) bool {
		fi, erri := os.Stat(paths[i])
		fj, errj := os.Stat(paths[j])
		if erri != nil || errj != nil {
			return paths[i] < paths[j]
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	return paths
}
