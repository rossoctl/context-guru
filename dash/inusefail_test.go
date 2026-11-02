package dash

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/store"
)

// The in-use check's answer is only as safe as its scan. A query that fails midway, or a
// row that fails to scan, must read as "every key is in use": the keys the scan did not
// reach would otherwise read as unused, and deleteSessionState would delete state a live
// session still reads (#775 round 2). The fake driver below stands in for the host's
// database — no real SQLite run fails a scan on demand.
func TestUnreadableInUseAnswerKeepsAllState(t *testing.T) {
	fake := &inUseFailDB{inUseVal: "k-live"}
	h := sql.OpenDB(inUseFailConnector{fake})
	defer h.Close()
	d := &DB{sql: h}
	d.deleteSessionState([]string{"k-gone", "k-live"}, nil)
	if fake.deletes != 0 {
		t.Errorf("ran %d state deletes on a scan that failed midway, want 0: keys the scan did not reach read as unused", fake.deletes)
	}
}

// The same, one step earlier: a row the scan cannot deliver (Scan fails) must not be
// skipped silently either. The row is followed by a CLEAN end, so only the Scan guard —
// not rows.Err() — can keep the keys that never scanned.
func TestUnscannableInUseRowKeepsAllState(t *testing.T) {
	// No value inside the driver.Value contract fails this Scan — strings, bytes, numbers,
	// bools and times all convert into a string — so the fake returns one outside it. The
	// guard is defense in depth, and this is the only way to reach it.
	fake := &inUseFailDB{inUseVal: struct{}{}, cleanEnd: true}
	h := sql.OpenDB(inUseFailConnector{fake})
	defer h.Close()
	d := &DB{sql: h}
	d.deleteSessionState([]string{"k-gone", "k-live"}, nil)
	if fake.deletes != 0 {
		t.Errorf("ran %d state deletes on a row that failed to scan, want 0", fake.deletes)
	}
}

// inUseFailDB is the shared state of the fake driver's connections. inUseVal is the one
// value the in-use queries yield; cleanEnd ends the scan with io.EOF instead of a failure;
// deletes counts the DELETE statements the state cleanup ran — both tests want none.
type inUseFailDB struct {
	inUseVal driver.Value
	cleanEnd bool
	deletes  int
}

type inUseFailConnector struct{ db *inUseFailDB }

func (c inUseFailConnector) Connect(context.Context) (driver.Conn, error) {
	return inUseFailConn{c.db}, nil
}

func (inUseFailConnector) Driver() driver.Driver { return inUseFailDriver{} }

type inUseFailDriver struct{}

func (inUseFailDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not implemented") }

type inUseFailConn struct{ db *inUseFailDB }

func (inUseFailConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (inUseFailConn) Close() error                        { return nil }
func (inUseFailConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c inUseFailConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "WHERE session_alias IN (") || strings.Contains(q, "WHERE session_id IN (") {
		// The in-use answer: one row, then the scan fails (or ends cleanly, per cleanEnd).
		return &inUseFailRows{vals: []driver.Value{c.db.inUseVal}, eof: c.db.cleanEnd}, nil
	}
	if strings.Contains(q, "FROM session_state") {
		// store.DeleteSessionState's listing: one key it would collect and delete, ending
		// CLEANLY so its own scan-error guard does not stop it — the broken in-use path
		// must reach the DELETE for the test to count it.
		return &inUseFailRows{vals: []driver.Value{[]byte(store.BilledPrefix + "k-gone")}, eof: true}, nil
	}
	return &inUseFailRows{}, nil
}

func (c inUseFailConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.db.deletes++
	return driver.RowsAffected(1), nil
}

// inUseFailRows yields its values, then fails — or ends cleanly when eof is set, which is
// how the Scan guard is tested apart from the rows.Err() guard. A non-io.EOF return from
// Next is a scan error, which lands in rows.Err() on the database/sql side.
type inUseFailRows struct {
	vals []driver.Value
	eof  bool
}

func (*inUseFailRows) Columns() []string { return []string{"k"} }
func (*inUseFailRows) Close() error      { return nil }

// Next has a POINTER receiver on purpose: it consumes r.vals, and a value receiver would
// hand each call a fresh copy of the full list, so the scan would never end.
func (r *inUseFailRows) Next(dest []driver.Value) error {
	if len(r.vals) > 0 {
		dest[0] = r.vals[0]
		r.vals = r.vals[1:]
		return nil
	}
	if r.eof {
		return io.EOF
	}
	return errors.New("scan failed midway")
}
