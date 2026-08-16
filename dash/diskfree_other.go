//go:build !unix

package dash

// diskUsage is unavailable off unix. Reporting ok=false disables the disk-pressure
// rule, leaving the age and byte-budget rules to bound the store — which is the
// right failure direction: guessing at free space would either evict history nobody
// asked us to evict, or fail to evict when the host is actually full.
func diskUsage(string) (float64, bool) { return 0, false }
