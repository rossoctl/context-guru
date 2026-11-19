package waitcause

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/tidwall/gjson"
)

// The work bounds of ScanBody. They bound the WORK, not the time: a body over either bound is not read
// at all, and the caller falls back to the counts the tracker holds. 8 MiB is above any body the
// dashboard writer scanned in measurements (783 KB was the largest in #424's benchmark, ~11 ms).
const (
	MaxScanBytes    = 8 << 20
	MaxScanMessages = 20000
)

// Scan is the open-task facts of ONE request body, read without the Tracker (#1225, #1227).
//
// ScanBody is a pure function of the body: it writes no shared state, so it can run while the request is
// upstream, on any goroutine, in any order. The Tracker keeps its own, ordered reading of the same body
// (the dashboard writer); the keeper plans from the Scan at response time instead of from the Tracker's
// counts, which lag by one request. A Scan carries the normalized messages so the writer can reuse them
// and read the body once.
type Scan struct {
	// OpenBackground and OpenTeammate count the tasks this body alone shows as open, with every task's
	// start marker still in the body. A task whose start fell out of a compacted body is not counted here;
	// the Tracker's persistent counts cover it (the keeper takes the larger of the two).
	OpenBackground int
	OpenTeammate   int
	entries        []bschemas.ChatMessage
}

// Messages returns the normalized message list the scan read. The caller must not change it.
func (s *Scan) Messages() []bschemas.ChatMessage {
	if s == nil {
		return nil
	}
	return s.entries
}

// ScanBody reads the open background tasks and teammate waits of one request body. It returns nil when it
// cannot say, and the caller then uses today's path:
//
//   - the body is over MaxScanBytes, or its message list over MaxScanMessages (the work bound);
//   - the body is empty, has no message list, or continues server-held history (previous_response_id);
//   - stop reports true (the request ended, so the answer is no longer needed);
//   - anything panics.
//
// stop is polled before and after the normalization and once per message, so the work in progress when it
// turns true is at most one message. nil never stops. No timer decides anything.
func ScanBody(stop func() bool, provider bschemas.ModelProvider, body []byte) (res *Scan) {
	defer func() {
		if recover() != nil {
			res = nil // fail open
		}
	}()
	if len(body) == 0 || len(body) > MaxScanBytes || (stop != nil && stop()) {
		return nil
	}
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return nil // server-held history: this body is only the newest turn
	}
	entries := apply.NormalizeMessages(provider, body)
	if len(entries) == 0 || len(entries) > MaxScanMessages || (stop != nil && stop()) {
		return nil
	}
	now := time.Now()
	ts := newThreadState()
	if !scanOpenState(ts, entries, now, stop) {
		return nil
	}
	bg, _ := countBackgroundLocked(ts, now) // ts is private to this call: no lock needed
	return &Scan{OpenBackground: bg, OpenTeammate: countTeammatesLocked(ts, now), entries: entries}
}
