package proxy

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/accrec"
)

// The format of the state and the rules for when it may be restored are accrec's (state.go). This file
// holds the file I/O: where the state goes, how it is written and when it is saved.
const (
	accStateMaxAge    = accrec.StateMaxAge
	accStateEvery     = accrec.StateEvery
	accStateVersion   = accrec.StateVersion
	accThinkingMaxAge = accrec.ThinkingMaxAge
)

// accStateFile is the file on disk: the proxy version around the library snapshot.
type accStateFile = accrec.StateFile

// lastSaved holds the bytes of the last file written, so a save with no change skips the write.
type lastSaved struct {
	mu   sync.Mutex
	data []byte
}

// save writes the snapshot to path through a temp file and a rename, so a crash mid-write leaves the old file.
// It syncs the temp file before the rename, so a power loss cannot leave an empty file under the final name.
// It writes nothing when the bytes equal the last file written.
func (s *savingsTracker) save(path string) error {
	data, err := s.SnapshotState()
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
// and the proxy starts with empty state, as before. It reports whether it restored the snapshot. The
// thinking counts are restored on their own: from a file up to accThinkingMaxAge old, and also when the
// snapshot is refused (accrec.Recorder.RestoreState holds the rules).
func (s *savingsTracker) restore(path string, now time.Time) bool {
	removeStaleTemps(path)
	fi, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("accountant: state file unreadable; starting empty", "path", path, "err", err)
		}
		return false
	}
	age := now.Sub(fi.ModTime())
	if age > accThinkingMaxAge {
		slog.Info("accountant: state file is older than any cache entry; starting empty", "path", path, "age", age.Round(time.Second))
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("accountant: state not restored; starting empty", "path", path, "err", err)
		return false
	}
	res := s.RestoreState(data, age)
	if res.ThinkingErr != nil {
		slog.Warn("accountant: thinking counts not restored", "path", path, "err", res.ThinkingErr)
	}
	if res.TooOld {
		slog.Info("accountant: state file is older than any cache entry; only the thinking counts are restored", "path", path, "age", age.Round(time.Second))
		return false
	}
	if res.Err != nil {
		slog.Warn("accountant: state not restored; starting empty", "path", path, "err", res.Err)
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
