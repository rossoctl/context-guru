package dash

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Back-filling the project column from Claude Code's own transcripts.
//
// WHY THIS EXISTS. The project dimension was added after the proxy had already recorded thousands
// of requests, and those rows cannot be attributed from anything the dashboard holds: the project
// is read from a request's prompt, and request bodies are not stored (content capture is off by
// default, and `tool_declarations` keeps digests rather than text).
//
// But the agent keeps its own transcript of every session under ~/.claude/projects/, each entry
// carrying an explicit `cwd` and a `timestamp`, keyed by the same session id the dashboard already
// records. So the information is on disk, just not in our database — which makes this a join, not
// a guess.
//
// A DIFFERENT DATA SOURCE, AND DELIBERATELY A ONE-OFF. Everything else in the dashboard is derived
// from traffic that went through the proxy. This reads the user's filesystem, so:
//
//   - It only works where the transcripts are: the user's own machine. A hosted proxy has no
//     access and must not pretend otherwise, which is the same limit skillDescriptions documents.
//   - It runs when asked, never on startup. A migration that silently rewrites historical rows
//     from a source outside the request path is not something to do behind the operator's back.
//
// PER REQUEST, NOT PER SESSION. A session's cwd changes — measured, one session moved 21 times —
// so each request is matched to the directory in force at ITS timestamp, by taking the last
// transcript entry at or before it. Attributing a whole session to its first directory would
// charge one project for work done in another, which is the thing the dimension exists to stop.

// cwdAt is one observation: a session was in this directory at this moment.
type cwdAt struct {
	ms  int64
	dir string
}

// ProjectBackfill is what a back-fill would do, or did.
type ProjectBackfill struct {
	// Sessions the dashboard holds with unattributed rows, and how many of them had a transcript.
	Sessions        int
	SessionsMatched int
	// Rows considered and rows a directory could be found for.
	Rows      int
	RowsFound int
	// Updated is how many rows were actually written; 0 for a dry run.
	Updated int
	// ByProject counts the rows each directory gained, so the caller can show the result before
	// committing to it.
	ByProject map[string]int
	// Unmatched names the sessions with no transcript, because "3,002 of 3,003" needs the 1
	// explained rather than rounded away.
	Unmatched []string
}

// BackfillProjects fills the project column on rows that have none, reading each session's working
// directory out of the agent's transcripts.
//
// `home` overrides the home directory, for tests. `apply` false reports what would change without
// writing, which is the mode worth running first: this rewrites historical rows.
func (d *DB) BackfillProjects(home string, apply bool) (*ProjectBackfill, error) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = h
	}
	out := &ProjectBackfill{ByProject: map[string]int{}}

	// Which sessions need attributing, and over what span. Scoped to rows with no project so a
	// re-run is cheap and never overwrites an attribution the request path already made — the
	// live path is the authority, this is only filling its history.
	rows, err := d.sql.Query(
		`SELECT session_id, COUNT(*) FROM requests WHERE project = '' AND session_id <> ''
		 GROUP BY session_id`)
	if err != nil {
		return nil, err
	}
	need := map[string]int{}
	for rows.Next() {
		var sess string
		var n int
		if err := rows.Scan(&sess, &n); err != nil {
			rows.Close()
			return nil, err
		}
		need[sess] = n
		out.Sessions++
		out.Rows += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(need) == 0 {
		return out, nil
	}

	// The transcripts, indexed by session. One pass over the directory rather than a lookup per
	// session: the file name is the session id, but the directory it sits in is an ENCODED path
	// whose decoding is ambiguous (a dash in a real folder name is indistinguishable from a
	// separator), so the directory name is never parsed — only the `cwd` inside is read.
	marks := map[string][]cwdAt{}
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	for _, f := range files {
		sess := filepath.Base(f)
		sess = sess[:len(sess)-len(".jsonl")]
		if _, wanted := need[sess]; !wanted {
			continue
		}
		m, err := readCwdMarks(f)
		if err != nil || len(m) == 0 {
			continue
		}
		marks[sess] = append(marks[sess], m...)
	}
	for sess := range marks {
		sort.Slice(marks[sess], func(i, j int) bool { return marks[sess][i].ms < marks[sess][j].ms })
		out.SessionsMatched++
	}
	for sess := range need {
		if _, ok := marks[sess]; !ok {
			out.Unmatched = append(out.Unmatched, sess)
		}
	}
	sort.Strings(out.Unmatched)

	// Now walk the unattributed rows and pair each with the directory in force at its timestamp.
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	upd, err := tx.Prepare(`UPDATE requests SET project = ? WHERE id = ? AND project = ''`)
	if err != nil {
		return nil, err
	}
	defer upd.Close()

	reqs, err := tx.Query(
		`SELECT id, session_id, ts FROM requests WHERE project = '' AND session_id <> '' ORDER BY ts`)
	if err != nil {
		return nil, err
	}
	type write struct {
		id  int64
		dir string
	}
	var writes []write
	for reqs.Next() {
		var id, ts int64
		var sess string
		if err := reqs.Scan(&id, &sess, &ts); err != nil {
			reqs.Close()
			return nil, err
		}
		dir := dirAt(marks[sess], ts)
		/* A request whose moment lands in scratch space is filed under the session's HOME instead.
		 * The per-moment answer is accurate and useless for those: an agent running a command in
		 * a temporary directory records it as the cwd, which put 485 requests across thirteen
		 * `/private/tmp/*` entries on a real store. The work belonged to the project either way.
		 *
		 * A move between two REAL directories is still honoured per request, which is what
		 * TestABackfillSplitsASessionThatChangedDirectory pins — that is a genuine change of
		 * project, not a tool call wandering. */
		if dir == "" || isScratchDir(dir) {
			if home := sessionHome(marks[sess]); home != "" {
				dir = home
			}
		}
		if dir == "" {
			continue
		}
		out.RowsFound++
		out.ByProject[dir]++
		writes = append(writes, write{id, dir})
	}
	reqs.Close()
	if err := reqs.Err(); err != nil {
		return nil, err
	}

	if !apply {
		return out, nil
	}
	for _, w := range writes {
		// Sanitized on the way in, exactly as the live write path does: this value came off the
		// filesystem rather than off the wire, but it is still a path somebody else chose.
		//
		// The UPDATE re-states `project = ''` even though the SELECT above already scoped to it.
		// Belt and braces on purpose: the live request path is the authority on a row it
		// attributed, and this reads a DIFFERENT source that can disagree with it — a transcript
		// is per-session while a request is per-moment. One clause is enough today; two mean a
		// future change to either query cannot silently start rewriting correct rows.
		if _, err := upd.Exec(ProjectPath(w.dir), w.id); err != nil {
			return nil, err
		}
		out.Updated++
	}
	return out, tx.Commit()
}

