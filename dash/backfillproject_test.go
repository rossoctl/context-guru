package dash

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript builds a fake ~/.claude/projects/<enc>/<session>.jsonl from (offset, cwd) pairs.
func writeTranscript(t *testing.T, home, enc, sess string, base time.Time, marks [][2]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", enc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, m := range marks {
		off := time.Duration(m[0].(int)) * time.Millisecond
		line, _ := json.Marshal(map[string]any{
			"sessionId": sess,
			"cwd":       m[1].(string),
			"timestamp": base.Add(off).UTC().Format(time.RFC3339Nano),
		})
		body = append(body, line...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, sess+".jsonl"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The core claim: rows recorded before the project column existed can be attributed afterwards,
// because the agent's own transcript holds the directory and is keyed by the same session id.
func TestABackfillAttributesRowsRecordedBeforeTheColumnExisted(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	if err := db.insertBatch([]*Event{
		{TS: base.Add(10 * time.Millisecond).UnixMilli(), SessionID: "s-alpha", Model: "m"},
		{TS: base.Add(20 * time.Millisecond).UnixMilli(), SessionID: "s-alpha", Model: "m"},
		{TS: base.Add(30 * time.Millisecond).UnixMilli(), SessionID: "s-beta", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, "-Users-v-alpha", "s-alpha", base, [][2]any{{0, "/Users/v/alpha"}})
	writeTranscript(t, home, "-Users-v-beta", "s-beta", base, [][2]any{{0, "/Users/v/beta"}})

	// A dry run must change nothing, which is what makes it safe to offer first.
	dry, err := db.BackfillProjects(home, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Updated != 0 {
		t.Errorf("a dry run wrote %d rows; it must write none", dry.Updated)
	}
	if dry.RowsFound != 3 {
		t.Errorf("dry run found %d attributable rows; want 3", dry.RowsFound)
	}
	var still int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE project <> ''`).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 0 {
		t.Fatalf("a dry run attributed %d rows on disk; it must be read-only", still)
	}

	got, err := db.BackfillProjects(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Updated != 3 {
		t.Errorf("applied run updated %d rows; want 3", got.Updated)
	}
	for _, tc := range []struct {
		project string
		want    int
	}{{"/Users/v/alpha", 2}, {"/Users/v/beta", 1}} {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE project = ?`,
			tc.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != tc.want {
			t.Errorf("project %s has %d rows; want %d", tc.project, n, tc.want)
		}
	}
}

// PER REQUEST, NOT PER SESSION — the same rule the live path follows, and the reason the
// transcript's timestamps are read rather than just its directory name.
//
// A session that moves directory must have its requests split at the move. Attributing the whole
// session to its first directory would charge one project for work done in another.
func TestABackfillSplitsASessionThatChangedDirectory(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)

	// Two requests before the move, two after.
	if err := db.insertBatch([]*Event{
		{TS: base.Add(100 * time.Millisecond).UnixMilli(), SessionID: "s-move", Model: "m"},
		{TS: base.Add(200 * time.Millisecond).UnixMilli(), SessionID: "s-move", Model: "m"},
		{TS: base.Add(900 * time.Millisecond).UnixMilli(), SessionID: "s-move", Model: "m"},
		{TS: base.Add(950 * time.Millisecond).UnixMilli(), SessionID: "s-move", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, "-Users-v-first", "s-move", base, [][2]any{
		{50, "/Users/v/first"},
		{500, "/Users/v/second"},
	})

	if _, err := db.BackfillProjects(home, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project string
		want    int
	}{{"/Users/v/first", 2}, {"/Users/v/second", 2}} {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE project = ?`,
			tc.project).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != tc.want {
			t.Errorf("project %s has %d rows; want %d — a session that changed directory must "+
				"be split at the move, not attributed wholesale to where it started",
				tc.project, n, tc.want)
		}
	}
}

// SCRATCH DIRECTORIES ARE NOT PROJECTS, and this is the rule that makes the back-fill usable
// rather than merely accurate.
//
// An agent that runs a command in a temporary directory records it as its cwd, so replaying a
// transcript literally files those requests under /tmp. Measured on the real store: 2,503 requests
// to the project and 485 spread across THIRTEEN `/private/tmp/*` directories. Thirteen throwaway
// folders in the project picker make the picker worse than no picker, and the work plainly
// belonged to the project either way.
//
// So a request whose moment lands in scratch space is filed under the session's home — the
// non-scratch directory it spent the most entries in. A move between two REAL directories is
// still split per request, which the test above pins.
func TestABackfillDoesNotTurnTempDirectoriesIntoProjects(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-6 * time.Hour).Truncate(time.Second)

	if err := db.insertBatch([]*Event{
		{TS: base.Add(100 * time.Millisecond).UnixMilli(), SessionID: "s-tmp", Model: "m"},
		{TS: base.Add(600 * time.Millisecond).UnixMilli(), SessionID: "s-tmp", Model: "m"},
		{TS: base.Add(700 * time.Millisecond).UnixMilli(), SessionID: "s-tmp", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	// The session lives in the project and dips into two scratch dirs, as a real one does.
	writeTranscript(t, home, "-Users-v-proj", "s-tmp", base, [][2]any{
		{50, "/Users/v/proj"},
		{80, "/Users/v/proj"},
		{500, "/private/tmp"},
		{650, "/private/tmp/cg-probe"},
	})

	if _, err := db.BackfillProjects(home, true); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE project = '/Users/v/proj'`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("the project has %d of 3 requests; the ones recorded while the agent was in a "+
			"temp directory must be filed under the session's home, not under /tmp", n)
	}
	var scratch int
	if err := db.sql.QueryRow(
		`SELECT COUNT(*) FROM requests WHERE project LIKE '/private/tmp%' OR project LIKE '/tmp%'`).
		Scan(&scratch); err != nil {
		t.Fatal(err)
	}
	if scratch != 0 {
		t.Errorf("%d requests were filed under a temp directory; those become junk entries in "+
			"the project picker", scratch)
	}
}

// A session that genuinely ran entirely in scratch space has nowhere better to be filed, so it
// keeps its own directory rather than being dropped or invented.
func TestASessionThatOnlyEverRanInTempKeepsIt(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)

	if err := db.insertBatch([]*Event{
		{TS: base.Add(100 * time.Millisecond).UnixMilli(), SessionID: "s-only-tmp", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, "-private-tmp", "s-only-tmp", base, [][2]any{
		{50, "/private/tmp/scratch"},
	})

	if _, err := db.BackfillProjects(home, true); err != nil {
		t.Fatal(err)
	}
	var p string
	if err := db.sql.QueryRow(
		`SELECT project FROM requests WHERE session_id = 's-only-tmp'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p != "/private/tmp/scratch" {
		t.Errorf("project = %q; a session with no non-scratch directory keeps its own rather "+
			"than being dropped or filed somewhere it never was", p)
	}
}

// A session with no transcript stays unattributed and is NAMED, because "3,002 of 3,003" needs
// the one explained rather than rounded away.
func TestABackfillLeavesWhatItCannotFindAloneAndSaysSo(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	if err := db.insertBatch([]*Event{
		{TS: base.Add(10 * time.Millisecond).UnixMilli(), SessionID: "s-known", Model: "m"},
		{TS: base.Add(20 * time.Millisecond).UnixMilli(), SessionID: "s-gone", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, "-Users-v-known", "s-known", base, [][2]any{{0, "/Users/v/known"}})

	got, err := db.BackfillProjects(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Updated != 1 {
		t.Errorf("updated %d rows; want only the 1 with a transcript", got.Updated)
	}
	if len(got.Unmatched) != 1 || got.Unmatched[0] != "s-gone" {
		t.Errorf("Unmatched = %v; want exactly [s-gone], so the gap is reportable", got.Unmatched)
	}
	var orphan string
	if err := db.sql.QueryRow(
		`SELECT project FROM requests WHERE session_id = 's-gone'`).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if orphan != "" {
		t.Errorf("a session with no transcript was attributed to %q; it must stay unattributed",
			orphan)
	}
}

// The live request path is the authority. A back-fill must never overwrite an attribution it
// already made, or a re-run would rewrite today's correct rows from yesterday's transcripts.
func TestABackfillNeverOverwritesWhatTheRequestPathAlreadyRecorded(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	if err := db.insertBatch([]*Event{
		{TS: base.Add(10 * time.Millisecond).UnixMilli(), SessionID: "s-live", Model: "m",
			Project: "/the/real/answer"},
	}); err != nil {
		t.Fatal(err)
	}
	// A transcript that disagrees.
	writeTranscript(t, home, "-wrong", "s-live", base, [][2]any{{0, "/somewhere/else"}})

	got, err := db.BackfillProjects(home, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Updated != 0 {
		t.Errorf("updated %d already-attributed rows; want 0", got.Updated)
	}
	var p string
	if err := db.sql.QueryRow(
		`SELECT project FROM requests WHERE session_id = 's-live'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p != "/the/real/answer" {
		t.Errorf("project became %q; the request path's own value must win", p)
	}
}

// A credential-shaped directory name must not reach the database from this path either. The value
// comes off the filesystem rather than the wire, but it is still a path somebody else chose.
func TestABackfillSanitizesWhatItReads(t *testing.T) {
	db := openTestDB(t)
	home := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	const canary = "sk-ant-api03-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

	if err := db.insertBatch([]*Event{
		{TS: base.Add(10 * time.Millisecond).UnixMilli(), SessionID: "s-secret", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, "-secret", "s-secret", base, [][2]any{{0, "/Users/v/" + canary}})

	if _, err := db.BackfillProjects(home, true); err != nil {
		t.Fatal(err)
	}
	var p string
	if err := db.sql.QueryRow(
		`SELECT project FROM requests WHERE session_id = 's-secret'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p != Redacted {
		t.Errorf("project = %q; want it redacted, since a folder can be named after a token", p)
	}
}
