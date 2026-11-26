package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
)

// A scan that fails midway must report the failure, not delete the rows it had already
// collected and return success (#772). The fake driver below stands in for the host's
// database: its first query returns two keys and then fails, which no real SQLite run
// reproduces on demand.
func TestDeleteSessionStateReportsScanError(t *testing.T) {
	fake := &scanErrDB{}
	db := sql.OpenDB(scanErrConnector{fake})
	defer db.Close()

	n, err := DeleteSessionState(db, "abc")
	if err == nil {
		t.Fatal("a scan that failed midway reported success")
	}
	if n != 0 {
		t.Errorf("deleted %d rows before the failure, want 0", n)
	}
	if fake.execs != 0 {
		t.Errorf("ran %d deletes against a failed scan, want 0", fake.execs)
	}
}

// scanErrDB is the shared state of the fake driver's connections.
type scanErrDB struct {
	execs   int
	queries int
}

// The two keys the failing scan yields before it fails: BilledPrefix is the first namespace
// DeleteSessionState asks about, and both keys pass its boundary check for session "abc".
var scanErrKeys = [][]byte{[]byte(BilledPrefix + "abc"), []byte(BilledPrefix + "abc\x00t1")}

type scanErrConnector struct{ db *scanErrDB }

func (c scanErrConnector) Connect(context.Context) (driver.Conn, error) {
	return scanErrConn{c.db}, nil
}

func (c scanErrConnector) Driver() driver.Driver { return scanErrDriver{} }

type scanErrDriver struct{}

func (scanErrDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not implemented") }

type scanErrConn struct{ db *scanErrDB }

func (scanErrConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (scanErrConn) Close() error                        { return nil }
func (scanErrConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c scanErrConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.db.queries++
	if c.db.queries > 1 {
		return &scanErrRows{}, nil // later namespaces: nothing to find
	}
	return &scanErrRows{keys: scanErrKeys, fail: true}, nil
}

func (c scanErrConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.db.execs++
	return driver.RowsAffected(1), nil
}

// scanErrRows yields its keys, then ends: in failure, or cleanly when fail is unset. A
// non-io.EOF return from Next is a scan error, which lands in rows.Err() on the
// database/sql side.
type scanErrRows struct {
	keys [][]byte
	fail bool
}

func (*scanErrRows) Columns() []string { return []string{"key"} }
func (*scanErrRows) Close() error      { return nil }

// Next has a POINTER receiver on purpose: it consumes r.keys, and a value receiver would
// hand each call a fresh copy of the full list, so the scan would never end.
func (r *scanErrRows) Next(dest []driver.Value) error {
	if len(r.keys) > 0 {
		dest[0] = r.keys[0]
		r.keys = r.keys[1:]
		return nil
	}
	if r.fail {
		return errors.New("scan failed midway")
	}
	return io.EOF
}
