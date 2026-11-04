package proxy

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
)

// save writes the snapshot to path through a temp file and a rename, so a crash mid-write leaves the old file.
func (s *savingsTracker) save(path string) error {
	data, err := s.trackers.Snapshot()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// restore loads a snapshot saved by save. It fails open: a missing, stale or unreadable file is logged
// and the proxy starts with empty state, as before. It reports whether it restored anything.
func (s *savingsTracker) restore(path string, now time.Time) bool {
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
		err = s.trackers.Restore(data)
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
