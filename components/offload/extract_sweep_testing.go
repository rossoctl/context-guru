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
	func() {
		dropMu.Lock()
		defer dropMu.Unlock()
		addMarkerOwner(st, marker, threadKey, ck)
	}()
}

// ResetSweepLedgerForTest clears the process-wide quality ledger. The ledger is global, like the ping
// counters, so a test that drives a drop must clear it again or it leaks into /stats for every test
// that follows in the same process.
func ResetSweepLedgerForTest() { resetSweepLedgerForTest() }

// ResetV3ForTest clears sweep v3's ledger, for the host's tests.
func ResetV3ForTest() { resetV3ForTest() }

// V3DryRunCutsForTest is the number of would-be cuts a dry run has booked.
func V3DryRunCutsForTest() int64 { return v3Counters().DryRunCuts }

// V3CutsForTest is the number of cuts v3 has applied.
func V3CutsForTest() int64 { return v3Counters().Cuts }

// DropRecordsForTest is how many drop records the sweep has written for a thread, from its own drop index.
func DropRecordsForTest(st store.Store, threadKey string) int {
	return len(getDropIndex(st, threadKey).Cks)
}
