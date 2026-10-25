package offload

import (
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/store"
)

// SeedSweepDropForTest records, as a cold apply would, that the sweep dropped `content` on a thread
// and stashed it under the marker id `marker`. For the host's tests, which drive an expand through the
// real proxy and cannot reach the unexported record.
func SeedSweepDropForTest(st store.Store, threadKey, content, marker string) {
	ck := extract.ContentKey(content)
	putDrop(st, sweepDropRec{Thread: threadKey, ToolUseID: "toolu_seed", CK: ck, Marker: marker, Tokens: 1})
	addToDropIndex(st, threadKey, []string{ck})
	dropMu.Lock()
	addMarkerOwner(st, marker, threadKey, ck)
	dropMu.Unlock()
}

// ResetSweepLedgerForTest clears the process-wide quality ledger. The ledger is global, like the ping
// counters, so a test that drives a drop must clear it again or it leaks into /stats for every test
// that follows in the same process.
func ResetSweepLedgerForTest() { resetSweepLedgerForTest() }
