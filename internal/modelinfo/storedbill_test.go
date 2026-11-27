package modelinfo

// storedBill is the old stored bill of one request, as a reference for the tests of this package: Price.Cost
// plus the one-hour write premium, added after. The same formula is in modelinfotest.StoredBill, which
// the other packages import. This package cannot import it: modelinfotest imports modelinfo, and a test of
// modelinfo that imports it would be an import cycle.
func storedBill(p Price, fresh, cacheRead, cacheWrite, output, cacheWrite1h int64) float64 {
	cost := p.Cost(fresh, cacheRead, cacheWrite, output)
	if cacheWrite1h > 0 {
		if premium := p.CacheWrite1hPremium(); premium > 0 {
			cost += float64(float64(cacheWrite1h) * premium) // no FMA; see Cost
		}
	}
	return cost
}
