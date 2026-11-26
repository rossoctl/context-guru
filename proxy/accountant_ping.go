package proxy

import (
	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/components"
)

// PingStore is accrec.PingStore. The same instance is given to the ping emitter (writer)
// and to the Handler (reader) through Options.Pings.
type PingStore = accrec.PingStore

// NewPingStore returns an empty store.
func NewPingStore() *PingStore { return accrec.NewPingStore() }

// PingSavings prices one keep-alive ping and stores its facts in st. The rules are accrec.PingSavings's.
func PingSavings(r components.KeepAliveReport, st *PingStore) (accountant.Result, bool) {
	// A nil *PingStore is a no-op: its methods are nil-safe.
	return accrec.PingSavings(r, st, accrec.PingCalls{Ops: pingOps, Standalone: accountantStandalone})
}

// BookPing is PingSavings plus the savings counters: the always-on path of a ping, with or without the dashboard
// (accrec.BookPing). obs may be nil.
func BookPing(r components.KeepAliveReport, st *PingStore, obs acmetrics.Observer) (accountant.Result, bool) {
	return accrec.BookPing(r, st, accrec.PingCalls{Ops: pingOps, Standalone: accountantStandalone}, obs)
}

// pingOps and accountantStandalone are the two library calls PingSavings makes. Tests replace them to make them panic.
var (
	pingOps              = accountant.PingOps
	accountantStandalone = accountant.ComputeStandalone
)
