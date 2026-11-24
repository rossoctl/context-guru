package proxy

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
)

// The accountant reads each wire in the accwire package (#1164). These are the host's names for
// the calls, so the callers and tests keep one spelling. Each picks the wire of the body once.

func accBodies(raw []byte) accountant.Body { return accwire.For(raw).Body(raw) }

func accBreakpoints(raw []byte) []accountant.Breakpoint { return accwire.For(raw).Breakpoints(raw) }

func accParse(raw []byte) (accountant.Body, []accountant.Breakpoint) {
	return accwire.For(raw).Parse(raw, nil)
}

func accParseCounted(raw []byte, count accountant.OpaqueCount) (accountant.Body, []accountant.Breakpoint) {
	return accwire.For(raw).Parse(raw, count)
}

func envelopeOps(client, forwarded []byte) []accountant.Operation {
	return accwire.For(client).EnvelopeOps(client, forwarded)
}

func messageOps(operator string, before, after []byte) []accountant.Operation {
	return accwire.For(before).MessageOps(operator, before, after)
}

// withoutCacheMarks and findCacheMark are the hash's rule for a cache mark. See accwire.
var (
	withoutCacheMarks = accwire.WithoutCacheMarks
	findCacheMark     = accwire.FindCacheMark
)
