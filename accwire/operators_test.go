package accwire

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
)

// The library exports no constant for the keep-alive ping's operator, so read it from PingOps: a
// rename in the library fails here, not in Prometheus as component="other".
func TestOpKeeperIsThePingOperator(t *testing.T) {
	ops, _ := accountant.PingOps(accountant.Ping{Session: "s", ThreadID: "main", Model: "m", At: time.Now()},
		accountant.Billing{Mode: accountant.BillingAPI})
	if len(ops.Ops) == 0 {
		t.Fatal("PingOps gave no operation")
	}
	for _, op := range ops.Ops {
		if op.Operator != OpKeeper {
			t.Errorf("PingOps books operator %q, OpKeeper is %q", op.Operator, OpKeeper)
		}
	}
}
