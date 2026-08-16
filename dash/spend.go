package dash

import (
	"database/sql"
	"errors"
	"time"
)

// MonthToDateUSD returns a tenant's billed cost since the start of the current
// calendar month, for the spend cap.
//
// Calendar month rather than a rolling 30 days, because a budget is something a person
// reconciles against a monthly invoice — a rolling window means the number a user sees
// never matches the number finance sees.
//
// Includes cg_llm_cost_usd: context-guru's OWN compaction spend goes on the same
// upstream credential, so a tenant that enables extract_llm is spending the
// organisation's money through us and the cap has to see it.
//
// It reads the tenant_spend ROLLUP, not a SUM over the requests table, and that is the
// whole point. Retention — the age rule, the byte budget, the per-tenant row quota, the
// disk rule — deletes request rows, so a sum over them is a spend figure a tenant can
// RESET by spending more: 120k requests worth $50 against a 100k-row quota evicts the
// oldest 20k rows, the sum drops back under the cap, and traffic resumes. The rollup is
// incremented in the insert transaction and nothing ever deletes from it.
//
// New failure modes, stated plainly:
//
//   - It is MONOTONIC within a month. Deleting rows by hand, or restoring a database,
//     does not refund spend; only the calendar turning over resets it. A cap raised in
//     error has to be raised, not cleaned up.
//   - It counts what the recorder WROTE. Events dropped by a full capture queue are
//     not billed — the same under-count the old SUM had, and /api/capture reports it.
//   - It lives in the dashboard database. The in-memory fallback (an unwritable path)
//     starts the month's total at zero, so the cap under-counts for the rest of that
//     month; it logs loudly. A schema-version bump no longer does this — Open carries
//     this table across from the preserved file (see carryNonDerived).
//   - A missing row means zero, not an error: a tenant that has not spent yet and a
//     tenant whose month just turned over are the same fact.
//
// A lookup error still returns an error and the caller still FAILS OPEN on it
// (proxy.checkSpend) — deliberately unchanged. The cap is a budget guard, not a
// security boundary, and a broken query is our problem rather than a user's.
func (d *DB) MonthToDateUSD(tenantID string) (float64, error) {
	var usd float64
	err := d.sql.QueryRow(`SELECT usd FROM tenant_spend WHERE tenant_id = ? AND month = ?`,
		tenantID, monthKey(time.Now().UnixMilli())).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return usd, err
}
