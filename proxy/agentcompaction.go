package proxy

import (
	"github.com/rossoctl/accountant"

	"github.com/rossoctl/context-guru/thread"
)

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
