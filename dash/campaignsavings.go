package dash

import (
	"sort"
	"strings"
)

// The real-saving half of a strategy campaign's drill-down — see
// proxy/campaign.go for how a campaign turns suggest cells into strategies and
// dash/keepalivestrategy.go's StrategyLedger, whose per-strategy attribution this
// generalizes from one strategy's totals to a (tenant, hour-of-day) grid.
//
// # Why the credited half is scoped by strategy id, not by tenant alone
//
// This file shipped believing a credited REAL row carried no strategy id — that only the
// ping did — and so summed a tenant's WHOLE keep-alive credit into whichever campaign
// happened to name that tenant, declaring the result a documented "ceiling" that could
// not be narrowed. That premise was already false when this shipped: keeper.arrive
// returns the strategy that resolved the idle entry, capture threads it onto the credited
// row (see dash/schema.go's keepalive_strategy_id comment), and
// keepalivestrategybackfill.go recovers it on rows written before that tagging existed.
// StrategyLedger was corrected to filter on it; this was not, and the two disagreed.
//
// The cost of leaving it was not theoretical. On this deployment every tenant with any
// credit at all is credited under three to six DISTINCT strategies, so a campaign owning
// one of them reported all six strategies' savings as its own — and two campaigns over
// one tenant each reported 100% of that tenant's credit, so the campaigns summed to
// several times the money that actually existed. That is the one thing the Strategies
// tab's own copy promises does not happen ("exact and additive across strategies, no
// double-counting"); a campaign is a bulk way to create those same rows and owes the
// reader the same guarantee.
//
// So SavedUSD is now EXACT, attributed once, the same way StrategyLedger's is. What it
// deliberately excludes: a credit whose ping matched no strategy at all (plain account
// config, or a session override) belongs to no campaign and is counted by none of them —
// the same population gap renderStrategiesList already explains to the reader, not a new
// one introduced here.
//
// # Why Requests and ActiveDays stay tenant-wide
//
// They are DENOMINATORS, not credit: "$ saved per 1k requests this tenant sent in this
// hour" only means anything if the denominator is all of that traffic. Narrowing them to
// the rescued rows would divide a strategy's saving by its own successes, which is a rate
// that always looks the same no matter how much traffic it missed. So the two halves of
// this query are scoped differently ON PURPOSE — a conditional SUM inside one scan rather
// than two, so the difference is visible in one place instead of split across queries.
//
// Every read here is bounded by `since` (a campaign's own activated_at): comparing a
// strategy's predicted saving against traffic that predates the campaign would credit it
// for a period it never ran in, which the design doc's "Out of scope" explicitly refuses
// to do for pings and which this refuses too, for the same reason.

// CampaignSavingCell is one (tenant, hour-of-day) cell's real economics since a
// campaign went live.
type CampaignSavingCell struct {
	TenantID string `json:"tenant_id"`
	HourUTC  int    `json:"hour_utc"`
	// Requests is real (non-ping) traffic in this cell since `since` — the denominator
	// for a $-per-1k-requests normalization, deliberately NOT the ping count, since a
	// ping is housekeeping this service issued, not traffic the tenant sent.
	Requests int64 `json:"requests"`
	// ActiveDays is how many distinct calendar days (UTC) this cell saw any real
	// request since `since` — the denominator for a $-per-active-day normalization.
	ActiveDays int64 `json:"active_days"`
	// Pings and PingUSD are EXACT, the same guarantee StrategyLedgerRow makes: every
	// ping row one of this campaign's own strategies caused carries that strategy's id.
	Pings   int64   `json:"pings"`
	PingUSD float64 `json:"ping_usd"`
	// SavedUSD is EXACT too, as of the strategy-id scoping described in the file doc
	// comment: only credit a ping from one of THIS campaign's own strategies rescued,
	// never the tenant's whole keep-alive credit in the cell.
	SavedUSD float64 `json:"saved_usd"`
	NetUSD   float64 `json:"net_usd"`
}

