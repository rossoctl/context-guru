package proxy

import (
	"github.com/rossoctl/accountant"

	"github.com/rossoctl/context-guru/thread"
)

// The proxy detects the agent's own compaction request with thread.IsAgentCompaction.
//
// The proxy does NOT bypass such a request. Bypass forwards the original history while the
// provider cache holds the history context-guru rewrote on earlier turns, so a bypassed
// compaction missed the cache and paid for the whole request again. The request goes through
// the pipeline like any other. The summary model therefore sees the rewritten history
// (shortened tool outputs, <<cg:HASH>> markers), not the full original.
//
// The match is still used for: thread identification (package thread), the larger body
// limit (readGatedBody), the dashboard row (replay cap), and the agent_compaction counter in
// /stats. A compaction body over maxRequestBytes is the one case that is bypassed (see chat).
// The match no longer changes how a request is treated, so a false positive (#533) costs only
// a wrong count and a wrong thread mark, except for that over-size case.

// compactionVerdict is what the proxy tells the accountant about a client compaction (R55) and
// stores on the dashboard row (#947): "unknown" on the first request after the agent's own
// compaction, "" on any other request. The wire does not yet tell a manual /compact from an
// auto compaction, so the verdict is never "manual" or "auto".
func compactionVerdict(th thread.Result) accountant.CompactionTrigger {
	if th.AfterCompaction {
		return accountant.CompactionUnknown
	}
	return ""
}
