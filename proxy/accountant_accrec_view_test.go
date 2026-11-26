package proxy

import (
	"github.com/rossoctl/context-guru/accrec"
	"github.com/rossoctl/context-guru/schema"
)

// The accountant's glue is in accrec (#1303). These are the proxy's old names for it, so the tests keep one spelling.
var (
	billedBody     = accrec.BilledBody
	deferredStub   = accrec.DeferredStub
	deferredNames  = accrec.DeferredNames
	accPingKey     = accrec.PingKey
	pingFromReport = accrec.PingFromReport
)

func responsesServerHeldHistory(body []byte) bool { return schema.ServerHeldHistory(body) }
