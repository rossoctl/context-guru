package keepalive

// BilledOneHourWriteSQL is BilledOneHour's write half as a SQL condition: the response billed
// every token it wrote at the one-hour tier (cache_write_1h >= cache_write). alias is the row's
// table alias with its dot ("r."), or empty.
//
// It is true only for a row that wrote (cache_write > 0). A row that wrote nothing bills no tier
// and inherits the thread's last one, which SQL readers take from the latest row that did write.
// A mixed write (some tokens at 1 hour, some at 5 minutes) is NOT one hour: the 5-minute part
// expires first. Every SQL reader of the billed tier calls this, so SQL and BilledOneHour cannot
// disagree.
//
// ttlPredicate in dash/kvcache.go asks a different question (did the provider bill ANY of the
// write at 1 hour, as evidence of the policy) and keeps its own test on purpose.
func BilledOneHourWriteSQL(alias string) string {
	return alias + "cache_write > 0 AND " + alias + "cache_write_1h >= " + alias + "cache_write"
}
