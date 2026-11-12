package dash

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// CurrencyMixed is the unit of a view whose priced rows are in more than one currency. Such a view
// has no money total: Bobcoins and dollars do not add. The UI shows no money figure for it.
const CurrencyMixed = "mixed"

// rowCurrency is what a row stores as its unit. An unpriced row has none and keeps the column's
// default, USD.
func rowCurrency(c string) string {
	if c == "" {
		return modelinfo.USD
	}
	return c
}

// Currency is the one unit of the priced rows in the filter: USD when there are none, CurrencyMixed
// when there are two or more. Only priced rows count. An unpriced row costs nothing in any unit, and
// it would otherwise make every Bob view mixed with the USD default it is stored with.
func (d *DB) Currency(f Filter) (string, error) {
	cond, args := f.where()
	rows, err := d.sql.QueryContext(d.readCtx(), `SELECT DISTINCT r.currency FROM requests r WHERE `+cond+
		` AND r.token_accounting = '`+AccountingComplete+`' LIMIT 2`, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var units []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return "", err
		}
		units = append(units, u)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(units) {
	case 0:
		return modelinfo.USD, nil
	case 1:
		return units[0], nil
	}
	return CurrencyMixed, nil
}

// currencyRoutes are the routes whose replies carry money figures. Mount wraps each in withCurrency,
// so every one of them says what its *_usd figures are in, from one query, without each handler
// doing it by hand.
var currencyRoutes = map[string]bool{
	"GET /api/stats": true, "GET /api/series": true, "GET /api/sessions": true,
	"GET /api/requests": true, "GET /api/cachestate": true,
	// The other tabs' money reads. The UI's unit is page-wide, so a tab that read none of these would
	// keep the unit of the page before it.
	"GET /api/components": true, "GET /api/breakdown": true, "GET /api/components/compaction-episodes": true,
	"GET /api/keepalive": true, "GET /api/keepalive/behaviour": true, "GET /api/keepalive/calc": true,
	"GET /api/keepalive/live": true, "GET /api/keepalive/recommend": true, "GET /api/keepalive/sessions": true,
	"GET /api/tools": true, "GET /api/tool-defer": true, "GET /api/toolfilter": true,
	"GET /api/kvcache": true, "GET /api/kvcache/rows": true, "GET /api/kvcache/simulate": true,
	"GET /api/kvcache/pricing": true, "GET /api/kvcache/suggest": true, "GET /api/kvcache/suggest/holdout": true,
	"GET /api/insights": true, "GET /api/spend/explain": true,
}

// withCurrency adds `currency` to the JSON object a money route answers with: the unit of the rows
// in the request's filter (DB.Currency). Any other answer (an error, a non-object) passes through
// unchanged. When the unit cannot be read, the reply says mixed: a money figure with a wrong unit
// is worse than none.
func (a *API) withCurrency(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &capture{h: http.Header{}, code: http.StatusOK}
		h(rec, r)
		body := rec.buf.Bytes()
		if rec.code == http.StatusOK && bytes.HasPrefix(body, []byte("{")) {
			if f, p, ok := a.scope(r); ok {
				body = addCurrency(body, a.viewCurrency(r, f, p))
			}
		}
		for k, v := range rec.h {
			w.Header()[k] = v
		}
		if rec.code != http.StatusOK {
			w.WriteHeader(rec.code)
		}
		_, _ = w.Write(body)
	}
}

// viewCurrency is DB.Currency for the request's filter, cached per principal and query.
func (a *API) viewCurrency(r *http.Request, f Filter, p Principal) string {
	key := cacheKey(p, r)
	if b, ok := a.currencyCache.get(key); ok {
		return string(b)
	}
	cur, err := a.db(r).Currency(f)
	if err != nil {
		slog.Warn("dash: the view's currency is unknown; money figures are hidden", "err", err)
		return CurrencyMixed // not cached: the next read tries again
	}
	a.currencyCache.set(key, []byte(cur))
	return cur
}

// addCurrency puts `"currency": cur` first in a JSON object.
func addCurrency(obj []byte, cur string) []byte {
	key, _ := json.Marshal(cur) // a string always marshals
	out := append([]byte(`{"currency":`), key...)
	rest := obj[1:]
	if t := bytes.TrimSpace(rest); !bytes.HasPrefix(t, []byte("}")) {
		out = append(out, ',')
	}
	return append(out, rest...)
}
