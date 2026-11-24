package accrec

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// A thread's state lives in memory in the library. Without a save, every restart drops the active
// changes of every live session: the next request of each one is priced as an estimate (context-guru#768,
// major 2). The snapshot holds hashes and sizes, no message text (accountant persist.go).
// This file holds the format and the rules of that state. The host reads and writes the file.
const (
	// StateMaxAge is how old a snapshot may be and still be restored. No cache entry outlives the longest
	// TTL (1h), so an older snapshot holds only expired state.
	StateMaxAge = time.Hour
	// StateEvery is how often a host should save the state while running, so a kill -9 or a crash loses minutes, not all.
	StateEvery = time.Minute
	// StateVersion is the version of the host's own part of the state: how accwire hashes a body. Change it
	// whenever that hash changes. The library snapshot has its own version inside. A file with another version is
	// ignored (issue #917). Version 1 was the unwrapped library snapshot, so a file from before the wrap fails the
	// check too. Version 2 is the wrap with the hash that leaves out cache marks, nested ones included.
	StateVersion = 2
	// ThinkingMaxAge is how old a state file may be and still give back its thinking counts. A count is
	// keyed by a signature's hash, not by a time, and a block can come back long after its cache entry expired
	// (it is billed as fresh input then), so the counts outlive the snapshot's hour.
	ThinkingMaxAge = 24 * time.Hour
)

// StateFile is the file on disk: the host version around the library snapshot.
type StateFile struct {
	Version  int             `json:"cgStateVersion"`
	Snapshot json.RawMessage `json:"snapshot"`
	// Thinking is the count of each thinking block (Recorder.Thinking): a key and a number each. A file
	// written before it has none, and restores with an empty table.
	Thinking json.RawMessage `json:"thinking,omitempty"`
}

// SnapshotState returns the bytes a host writes to its state file.
func (s *Recorder) SnapshotState() ([]byte, error) {
	snap, err := s.Trackers.Snapshot()
	if err != nil {
		return nil, err
	}
	thinking, err := json.Marshal(s.Thinking)
	if err != nil {
		return nil, err
	}
	return json.Marshal(StateFile{Version: StateVersion, Snapshot: snap, Thinking: thinking})
}

// RestoreResult is what RestoreState did, for the host to log. It fails open: Restored is false for a
// state it could not use, and the host starts with empty state.
type RestoreResult struct {
	// Restored is true when the snapshot was loaded.
	Restored bool
	// ThinkingErr is set when the thinking counts were bad. They only refine a price, so the snapshot is still read.
	ThinkingErr error
	// TooOld is true when the snapshot was older than StateMaxAge: only the thinking counts were restored.
	TooOld bool
	// Err is why the state was not restored, when it was not and TooOld is false.
	Err error
}

// RestoreState loads the bytes SnapshotState made. age is how old the file is; the host checks it against
// ThinkingMaxAge before it reads the file. The thinking counts are restored on their own: from a file up to
// ThinkingMaxAge old, and also when the snapshot is refused.
func (s *Recorder) RestoreState(data []byte, age time.Duration) RestoreResult {
	var f StateFile
	var res RestoreResult
	switch {
	case json.Unmarshal(data, &f) != nil:
		res.Err = errors.New("not a state file")
	case f.Version != StateVersion:
		res.Err = errors.New("state file has proxy version " + strconv.Itoa(f.Version) + ", this proxy uses " + strconv.Itoa(StateVersion))
	}
	if res.Err == nil && len(f.Thinking) > 0 {
		// The counts only refine a price, so a bad table is dropped, and the snapshot is still read.
		if terr := json.Unmarshal(f.Thinking, s.Thinking); terr != nil {
			res.ThinkingErr = terr
		}
	}
	if res.Err == nil && age > StateMaxAge {
		res.TooOld = true
		return res
	}
	if res.Err == nil {
		res.Err = s.Trackers.Restore(f.Snapshot)
	}
	res.Restored = res.Err == nil
	return res
}
