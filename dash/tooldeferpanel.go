package dash

import (
	"net/http"
	"sort"
)

// GET /api/tool-defer: tool deferral's net saving per account and per day (#485, #869).
//
// The figures are the proxy's own deferral ledger, stored per request on the request row
// (Event.DeferTokens and the rest): tokens deferred, the server-side searches the responses ran,
// the tools those searches loaded, the wrong guesses (a deferred tool that was then loaded), and
// the ledger's gross and net dollars. Net is gross minus the names list and every search cost
// (tooldefer.Price), so this page and /stats show the same net figure. The wrong guesses and the
// dropped searches are the ledger's own numbers (tooldefer.WrongGuesses, SearchRead.Dropped).
//
// Metadata only: counts and prices from the request rows. Days are UTC days, like every other
// per-day figure on the dashboard (see DayMs).

// deferNetSum is the one SQL expression for deferral's net dollars over a set of request rows. The
// panel, the Savings totals (SavingsTotals, Overview, OverviewLean) and the daily series all use
// it, so they cannot disagree. Net is stored per request; nothing is priced on read.
const deferNetCol = "r.defer_net_usd"
const deferNetSum = "COALESCE(SUM(" + deferNetCol + "),0)"

// ToolDeferDay is one account's deferral on one UTC day.
type ToolDeferDay struct {
	// Day is the UTC midnight of the day, epoch ms.
	Day    int64  `json:"day"`
	Tenant string `json:"tenant"`
	// Requests is how many requests deferral changed.
	Requests int64 `json:"requests"`
	// TokensDeferred is the deferred definitions' weight summed over those requests: what they did
	// not carry. Our tokenizer's units, before the billed-token correction the dollars carry.
	TokensDeferred int64 `json:"tokens_deferred"`
	ListTokens     int64 `json:"list_tokens"`
	Searches       int64 `json:"searches"`
	// DroppedSearches is the searches with no result (#866 N8): the model searched again.
	DroppedSearches int64 `json:"dropped_searches"`
	// Loaded is the tools the searches loaded; WrongGuesses the part of them deferral had
	// deferred and no earlier search in the thread had loaded.
	Loaded       int64   `json:"loaded"`
	WrongGuesses int64   `json:"wrong_guesses"`
	GrossUSD     float64 `json:"gross_usd"`
	NetUSD       float64 `json:"net_usd"`
	// CostUSD is gross minus net: the names list and the searches.
	CostUSD float64 `json:"cost_usd"`
	// Unpriced is the changed requests whose model has no price: their dollars read 0, not free.
	Unpriced int64 `json:"unpriced_requests"`
}

// ToolDeferReport is the /api/tool-defer response: the days, newest first, and their total.
type ToolDeferReport struct {
	Days  []ToolDeferDay `json:"days"`
	Total ToolDeferDay   `json:"total"`
}

// ToolDeferReportFor reads deferral's per-day ledger over f. A request deferral did not change has
// defer_tokens = 0 and is not read.
func (d *DB) ToolDeferReportFor(f Filter) (*ToolDeferReport, error) {
	cond, args := f.where()
	q := `SELECT (r.ts / ?) * ?, r.tenant_id, COUNT(*), SUM(r.defer_tokens), SUM(r.defer_list_tokens),
		SUM(r.defer_searches), SUM(r.defer_dropped_searches), SUM(r.defer_loaded), SUM(r.defer_wrong_guesses),
		SUM(r.defer_gross_usd), ` + deferNetSum + `,
		SUM(CASE WHEN r.defer_gross_usd = 0 AND r.defer_net_usd = 0 THEN 1 ELSE 0 END)
		FROM requests r WHERE ` + cond + ` AND r.defer_tokens > 0 GROUP BY 1, 2`
	rows, err := d.sql.QueryContext(d.readCtx(), q, append([]any{DayMs, DayMs}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &ToolDeferReport{Days: []ToolDeferDay{}}
	for rows.Next() {
		var x ToolDeferDay
		if err := rows.Scan(&x.Day, &x.Tenant, &x.Requests, &x.TokensDeferred, &x.ListTokens,
			&x.Searches, &x.DroppedSearches, &x.Loaded, &x.WrongGuesses, &x.GrossUSD, &x.NetUSD, &x.Unpriced); err != nil {
			return nil, err
		}
		x.CostUSD = x.GrossUSD - x.NetUSD
		out.Days = append(out.Days, x)
		t := &out.Total
		t.Requests += x.Requests
		t.TokensDeferred += x.TokensDeferred
		t.ListTokens += x.ListTokens
		t.Searches += x.Searches
		t.DroppedSearches += x.DroppedSearches
		t.Loaded += x.Loaded
		t.WrongGuesses += x.WrongGuesses
		t.GrossUSD += x.GrossUSD
		t.NetUSD += x.NetUSD
		t.CostUSD += x.CostUSD
		t.Unpriced += x.Unpriced
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out.Days, func(i, j int) bool {
		if out.Days[i].Day != out.Days[j].Day {
			return out.Days[i].Day > out.Days[j].Day
		}
		return out.Days[i].Tenant < out.Days[j].Tenant
	})
	return out, nil
}

// toolDefer serves GET /api/tool-defer for the caller's scope: one account, or every account for a
// manager's service-wide view.
func (a *API) toolDefer(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	rep, err := a.db(r).ToolDeferReportFor(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, rep)
}
