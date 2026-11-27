// Package modelinfotest holds REFERENCE formulas for tests. No production code may import it.
//
// The accountant prices every bill (accountant.RequestCost). A test that pins the accountant to the formula
// that priced the same request before imports the reference from here, so the old formula has one test-only
// home and no production caller.
package modelinfotest

import "github.com/rossoctl/context-guru/internal/modelinfo"

// StoredBill is the OLD stored bill of one request: modelinfo.Price.Cost (each product rounded on its own, so
// no CPU fuses a multiply and an add, #1128) plus the one-hour write premium on the tokens billed at the
// 1-hour tier. Every write token is priced at the 5-minute rate, and the premium (1-hour rate minus 5-minute
// rate, never below 0) is added after. p holds the rates at the request's prompt size (Price.At).
//
// accountant.RequestCost must give the same bits. This function exists to test that. Do not call it from
// production code.
func StoredBill(p modelinfo.Price, fresh, cacheRead, cacheWrite, output, cacheWrite1h int64) float64 {
	cost := p.Cost(fresh, cacheRead, cacheWrite, output)
	if cacheWrite1h > 0 {
		if premium := p.CacheWrite1hPremium(); premium > 0 {
			cost += float64(float64(cacheWrite1h) * premium) // no FMA; see Price.Cost
		}
	}
	return cost
}
