package dash

// One check that the janitor actually refreshes planner statistics.
//
// Worth a test because the failure is silent and slow rather than visible: this database ran in
// production with no sqlite_stat1 at all, so SQLite planned every join in this package on
// hard-coded guesses and picked the wrong join order. Nothing breaks when that regresses —
// queries just get several times slower, which is how it went unnoticed in the first place.

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestJanitorPassRefreshesPlannerStatistics(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	// Enough rows that PRAGMA optimize judges the tables worth analysing; with a handful it may
	// legitimately decide there is nothing to do.
	evs := make([]*Event, 0, 400)
	for i := 0; i < 400; i++ {
		e := mkEvent(time.Now().UnixMilli()-int64(i)*1000, "sess-opt", "aws/claude-sonnet-5", 1000, 800)
		e.Components = []CompRow{{Component: "toon", Kind: "reformat", Acted: true}}
		evs = append(evs, e)
	}
	seed(t, rec, evs...)
	_ = a

	var before int
	// sqlite_stat1 does not exist until something analyses; a missing table is the "before" state.
	_ = rec.DB().sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sqlite_stat1'`).Scan(&before)
	if before != 0 {
		t.Fatalf("sqlite_stat1 already exists before any janitor pass (%d); this check needs rewriting", before)
	}

	rec.janitorPass()

	var after int
	if err := rec.DB().sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sqlite_stat1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == 0 {
		t.Error("janitorPass did not create sqlite_stat1: planner statistics are never refreshed.\n" +
			"Measured consequence on the production corpus: /api/components over 24h took 3.59s " +
			"without statistics and 0.81s with them — same code, same indexes, 4.4x.")
	}
}

// The mask is asserted at SOURCE level because the functional test above cannot be trusted to
// catch its absence, and I know that because it did not.
//
// A bare `PRAGMA optimize` only considers tables the CURRENT CONNECTION has queried. The janitor
// takes a fresh pooled connection and queries nothing, so in production a bare form analyses
// nothing at all — measured on a copy of the production database: 0.00s, sqlite_stat1 still
// absent. The test above passed anyway, because seeding and janitorPass shared a pooled
// connection that HAD touched those tables. So the functional assertion can go green while the
// deployed behaviour is a no-op, which is precisely the shape that needs a source-level guard.
func TestOptimizeKeepsTheMaskThatMakesItWork(t *testing.T) {
	src := readSource(t, "store.go")
	if !strings.Contains(src, "PRAGMA optimize(0x10002)") {
		t.Error("DB.optimize no longer runs `PRAGMA optimize(0x10002)`.\n" +
			"0x10000 is what lets it consider tables this connection has not queried; without it the " +
			"janitor's fresh pooled connection causes SQLite to analyse nothing and the call is a " +
			"silent no-op. Measured: bare form 0.00s and no statistics; 0x10002 produces all 26 rows, " +
			"also 0.00s, and takes the component aggregate from 2.0s to 0.209s.")
	}
}

// readSource reads a Go source file in this package from disk. Package tests run with the package
// directory as the working directory, so a bare filename resolves.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
