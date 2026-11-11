package proxy

import (
	"context"

	"github.com/rossoctl/context-guru/agent/bob"
	"github.com/tidwall/gjson"
)

// bobUsage exists only so the five older tests in bobusage_test.go that call the parser directly
// keep compiling unchanged, now that the parser is agent/bob's Bob.ReadUsage. It wraps the block
// as a JSON reply and reads it through the host's own path (readAgentUsage). The real proof of the
// move is the proxy end-to-end tests: TestBobRouteRowReadsCacheWrites,
// TestBobRouteRowReadsCacheWritesOnTheBufferedPath and TestNonBobRoutesIgnoreBobCacheWrites.
func bobUsage(u gjson.Result, base Usage) Usage {
	return readAgentUsage(context.Background(), bob.Bob{}, "application/json", []byte(`{"usage":`+u.Raw+`}`), base)
}