// CampaignRealSavings computes real, time-bounded economics per (tenant, hour-of-day)
// cell for a set of strategies (this campaign's own, so the cost half is exact) and the
// tenants they target (so the saving half's ceiling is at least scoped to the right
// accounts). since is a campaign's own activated_at, in epoch milliseconds — never 0
// meaning "no bound", because that would silently fold in pre-campaign history.
//
// Returns one cell per (tenant, hour) that had ANY ping or any real credited request
// since `since` — a cell absent from the result had neither, not a cell this query
// forgot to report.
func (d *DB) CampaignRealSavings(strategyIDs, tenantIDs []string, since int64) ([]CampaignSavingCell, error) {
	if len(strategyIDs) == 0 && len(tenantIDs) == 0 {
		return []CampaignSavingCell{}, nil
	}
	type cellKey struct {
		tenantID string
		hour     int
	}
	cells := map[cellKey]*CampaignSavingCell{}
	get := func(tenantID string, hour int) *CampaignSavingCell {
		key := cellKey{tenantID, hour}
		c := cells[key]
		if c == nil {
			c = &CampaignSavingCell{TenantID: tenantID, HourUTC: hour}
			cells[key] = c
		}
		return c
	}

	// Cost half: every ping row one of this campaign's strategies caused, grouped by
	// the tenant it pinged and the UTC hour it landed in. %H is a Go format verb as
	// much as a strftime one — this string is a query parameter, never passed through
	// fmt.Sprintf (see dash/kvcache.go's own warning about exactly this trap).
	//
	// Gated on strategyIDs alone (not tenantIDs too): this half doesn't touch
	// tenant_id at all, so a caller with strategies but no tenant list (or vice versa
	// for the saving half below) must still get its own half's real answer, not an
	// empty result — an empty answer here must mean "no matching rows," never "the
	// caller happened to pass an empty other list."
	if len(strategyIDs) > 0 {
		costArgs := make([]any, 0, len(strategyIDs)+1)
		for _, id := range strategyIDs {
			costArgs = append(costArgs, id)
		}
		costArgs = append(costArgs, since)
		costRows, err := d.sql.Query(`SELECT tenant_id,
				CAST(strftime('%H', ts/1000, 'unixepoch') AS INTEGER) h,
				COUNT(*), COALESCE(SUM(cost_usd),0)
			FROM requests WHERE keepalive = 1 AND keepalive_strategy_id IN (`+
			placeholders(len(strategyIDs))+`) AND ts >= ?
			GROUP BY tenant_id, h`, costArgs...)
		if err != nil {
			return nil, err
		}
		for costRows.Next() {
			var tenantID string
			var hour int
			var pings int64
			var pingUSD float64
			if err := costRows.Scan(&tenantID, &hour, &pings, &pingUSD); err != nil {
				costRows.Close()
				return nil, err
			}
			c := get(tenantID, hour)
			c.Pings, c.PingUSD = pings, pingUSD
		}
		costRows.Close()
		if err := costRows.Err(); err != nil {
			return nil, err
		}
	}

	// Saving half: every REAL (non-ping) request for one of this campaign's tenants,
	// grouped the same way. Requests and ActiveDays cover ALL of that traffic (they are
	// normalization denominators); the credited sum inside the same scan is narrowed to
	// rows one of THIS campaign's own strategies rescued — see the file doc comment for
	// why the two are scoped differently, and why that is not the ceiling this once was.
	//
	// With no strategy ids at all (a campaign whose every cell resolved to a
	// non-activatable arm), the credited sum is a literal 0 rather than an "IN ()" —
	// which is not SQLite-valid — and the denominators still report, so the drill-down
	// can honestly say "this much traffic, none of it ours to claim."
	if len(tenantIDs) > 0 {
		savedExpr := `0`
		savingArgs := make([]any, 0, len(strategyIDs)+len(tenantIDs)+1)
		if len(strategyIDs) > 0 {
			savedExpr = `COALESCE(SUM(CASE WHEN keepalive_saved_usd > 0
				AND keepalive_strategy_id IN (` + placeholders(len(strategyIDs)) + `)
				THEN keepalive_saved_usd ELSE 0 END),0)`
			// Bound before the tenant list because the CASE sits in the SELECT clause,
			// which SQLite binds ahead of the WHERE — the args must follow the ORDER THE
			// TEXT reads, not the order the two lists are named in this function's own
			// signature. Getting this backwards would silently filter by tenant ids in
			// the strategy position and vice versa, with no error at all: both are
			// opaque hex strings of the same shape, so every row would simply fail to
			// match and every cell would read $0.00 saved.
			for _, id := range strategyIDs {
				savingArgs = append(savingArgs, id)
			}
		}
		for _, id := range tenantIDs {
			savingArgs = append(savingArgs, id)
		}
		savingArgs = append(savingArgs, since)
		savingRows, err := d.sql.Query(`SELECT tenant_id,
				CAST(strftime('%H', ts/1000, 'unixepoch') AS INTEGER) h,
				COUNT(*), COUNT(DISTINCT ts/86400000), `+savedExpr+`
			FROM requests WHERE keepalive = 0 AND tenant_id IN (`+
			placeholders(len(tenantIDs))+`) AND ts >= ?
			GROUP BY tenant_id, h`, savingArgs...)
		if err != nil {
			return nil, err
		}
		for savingRows.Next() {
			var tenantID string
			var hour int
			var requests, activeDays int64
			var savedUSD float64
			if err := savingRows.Scan(&tenantID, &hour, &requests, &activeDays, &savedUSD); err != nil {
				savingRows.Close()
				return nil, err
			}
			c := get(tenantID, hour)
			c.Requests, c.ActiveDays, c.SavedUSD = requests, activeDays, savedUSD
		}
		savingRows.Close()
		if err := savingRows.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]CampaignSavingCell, 0, len(cells))
	for _, c := range cells {
		c.NetUSD = c.SavedUSD - c.PingUSD
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].HourUTC < out[j].HourUTC
	})
	return out, nil
}

// placeholders builds "?,?,...", n times — SQLite has no native array bind, so an IN
// clause over a caller-supplied id list needs one placeholder per id.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
