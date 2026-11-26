package dash

// ManualPings is the "Manual pings" savings category: pings the USER asked for with the Ping
// button, kept apart from the cache strategy because a person pressing a button is not the
// strategy's decision and must not move its net.
//
// SavedUSD is already NET of the pings' own cost: the cache write each ping avoided on the
// then-current context, minus what the ping cost (negative if a ping missed and paid for a write
// instead). So it is the figure that joins total_saved_usd, and PingUSD is shown beside it for the
// reader, not to be subtracted again.
//
// WritesAvoided is how many of the pings found the cache still warm (read it). It counts pings, not
// later requests: two presses in one cache lifetime both count, so the Overview shows it as an
// upper bound, apart from the measured count of requests.
type ManualPings struct {
	Count         int64   `json:"count"`
	PingUSD       float64 `json:"ping_usd"`
	SavedUSD      float64 `json:"saved_usd"`
	WritesAvoided int64   `json:"writes_avoided"`
}

// manualAgg is what the SQL sums: the stored, GROSS avoided write beside the cost.
type manualAgg struct {
	count, writes    int64
	pingUSD, avoided float64
}

func (a manualAgg) pings() ManualPings {
	return ManualPings{Count: a.count, PingUSD: a.pingUSD, SavedUSD: a.avoided - a.pingUSD, WritesAvoided: a.writes}
}

func (a *manualAgg) dest() []any { return []any{&a.count, &a.pingUSD, &a.avoided, &a.writes} }

// autoPing selects the pings the keeper scheduled: the rows every strategy ledger is built from.
// A manual ping is also keepalive = 1 (it is real spend and it refreshes the provider's entry),
// so each ledger names this instead of `r.keepalive = 1`. Written once so a new ledger cannot
// forget the exclusion.
const autoPing = `r.keepalive = 1 AND r.manual_ping = 0`

// manualPing selects the user's own pings.
const manualPing = `r.keepalive = 1 AND r.manual_ping = 1`

// manualSelect is the four SELECT terms behind manualAgg, in field order. The caller's WHERE
// must include ping rows (withKeepAlive).
const manualSelect = `COALESCE(SUM(CASE WHEN ` + manualPing + ` THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + manualPing + ` THEN r.cost_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + manualPing + ` THEN r.manual_saved_usd ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN ` + manualPing + ` AND r.cache_read > 0 THEN 1 ELSE 0 END),0)`