// readCwdMarks pulls every (timestamp, cwd) pair out of one transcript.
//
// Tolerant by design: a transcript is another program's append-only log, it contains entry types
// this code has never heard of, and a partial last line is normal for a session still running.
// A line that does not parse is skipped rather than failing the back-fill.
func readCwdMarks(path string) ([]cwdAt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Transcript lines carry whole tool outputs and run long; the default 64 KiB limit would
	// truncate them and silently drop the entries that follow in the same line.
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var out []cwdAt
	for sc.Scan() {
		var e struct {
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Cwd == "" || e.Timestamp == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			continue
		}
		out = append(out, cwdAt{ms: t.UnixMilli(), dir: e.Cwd})
	}
	// A scanner error mid-file still yields what was read: a truncated transcript should attribute
	// the part it can rather than nothing.
	return out, nil
}

// dirAt is the directory in force at a moment: the last observation at or before it.
//
// Falls back to the FIRST observation for a request that predates every entry, which happens
// because the proxy records a request as it forwards it while the agent writes its transcript
// entry a moment later. The alternative — leaving those rows unattributed — would drop the first
// request of every session, and the session's own first directory is the honest answer for them.
func dirAt(marks []cwdAt, ms int64) string {
	if len(marks) == 0 {
		return ""
	}
	i := sort.Search(len(marks), func(i int) bool { return marks[i].ms > ms })
	if i == 0 {
		return marks[0].dir
	}
	return marks[i-1].dir
}

// sessionHome is the directory a session actually belongs to: the one it spent the most entries
// in, ignoring scratch space.
//
// WHY A SESSION NEEDS A HOME AT ALL. The live request path reads the project from each request's
// own prompt, and that is right for it — the prompt states where the agent IS. A transcript
// records the same thing, and replaying it literally produced a result that was accurate and
// useless: a dry run over real data attributed 2,503 requests to the project and 485 across
// THIRTEEN `/private/tmp/*` directories, because an agent that runs a command in a temporary
// directory records that as its cwd. Thirteen throwaway folders in the project picker make the
// picker worse than no picker.
//
// So a back-fill attributes a request to the session's home rather than to wherever a tool call
// had wandered at that instant. This is a DIFFERENT rule from the live path's, applied only here,
// and the difference is the point: the live path sees one request and must answer from it, while a
// back-fill can see the whole session and knows which of those directories the work was in.
//
// A session with no non-scratch directory keeps its own majority directory, because a session that
// genuinely ran entirely in /tmp has nowhere better to be filed.
func sessionHome(marks []cwdAt) string {
	if len(marks) == 0 {
		return ""
	}
	count := map[string]int{}
	for _, m := range marks {
		count[m.dir]++
	}
	best, bestN := "", -1
	bestAny, bestAnyN := "", -1
	for dir, n := range count {
		if n > bestAnyN || (n == bestAnyN && dir < bestAny) {
			bestAny, bestAnyN = dir, n
		}
		if isScratchDir(dir) {
			continue
		}
		// Ties broken by name so the result does not depend on map iteration order.
		if n > bestN || (n == bestN && dir < best) {
			best, bestN = dir, n
		}
	}
	if best != "" {
		return best
	}
	return bestAny
}

// isScratchDir reports whether a path is temporary space rather than somewhere work lives.
//
// Deliberately a short, explicit list rather than a heuristic: the cost of a false positive is
// filing real work under the wrong project, which is worse than one extra entry in a picker.
func isScratchDir(dir string) bool {
	for _, p := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/dev"} {
		if dir == p || (len(dir) > len(p) && dir[:len(p)] == p && dir[len(p)] == '/') {
			return true
		}
	}
	return false
}
