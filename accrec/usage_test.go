package accrec

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
)

// Usage.Accountant is the one conversion to the library's type; ReportUsage reads a keeper report.
func TestUsageAccountantMapsEveryField(t *testing.T) {
	got := Usage{FreshInput: 1, CacheRead: 2, CacheWrite: 3, CacheWrite1h: 4}.Accountant()
	if want := (accountant.Usage{Input: 1, CacheRead: 2, CacheWrite: 3, CacheWrite1h: 4}); got != want {
		t.Fatalf("Accountant() = %+v, want %+v", got, want)
	}
	r := components.KeepAliveReport{FreshInput: 5, CacheRead: 6, CacheWrite: 7, CacheWrite1h: 8}
	if got, want := ReportUsage(r).Accountant(), (accountant.Usage{Input: 5, CacheRead: 6, CacheWrite: 7, CacheWrite1h: 8}); got != want {
		t.Fatalf("ReportUsage = %+v, want %+v", got, want)
	}
	if p := PingFromReport(r); p.Usage.Usage != ReportUsage(r).Accountant() {
		t.Fatalf("PingFromReport usage = %+v", p.Usage.Usage)
	}
	if o := PingObservation(r, accountant.Result{}); o.Usage != ReportUsage(r).Accountant() {
		t.Fatalf("PingObservation usage = %+v", o.Usage)
	}
}
