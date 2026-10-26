package dash

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// Importing pre-install history from Claude Code's own transcripts into the requests table.
//
// transcript.go turns a session file into billed turns; this turns those turns into rows a
// dashboard query can reach. Two rules decide whether the result can be trusted at all, and both
// are enforced here rather than hoped for:
//
//  1. AN IMPORTED ROW NEVER OVERLAPS A MEASURED ONE. The sources overlap by construction — a
//     transcript covers the whole session, including the part that ran after the proxy was
//     installed — so importing without a cutoff double-counts every request on the overlapping
//     days, once from each source. The measured row always wins, because it knows what the
//     request declared and the imported one cannot.
//  2. AN IMPORTED ROW CARRIES NO FIGURE THE TRANSCRIPT CANNOT SUPPORT. Every cost on the insights
//     page means "spent carrying this for nothing", which needs the list of tools each request
//     DECLARED. A transcript does not record it. So the declaration fields stay zero: a
//     real-looking price for an inventory nobody observed is the one failure this dashboard
//     cannot afford, since its whole claim is that every number traces to a measurement.

// ImportOptions carries what the import cannot derive for itself.
type ImportOptions struct {
	// Apply writes. False reports what would happen and touches nothing — the mode to run first,
	// since this reads the user's filesystem and can write thousands of rows.
	Apply bool
	// Since, when non-zero, ignores turns older than this (epoch ms). Absent means everything the
	// transcripts hold: a claim like "never used in twelve months" rests on evidence that only a
	// full import provides.
	Since int64
}

// ImportResult is what an import did, or would do.
type ImportResult struct {
	Files int
	// Importable is every turn eligible after the cutoffs; Imported is what was written (0 on a
	// dry run). Kept apart so a dry run can state the size of the job.
	Importable int
	Imported   int
	// Why turns were left out. Reported rather than silently dropped: a skip that is not counted
	// looks like a transcript that was short, and the reader would not know to ask.
	SkippedOverlap int
	SkippedAlready int
	SkippedOld     int
	// Truncated counts transcripts whose read stopped early (a line past the buffer), so a short
	// total is attributable rather than silent.
	Truncated int
	// Called counts every capability invoked across the imported span, skills under their own
	// names. This is the one finding the import can make about capabilities, since the cost of
	// CARRYING them is not recoverable.
	Called map[string]int
	// The span actually covered, as epoch ms, so a caller can say which days are now explained.
	FirstTS int64
	LastTS  int64
	// Projects seen, for the same reason.
	Projects map[string]int
}

