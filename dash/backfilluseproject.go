package dash

// Repairing the folder on invocations imported before `tool_uses.project` existed.
//
// WHY THIS IS NOT A RE-IMPORT. ImportHistory dedupes against the `requests` rows it already
// wrote, so re-running it cannot double-count spend — and that same dedupe skips exactly the
// turns whose invocations need a folder. Measured on a live store after the migration: all 77
// invocation rows, covering 5,379 imported requests, carried project='' and a re-import skipped
// every one of them, leaving every per-project list empty.
//
// So this re-reads the transcripts and writes to `tool_uses` ALONE. It never touches `requests`,
// because the one thing a repair must not do is change what the history says was spent.

// BackfillUseProjects fills in the folder on imported invocation rows, returning how many
// invocations it re-attributed (or, when apply is false, would). Dry by default at every call
// site: nothing is written unless apply is true.
//
// `home` overrides the home directory, for tests. Idempotent: it recomputes each row from the
// transcripts and writes the same answer every time, so an operator can run it after any upgrade
// without having to know whether it has run before.
func (d *DB) BackfillUseProjects(home string, apply bool) (int, error) {
	// WHICH SESSIONS HAVE IMPORTED ROWS, and from when each one's measured traffic begins.
	//
	// THE UNIT IS THE INVOCATION, NOT THE SESSION. An earlier rule here skipped any session that
	// held a measured row, reasoning that the measured side must win. It does — but a session
	// routinely holds both, because the proxy gets installed part-way through one, and on the
	// live store that rule left a session of 1,053 requests unattributed to protect the 17 of
	// them that were measured. The project it belonged to then reported ZERO capabilities used
	// across 1,067 requests of real work.
	//
	// So each session carries its own cutoff: its earliest MEASURED request. A turn before that
	// is history the transcript is the only witness to, and repairing it takes nothing from the
	// measured side. A turn at or after it is traffic the proxy watched, and the transcript must
	// not speak for it. This is the same boundary ImportHistory uses to decide which turns to
	// import at all — see rule 1 there — applied to invocations.
	//
	// A session with no measured requests gets a cutoff of 0, meaning "no measured traffic, so
	// every turn is fair game".
	measuredFrom := map[string]int64{}
	rows, err := d.sql.Query(`SELECT session_id,
		SUM(CASE WHEN source = ? THEN 1 ELSE 0 END) AS imported_rows,
		COALESCE(MIN(CASE WHEN source = '' THEN ts END), 0) AS first_measured
		FROM requests GROUP BY session_id HAVING imported_rows > 0`, SourceImported)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var s string
		var impRows, firstMeasured int64
		if err := rows.Scan(&s, &impRows, &firstMeasured); err != nil {
			rows.Close()
			return 0, err
		}
		measuredFrom[s] = firstMeasured
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(measuredFrom) == 0 {
		return 0, nil
	}

	// Rebuild the invocations from the transcripts, exactly as the importer does — same per-turn
	// folder, same skill attribution — so the repaired rows are indistinguishable from rows a
	// fresh import would have written.
	var uses []importedUse
	for _, f := range TranscriptFiles(home) {
		turns, err := ReadTranscript(f)
		if err != nil {
			continue
		}
		for _, tn := range turns {
			cutoff, known := measuredFrom[tn.SessionID]
			if !known {
				continue // no imported rows here: nothing for this to repair
			}
			// `>=` not `>`: a turn at exactly the boundary is the request the proxy recorded.
			if cutoff > 0 && tn.TS >= cutoff {
				continue
			}
			for _, name := range tn.Called {
				uses = append(uses, importedUse{
					session: tn.SessionID, name: name, ts: tn.TS,
					isSkill: tn.Skills[name], project: tn.Project,
				})
			}
		}
	}
	if len(uses) == 0 {
		return 0, nil
	}

	// THE STALE UNATTRIBUTED ROWS GO FIRST, but ONLY the ones this repair replaces.
	//
	// insertImportedUses writes an absolute call count keyed by (session, project, name, skill),
	// so a row still keyed with project='' would survive alongside its repaired twin and
	// double-count that capability in the unscoped view.
	//
	// NARROWED TO THE CAPABILITIES BEING REWRITTEN, not every unattributed row in the session. A
	// mixed session also holds MEASURED invocations with no folder — the live path wrote them
	// before the column existed — and deleting those would destroy counts the proxy observed
	// itself, replacing them with whatever the transcript happens to say. Observed doing exactly
	// that: a measured row of 99 calls was replaced by a transcript-derived 1.
	if !apply {
		return len(uses), nil
	}
	type pair struct{ session, name, skill string }
	clear := map[pair]bool{}
	for _, u := range uses {
		name, skill := u.name, ""
		if u.isSkill {
			name, skill = "Skill", u.name
		}
		clear[pair{u.session, name, skill}] = true
	}
	// ONE TRANSACTION for the delete and the re-insert: a crash or lock error between two of them
	// used to lose the rows.
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	del, err := tx.Prepare(`DELETE FROM tool_uses
		WHERE project = '' AND session_id = ? AND name = ? AND skill = ?`)
	if err != nil {
		return 0, err
	}
	defer del.Close()
	for p := range clear {
		if _, err := del.Exec(p.session, p.name, p.skill); err != nil {
			return 0, err
		}
	}
	if err := insertImportedUsesTx(tx, uses); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(uses), nil
}
