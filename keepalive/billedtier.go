package keepalive

// A write can be mixed: a 1-hour head over a 5-minute tail (the proxy's own head-TTL shape). That is
// two cache entries with two lifetimes: the head lives one hour and only the tail expires in five
// minutes. So "which tier?" is two questions, and each has one named rule here, in Go and in SQL.
//
//   - BilledOneHour / BilledOneHourWriteSQL: does the WHOLE entry live one hour? Every written token
//     was billed at the one-hour tier. The keeper and the savings gate decide money on this.
//   - HeadLifetimeOneHour / HeadLifetimeOneHourWriteSQL: does the HEAD live one hour? Any written
//     token was billed at the one-hour tier. The panels' lifetime clock uses this one.
//
// In both, a turn that only READS the cache bills no tier and inherits the thread's last answer
// (prev in Go; the latest earlier write row in SQL).

// BilledOneHourWriteSQL is BilledOneHour's write half as a SQL condition: every token the row wrote
// was billed at the one-hour tier (cache_write_1h >= cache_write). alias is the row's table alias
// with its dot ("r."), or empty. It is true only for a row that wrote (cache_write > 0). A mixed
// write is NOT whole-entry one hour: its 5-minute tail expires first.
func BilledOneHourWriteSQL(alias string) string {
	return alias + "cache_write > 0 AND " + alias + "cache_write_1h >= " + alias + "cache_write"
}

// HeadLifetimeOneHour answers: does the head of the entry live one hour? A response that wrote
// tokens bills a tier on them, and one token at the one-hour tier is enough: the head of a mixed
// write (1-hour head, 5-minute tail) lives one hour. A response that only read the cache bills no
// tier, so it inherits prev. Compare BilledOneHour, which asks the same of the WHOLE entry.
func HeadLifetimeOneHour(cacheWrite, cacheWrite1h int64, prev bool) bool {
	if cacheWrite > 0 {
		return cacheWrite1h > 0
	}
	return prev
}

// HeadLifetimeOneHourWriteSQL is HeadLifetimeOneHour's write half as a SQL condition: the row wrote,
// and any of it was billed at the one-hour tier. alias is as in BilledOneHourWriteSQL.
func HeadLifetimeOneHourWriteSQL(alias string) string {
	return alias + "cache_write > 0 AND " + alias + "cache_write_1h > 0"
}
