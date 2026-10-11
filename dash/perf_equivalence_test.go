package dash

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The golden test pins the endpoints' output on a fixture too small to reach some of the new
// code (the 250-session cut-over to the table pass). These tests pin each rewritten read to the
// read it replaced, on the same fixture, by forcing both.

// The inventory built from one pass over tool_declarations must equal the per-session probe it
// replaced, for the report, the observation windows and the prompt view.
func TestInventoryTablePassEqualsPerSessionProbe(t *testing.T) {
	a := goldenAPI(t)
	f := Filter{TenantAll: true}
	read := func() (rep, win, prompt []byte) {
		db := a.rec.DB()
		r, err := db.ToolFilterDocFor(f, flatPrice, ToolFilterState{})
		if err != nil {
			t.Fatal(err)
		}
		tr, err := db.ToolReportFor(f, flatPrice)
		if err != nil {
			t.Fatal(err)
		}
		pv, err := db.PromptViewFor(f)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(tr)
		b, _ := json.Marshal(r)
		c, _ := json.Marshal(pv)
		return a, b, c
	}
	old := declScanMinSessions
	defer func() { declScanMinSessions = old }()
	declScanMinSessions = 1 << 30
	r1, w1, p1 := read()
	declScanMinSessions = 1
	r2, w2, p2 := read()
	for name, pair := range map[string][2][]byte{"report": {r1, r2}, "filter doc": {w1, w2}, "prompt view": {p1, p2}} {
		var x, y any
		_ = json.Unmarshal(pair[0], &x)
		_ = json.Unmarshal(pair[1], &y)
		if d := jsonDiff("", x, y, nil); len(d) > 0 {
			t.Errorf("%s: table pass differs from per-session probe: %v", name, d)
		}
	}
}

// The sharded dataset read must be the unsharded read, row for row, successor for successor.
func TestShardedKVDatasetEqualsSingleRead(t *testing.T) {
	a := goldenAPI(t)
	db := a.rec.DB()
	for _, o := range []KVCacheOptions{{}, {HasNext: "yes"}, {Bucket: "afternoon"}} {
		f := Filter{TenantAll: true}
		got, ok, err := db.kvCacheDatasetSharded(f, o)
		if err != nil || !ok {
			t.Fatalf("sharded read: ok=%v err=%v", ok, err)
		}
		want, total, err := db.kvCacheDatasetOne(f, o)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(got)) != total || !reflect.DeepEqual(got, want) {
			t.Fatalf("%+v: sharded dataset (%d rows) differs from the single read (%d rows)", o, len(got), len(want))
		}
	}
	// Over the cap the sharded read declines and the single read keeps the newest rows.
	old := kvCacheMaxRows
	defer func() { kvCacheMaxRows = old }()
	kvCacheMaxRows = 50
	rows, total, err := db.KVCacheDataset(Filter{TenantAll: true}, KVCacheOptions{})
	if err != nil || len(rows) != 50 || total <= 50 {
		t.Fatalf("truncated read: %d rows of %d, err %v", len(rows), total, err)
	}
}

// The Go counter sum and the json_each sum must agree, including for the shape the Go parser
// refuses (an escaped key) and for the two it must accept.
func TestCounterTotalsFastPathEqualsJSONEach(t *testing.T) {
	a := goldenAPI(t)
	db := a.rec.DB()
	cond, args := Filter{TenantAll: true}.where()
	fast, err := db.counterTotals(cond, args, "events") // flat integers only: the Go path
	if err != nil {
		t.Fatal(err)
	}
	if len(fast) == 0 {
		t.Fatal("fixture has no event counters")
	}
	// The escaped key in the fixture's gates forces the json_each path; the golden file pins its sums.
	general, err := db.counterTotals(cond, args, "gates")
	if err != nil {
		t.Fatal(err)
	}
	if general[[2]string{"extract", `weird"key`}] != 15 {
		t.Fatalf("json_each path lost the escaped key: %v", general)
	}
	for _, c := range []struct {
		in   string
		ok   bool
		want map[string]int64
	}{
		{`{"a":1,"b":22}`, true, map[string]int64{"a": 1, "b": 22}},
		{`{"a":-3}`, true, map[string]int64{"a": -3}},
		{`{}`, true, map[string]int64{}},
		{`{"a":1.5}`, false, nil}, {`{"a":"x"}`, false, nil}, {`{"a":01}`, false, nil},
		{`{"a\"b":1}`, false, nil}, {`[1,2]`, false, nil}, {`{"a":1,}`, false, nil},
		{`{"a":12345678901234567890}`, false, nil},
	} {
		got := map[string]int64{}
		ok := addIntObject(c.in, func(k string, n int64) { got[k] += n })
		if ok != c.ok || (ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("addIntObject(%s) = %v %v, want %v %v", c.in, ok, got, c.ok, c.want)
		}
	}
}

// An unbounded /api/series would answer a minute-wide bucket over all history with tens of
// thousands of buckets; it is widened to a whole multiple that fits.
func TestSeriesBucketIsWidenedToFitTheCap(t *testing.T) {
	a := goldenAPI(t)
	db := a.rec.DB()
	f := Filter{TenantAll: true}
	if got := db.SeriesBucketFor(f, 3_600_000); got != 3_600_000 {
		t.Errorf("an hourly request over %d hours was widened to %d", 9*24, got)
	}
	got := db.SeriesBucketFor(f, 1000) // 1 s buckets over ~10 days: 860k of them
	if got%1000 != 0 || got <= 1000 {
		t.Fatalf("bucket %d is not a widened multiple of 1000", got)
	}
	b, err := db.Series(f, got)
	if err != nil || len(b) > seriesMaxBuckets {
		t.Fatalf("%d buckets (cap %d), err %v", len(b), seriesMaxBuckets, err)
	}
	var total int64
	for _, x := range b {
		total += x.Requests
	}
	var want int64
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE keepalive = 0`).Scan(&want)
	if total != want {
		t.Errorf("widened series counts %d requests, the table has %d", total, want)
	}
}

// The driver runs the DSN's pragmas in order on every new connection, and journal_mode needs a
// lock. With busy_timeout after it a connection opened during a checkpoint failed at once with
// SQLITE_BUSY; the fan-out reads open a burst of connections, so the order is now load-bearing.
func TestBusyTimeoutIsSetBeforeJournalMode(t *testing.T) {
	d := dsn("/x/d.db")
	b, j := strings.Index(d, "busy_timeout"), strings.Index(d, "journal_mode")
	if b < 0 || j < 0 || b > j {
		t.Fatalf("busy_timeout must precede journal_mode in %q", d)
	}
}