// ImportHistory reads every transcript under `home` and writes rows for traffic the proxy never
// measured.
//
// `home` overrides the home directory, for tests. Idempotent: a second run imports nothing,
// because re-running is the normal case — a user accumulates more history every day, and an
// import that duplicates its own rows is one nobody can repeat safely.
func (d *DB) ImportHistory(home string, opt ImportOptions) (*ImportResult, error) {
	out := &ImportResult{Called: map[string]int{}, Projects: map[string]int{}}

	// THE CUTOFF, PER SESSION: the earliest request the proxy measured in that session. A turn at
	// or after it is already known, and better known. It is per session and not global because a
	// global minimum makes one measured request on any project hide every other project's later
	// history for good, and reports it as "the proxy saw these itself".
	measuredFrom := map[string]int64{}
	crows, err := d.sql.Query(
		`SELECT session_id, MIN(ts) FROM requests WHERE source = '' GROUP BY session_id`)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var sess string
		var ts int64
		if err := crows.Scan(&sess, &ts); err != nil {
			crows.Close()
			return nil, err
		}
		measuredFrom[sess] = ts
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}

	// What is already imported, keyed by session and timestamp. A turn's identity across runs is
	// (session, ts of its first line); within a read, message ids already made it one turn.
	already := map[string]bool{}
	rows, err := d.sql.Query(`SELECT session_id, ts FROM requests WHERE source = ?`, SourceImported)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sess string
		var ts int64
		if err := rows.Scan(&sess, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		already[sess+"@"+strconv.FormatInt(ts, 10)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	files := TranscriptFiles(home)
	var batch []*Event
	var uses []importedUse
	for _, f := range files {
		turns, truncated, err := ReadTranscriptChecked(f)
		if err != nil {
			// A transcript that cannot be opened is skipped, not fatal: one unreadable file in a
			// directory of a hundred must not cost the other ninety-nine.
			continue
		}
		out.Files++
		if truncated {
			out.Truncated++
		}
		for _, tn := range turns {
			if opt.Since > 0 && tn.TS < opt.Since {
				out.SkippedOld++
				continue
			}
			// Rule 1. `>=` not `>`: a turn at exactly the measured boundary is the same request
			// the proxy recorded.
			if cut := measuredFrom[tn.SessionID]; cut > 0 && tn.TS >= cut {
				out.SkippedOverlap++
				continue
			}
			isNew := !already[tn.SessionID+"@"+strconv.FormatInt(tn.TS, 10)]
			/* INVOCATIONS ARE RECOMPUTED FROM EVERY ELIGIBLE TURN, new or not. They used to be
			 * collected only for turns not yet imported, which had two faults: a run interrupted
			 * between the requests and the uses left requests that every later run skipped, so
			 * their uses were never written; and a re-run with more history replaced a count with
			 * only the new turns' share. The write is idempotent (see insertImportedUses), so
			 * re-deriving them each run is safe and heals an interrupted one. */
			if opt.Apply {
				for _, name := range tn.Called {
					uses = append(uses, importedUse{
						session: tn.SessionID, name: name, ts: tn.TS,
						isSkill: tn.Skills[name],
						// The turn's own folder, which the transcript records per turn.
						project: tn.Project,
					})
				}
			}
			if !isNew {
				out.SkippedAlready++
				continue
			}
			out.Importable++
			for _, n := range tn.Called {
				out.Called[n]++
			}
			if tn.Project != "" {
				out.Projects[tn.Project]++
			}
			if out.FirstTS == 0 || tn.TS < out.FirstTS {
				out.FirstTS = tn.TS
			}
			if tn.TS > out.LastTS {
				out.LastTS = tn.TS
			}
			if opt.Apply {
				batch = append(batch, importedEvent(tn))
			}
		}
	}

	if !opt.Apply {
		return out, nil
	}
	// In chronological order, so an interrupted write leaves a contiguous span rather than a
	// scatter, and in chunks, because a year of history is tens of thousands of rows and one
	// transaction over all of them holds a write lock for the duration.
	sort.Slice(batch, func(i, j int) bool { return batch[i].TS < batch[j].TS })
	const chunk = 500
	for i := 0; i < len(batch); i += chunk {
		end := i + chunk
		if end > len(batch) {
			end = len(batch)
		}
		if err := d.insertBatch(batch[i:end]); err != nil {
			return nil, err
		}
		out.Imported += end - i
	}
	if err := d.insertImportedUses(uses); err != nil {
		return nil, err
	}
	return out, nil
}

// importedUse is one capability invocation recovered from a transcript.
type importedUse struct {
	session string
	name    string
	ts      int64
	// isSkill comes from the transcript, which saw the invocation go through the `Skill` tool.
	// Not derived from the name: a marketplace skill is a bare word like `artifact-design` and
	// looks exactly like a tool.
	isSkill bool
	// project is the folder this invocation happened in. Carried per-invocation rather than
	// per-session because ONE SESSION WORKS IN SEVERAL FOLDERS — measured, one spanned 14 — and
	// a per-session folder is exactly the approximation that made every project report the same
	// usage figures.
	project string
}

// insertImportedUses records recovered invocations in tool_uses.
//
// WRITTEN AS AN ABSOLUTE COUNT (never lowered), not accumulated. The live path upserts with
// `calls = calls + excluded.calls`, which is right for it: each batch carries new traffic. An
// import re-reads the SAME transcripts every run, so accumulating would make a capability look
// twice as used after a second run — and a user who re-runs an import to pick up last week's
// history would silently inflate every figure that run touched.
//
// Skills and plain tools are told apart the way the inventory does it: a skill's name goes in the
// `skill` column, a tool's in `name`. Reading a transcript, the distinction is whether the
// invocation came through the `Skill` tool — see transcriptCalled.
func (d *DB) insertImportedUses(uses []importedUse) error {
	if len(uses) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertImportedUsesTx(tx, uses); err != nil {
		return err
	}
	return tx.Commit()
}

// insertImportedUsesTx is insertImportedUses inside the caller's transaction, so a caller that
// deletes first can make the delete and the re-insert one atomic step.
func insertImportedUsesTx(tx *sql.Tx, uses []importedUse) error {
	if len(uses) == 0 {
		return nil
	}
	// Collapsed per (session, capability) first, because tool_uses holds one accumulated row per
	// pair and a session invokes the same capability many times.
	// KEYED BY FOLDER TOO, matching the table's primary key: the same capability invoked in two
	// folders in one session is two facts, and collapsing them is the defect this fixes.
	type key struct{ session, project, name string }
	type acc struct {
		calls       int
		first, last int64
		// isSkill comes from the transcript, which saw the invocation go through the `Skill`
		// tool. Not derived from the name: a marketplace skill is a bare word like
		// `artifact-design` and is indistinguishable from a tool by shape alone.
		isSkill bool
	}
	agg := map[key]*acc{}
	for _, u := range uses {
		k := key{u.session, u.project, u.name}
		a := agg[k]
		if a == nil {
			a = &acc{}
			agg[k] = a
		}
		a.calls++
		if u.isSkill {
			a.isSkill = true
		}
		if a.first == 0 || u.ts < a.first {
			a.first = u.ts
		}
		if u.ts > a.last {
			a.last = u.ts
		}
	}

	stmt, err := tx.Prepare(`INSERT INTO tool_uses(
		tenant_id, session_id, name, server, skill, calls, first_ts, last_ts, project
	) VALUES ('',?,?,'',?,?,?,?,?)
	ON CONFLICT(tenant_id, session_id, project, name, skill) DO UPDATE SET
		calls = MAX(calls, excluded.calls),
		first_ts = MIN(first_ts, excluded.first_ts),
		last_ts = MAX(last_ts, excluded.last_ts)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, a := range agg {
		// A capability whose name carries a colon came through the Skill tool; anything else is a
		// tool or an MCP tool, which the inventory keys by `name`.
		toolName, skillName := k.name, ""
		if a.isSkill {
			toolName, skillName = "Skill", k.name
		}
		if _, err := stmt.Exec(k.session, toolName, skillName,
			a.calls, a.first, a.last, k.project); err != nil {
			return err
		}
	}
	return nil
}

// importedEvent is one transcript turn as a request row.
//
// WHAT IT DELIBERATELY LEAVES EMPTY is the point of this function. Tools, FilteredDeclTokens and
// BaselineCostUSD all stay zero, because each describes what the request DECLARED or what it
// WOULD have cost — and a transcript records neither. Filling them from the usage figures would
// be inventing an inventory, and this page's entire credibility rests on not doing that.
func importedEvent(tn TranscriptTurn) *Event {
	return &Event{
		TS:        tn.TS,
		SessionID: tn.SessionID,
		Model:     tn.Model,
		Project:   tn.Project,
		Source:    SourceImported,
		// The usage the provider billed, which is the one thing a transcript states outright.
		FreshInput:   tn.FreshInput,
		CacheRead:    tn.CacheRead,
		CacheWrite:   tn.CacheWrite,
		OutputTokens: tn.Output,
		// The agent is known from the source itself: these files are Claude Code's own.
		Agent: "claude-code",
	}
}

// NeverCalled is the capabilities installed on this machine that no transcript shows being used.
//
// THE FINDING THIS IMPORT EXISTS FOR, and the one it can make honestly. The cost of carrying an
// unused skill is not recoverable from a transcript — but WHETHER it was ever used is, and so is
// the inventory on disk. "You have 46 skills installed and have used 10 of them" needs no
// inference and is available the day the proxy is installed.
//
// `home` overrides the home directory, for tests.
func NeverCalled(home string, called map[string]int) []string {
	installed := installedSkills(home)
	var out []string
	for name := range installed {
		if called[name] == 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// installedSkills lists the skills on disk, keyed as the inventory names them.
//
// Reuses the layout skillDescriptions already walks: plugin skills under the plugin cache keyed
// <plugin>:<name>, marketplace skills under the marketplace directory keyed bare. Built-in skills
// have no file anywhere, which is why this cannot be a complete inventory and is not presented as
// one — it answers "what did I install", not "what is in the prompt".
func installedSkills(home string) map[string]bool {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return map[string]bool{}
		}
		home = h
	}
	out := map[string]bool{}
	base := filepath.Join(home, ".claude", "plugins")
	cache := filepath.Join(base, "cache")
	for _, mkt := range dirNames(cache) {
		for _, plug := range dirNames(filepath.Join(cache, mkt)) {
			for _, ver := range dirNames(filepath.Join(cache, mkt, plug)) {
				for _, name := range dirNames(filepath.Join(cache, mkt, plug, ver, "skills")) {
					out[plug+":"+name] = true
				}
			}
		}
	}
	markets := filepath.Join(base, "marketplaces")
	for _, mkt := range dirNames(markets) {
		for _, name := range dirNames(filepath.Join(markets, mkt, "skills")) {
			if !out[name] {
				out[name] = true
			}
		}
	}
	return out
}
