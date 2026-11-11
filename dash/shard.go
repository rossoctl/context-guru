package dash

import (
	"database/sql"
	"runtime"
	"sync"

	"golang.org/x/sync/errgroup"
)

// shardCount is how many id slices a sharded aggregate is split into. See forEachShard.
const shardCount = 8

// forEachShard runs fn once per contiguous slice of the request id space, all slices at once, and
// returns the first error.
//
// It exists because the heavy aggregates join request_components (1.7M rows) or tool_declarations
// to requests, and one SQLite statement runs on one core: the Components tab's six such queries
// were 19 s end to end on a machine that was otherwise idle. A GROUP BY over a disjoint slice of
// the ids is the same GROUP BY over fewer rows, and its groups merge by summation, so the slices
// can run side by side. Measured: reads on separate pooled connections scale ~4x at 4-way and the
// slices finish evenly because request ids are dense.
//
// pred is " AND r.id >= ? AND r.id < ?" and bounds its two arguments; the caller appends both to
// its own argument list immediately after the cond they follow, which is where the placeholders
// sit in the text. fn runs concurrently with itself and must guard whatever it shares.
func (d *DB) forEachShard(fn func(pred string, bounds []any) error) error {
	var lo, hi sql.NullInt64
	if err := d.sql.QueryRowContext(d.readCtx(), `SELECT MIN(id), MAX(id) FROM requests`).Scan(&lo, &hi); err != nil {
		return err
	}
	if !lo.Valid {
		return fn("", nil) // empty table: one unsharded run, which finds nothing
	}
	step := (hi.Int64-lo.Int64)/shardCount + 1
	var g errgroup.Group
	for k := int64(0); k < shardCount; k++ {
		a, b := lo.Int64+k*step, lo.Int64+(k+1)*step
		g.Go(func() error { return fn(" AND r.id >= ? AND r.id < ?", []any{a, b}) })
	}
	return g.Wait()
}

// guarded is a mutex around the merge a forEachShard callback does.
type guarded struct{ sync.Mutex }

// parallelFor runs fn(0..n-1) on up to GOMAXPROCS goroutines and returns when all are done. fn
// must write only to its own index of whatever it fills, which is how its callers keep the output
// order the sequential loop had.
func parallelFor(n int, fn func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}
