package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// A thread's state lives in memory in the library. Without a save, every restart drops the active
// changes of every live session: the next request of each one is priced as an estimate (context-guru#768,
// major 2). The snapshot holds hashes and sizes, no message text (accountant persist.go).
const (
	// accStateMaxAge is how old a snapshot may be and still be restored. No cache entry outlives the longest
	// TTL (1h), so an older snapshot holds only expired state.
	accStateMaxAge = time.Hour
	// accStateEvery is how often the state is saved while running, so a kill -9 or a crash loses minutes, not all.
	accStateEvery = time.Minute
	// accStateVersion is the version of the proxy's own part of the state: how accBodies hashes a body. Change it
	// whenever that hash changes. The library snapshot has its own version inside. A file with another version is
	// ignored (issue #917). Version 1 was the unwrapped library snapshot, so a file from before the wrap fails the
	// check too. Version 2 is the wrap with the hash that leaves out cache marks, nested ones included.
	accStateVersion = 2
)

// accStateFile is the file on disk: the proxy version around the library snapshot.
type accStateFile struct {
	Version  int             `json:"cgStateVersion"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// lastSaved holds the bytes of the last file written, so a save with no change skips the write.
type lastSaved struct {
	mu   sync.Mutex
	data []byte
}

// save writes the snapshot to path through a temp file and a rename, so a crash mid-write leaves the old file.
// It syncs the temp file before the rename, so a power loss cannot leave an empty file under the final name.
// It writes nothing when the bytes equal the last file written.
func (s *savingsTracker) save(path string) error {
	snap, err := s.trackers.Snapshot()
	if err != nil {
		return err
	}
	data, err := json.Marshal(accStateFile{Version: accStateVersion, Snapshot: snap})
	if err != nil {
		return err
	}
	s.written.mu.Lock()
	defer s.written.mu.Unlock()
	if s.written.data != nil && bytes.Equal(s.written.data, data) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.written.data = data
	return nil
}

// removeStaleTemps deletes the temp files a crash between CreateTemp and Rename left beside the state file.
func removeStaleTemps(path string) {
	old, err := filepath.Glob(path + ".tmp*")
	if err != nil {
		return
	}
	for _, f := range old {
		if err := os.Remove(f); err == nil {
			slog.Info("accountant: removed a stale temp file", "path", f)
		}
	}
}

// restore loads a snapshot saved by save. It fails open: a missing, stale or unreadable file is logged
// and the proxy starts with empty state, as before. It reports whether it restored anything.
func (s *savingsTracker) restore(path string, now time.Time) bool {
	removeStaleTemps(path)
	fi, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("accountant: state file unreadable; starting empty", "path", path, "err", err)
		}
		return false
	}
	if age := now.Sub(fi.ModTime()); age > accStateMaxAge {
		slog.Info("accountant: state file is older than any cache entry; starting empty", "path", path, "age", age.Round(time.Second))
		return false
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var f accStateFile
		switch {
		case json.Unmarshal(data, &f) != nil:
			err = errors.New("not a state file")
		case f.Version != accStateVersion:
			err = errors.New("state file has proxy version " + strconv.Itoa(f.Version) + ", this proxy uses " + strconv.Itoa(accStateVersion))
		default:
			err = s.trackers.Restore(f.Snapshot)
		}
	}
	if err != nil {
		slog.Warn("accountant: state not restored; starting empty", "path", path, "err", err)
		return false
	}
	slog.Info("accountant: state restored", "path", path, "bytes", len(data))
	return true
}

// keepSaved saves the state every accStateEvery until stop is closed, then once more. It returns when done.
func (s *savingsTracker) keepSaved(path string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(accStateEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-stop:
			if err := s.save(path); err != nil {
				slog.Warn("accountant: state not saved at shutdown", "path", path, "err", err)
			}
			return
		}
		if err := s.save(path); err != nil {
			slog.Warn("accountant: state not saved", "path", path, "err", err)
		}
	}
}
