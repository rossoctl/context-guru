package accrec

import (
	"log/slog"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/components"
)

// PingSink keeps the facts of a priced ping for the thread's next real request, and counts the pings priced. The
// host owns the store. A nil sink is allowed.
type PingSink interface {
	Put(key string, f accountant.PingFacts)
	NotePriced()
}

// PingCalls are the two library calls PingSavings makes. A host passes its own to wrap or replace them, as a
// test does to make them panic. A zero value uses the library's.
type PingCalls struct {
	Ops        func(accountant.Ping, accountant.Billing) (accountant.RequestOps, accountant.PingFacts)
	Standalone func(accountant.RequestOps) accountant.Result
}

// PingFromReport maps a keeper report to the library's Ping. r.CostUSD is deliberately not
// carried: the host prices the ping with its own rates.
func PingFromReport(r components.KeepAliveReport) accountant.Ping {
	return accountant.Ping{
		Session: r.Session, ThreadID: r.Thread, Model: r.Model, Host: r.Route,
		Usage: accountant.CallUsage{Usage: accountant.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead),
			CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)}, Output: int(r.Output)},
		At: pingStart(r), Window: r.Window,
		// The TTL the ping asked: 1 hour for a 1-hour-on-wait ping (every marker relabelled). A plain ping
		// leaves it 0 (not known): the library then judges the entry by the previous request's markers.
		TTL: oneHourIf(r.OneHour),
	}
}

// pingStart is when the ping began. The report's TS is when it was recorded, after the response, and the library
// measures the entry gap from the START of a request or ping, as the provider measures the TTL (accountant design
// §5.2; PingFacts.At is "when the ping started"). A ping that streams for minutes would otherwise shorten the gap.
func pingStart(r components.KeepAliveReport) time.Time {
	return time.UnixMilli(r.TS).Add(-time.Duration(r.DurationMs * float64(time.Millisecond)))
}

// PingSavings prices one keep-alive ping as a spend operation and stores its facts in st for the
// thread's next real request. ok is false, and nothing is stored, for a ping that is not
// converted: no Window (its ChangeID would be shared by every such ping), a status outside
// 200-299, or no usage (it billed nothing). Manual and sweep-substitute pings are converted.
//
// A MANUAL ping works like a scheduled one: its facts are stored, and the accountant credits the rescue on the
// thread's next real request (#1460). Its row books no estimate (ManualSavedUSD), so the write it avoided is
// counted once, as that rescue. Before #1460 the row booked the user's estimate and the facts were not stored,
// because both together counted the write twice (context-guru#768 B3: $0.0200 booked, about $0.0096 true).
//
// A panic in the library is recovered and logged, and the ping is reported as not converted. The keeper
// runs this in its own goroutine, so an unrecovered panic would end the process (issue #918).
func PingSavings(r components.KeepAliveReport, st PingSink, calls PingCalls) (res accountant.Result, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("accountant: ping pricing panicked; the ping is recorded with no saving", "panic", p)
			res, ok = accountant.Result{}, false
		}
	}()
	if calls.Ops == nil {
		calls.Ops = accountant.PingOps
	}
	if calls.Standalone == nil {
		calls.Standalone = accountant.ComputeStandalone
	}
	if r.Window.IsZero() || r.Status < 200 || r.Status > 299 ||
		r.FreshInput+r.CacheRead+r.CacheWrite+r.CacheWrite1h+r.Output == 0 {
		return accountant.Result{}, false
	}
	ops, facts := calls.Ops(PingFromReport(r), accountant.Billing{Mode: accountant.BillingAPI})
	res = calls.Standalone(ops)
	if st != nil {
		st.NotePriced()
	}
	if st != nil {
		st.Put(PingKey(r.Session, ThreadID(r.Thread)), facts)
	}
	return res, true
}

// PingObservation is the savings-counter observation of a priced ping. Output stays unset on purpose: a ping
// books only a spend operation, and its output is priced through the spend call.
func PingObservation(r components.KeepAliveReport, res accountant.Result) acmetrics.Observation {
	return acmetrics.Observation{
		Meta: accountant.RequestMeta{RequestID: "", Session: r.Session, Model: r.Model,
			At: time.UnixMilli(r.TS), Billing: accountant.Billing{Mode: accountant.BillingAPI}},
		Usage: accountant.Usage{Input: int(r.FreshInput), CacheRead: int(r.CacheRead),
			CacheWrite: int(r.CacheWrite), CacheWrite1h: int(r.CacheWrite1h)},
		Result: res,
	}
}

// BookPing is the always-on path of a ping (#1457). It prices the ping (PingSavings), stores its facts for the
// thread's next real request, and books the result into the savings counters. It needs no dashboard. ok is false
// when the ping is not converted, and then nothing is booked. A panic in the sink costs the metric only: the
// result is returned either way. obs may be nil.
func BookPing(r components.KeepAliveReport, st PingSink, calls PingCalls, obs acmetrics.Observer) (accountant.Result, bool) {
	res, ok := PingSavings(r, st, calls)
	if !ok || obs == nil {
		return res, ok
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("metrics: ping Observe panicked; this ping books no savings metric", "panic", p)
			}
		}()
		obs.Observe(PingObservation(r, res))
	}()
	return res, ok
}

// oneHourIf is time.Hour when on, else 0.
func oneHourIf(on bool) time.Duration {
	if on {
		return OneHourTTL
	}
	return 0
}
