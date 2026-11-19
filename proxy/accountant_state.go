package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
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
	// Thinking is the count of each thinking block (savingsTracker.thinking): a key and a number each. A file
	// written before it has none, and restores with an empty table.
	Thinking json.RawMessage `json:"thinking,omitempty"`
	// Config is accConfigKey of the proxy that wrote the file. A snapshot from another configuration is not restored.
	Config string `json:"config,omitempty"`
}

// accConfigKey names the configuration whose changes a snapshot holds, hashed: the pipeline's components and flags,
// the configuration document, and the inputs that decide each declaration the proxy adds. The library ends a change
// that ADDED content (the expand or adjudicate declaration) only when the client edits before it. A proxy started
// with another configuration may no longer add it, so the restored change stays active, and every later request of
// that thread fails the sum check and books no saving (#1169). The key holds no request data.
//
// The two declarations follow the rules on the request path (proxy.go): expand is added when the mode allows it, the
// store persists, and under auto the pipeline can offload (expand.Inject); adjudicate when extract_llm_sweep runs.
// persists is the store's Persists, which the store flag sets outside the document.
func accConfigKey(pipe *components.Pipeline, persists bool, injectExpand, doc string) string {
	if injectExpand == "" {
		injectExpand = expand.InjectAuto
	}
	addsExpand := injectExpand != expand.InjectNever && persists && (injectExpand != expand.InjectAuto || pipe.HasOffload())
	return docHash(fmt.Sprint(pipe.Names(), pipe.TailBreakpoint(), pipe.InBandBreakpoint(), pipe.FixedRestore(),
		injectExpand, addsExpand, pipe.Has("extract_llm_sweep"), doc))
}

// accConfigMixed is the key of state that holds the changes of two configurations: a live swap (an adapt apply or
// revert) changed the pipeline after the state was restored. No start computes it, so no start restores that state.
const accConfigMixed = "mixed"

// setConfig names the configuration whose changes the state holds. Before openConfig (the start, where an adapt
// override may replace the start pipeline) the key is replaced. After it, a different key means the state now holds
// the changes of both configurations, so the next save writes accConfigMixed. Nil-safe.
func (s *savingsTracker) setConfig(key string) {
	if s == nil {
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.configOpen && key != s.config {
		key = accConfigMixed
	}
	s.config = key
}

// openConfig ends the start: the key is the running configuration's, and the state is restored under it. Nil-safe.
func (s *savingsTracker) openConfig() {
	if s == nil {
		return
	}
	s.configMu.Lock()
	s.configOpen = true
	s.configMu.Unlock()
}

func (s *savingsTracker) configKey() string {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.config
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
	thinking, err := json.Marshal(s.thinking)
	if err != nil {
		return err
	}
	data, err := json.Marshal(accStateFile{Version: accStateVersion, Snapshot: snap, Thinking: thinking, Config: s.configKey()})
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

// accThinkingMaxAge is how old a state file may be and still give back its thinking counts. A count is
// keyed by a signature's hash, not by a time, and a block can come back long after its cache entry expired
// (it is billed as fresh input then), so the counts outlive the snapshot's hour.
const accThinkingMaxAge = 24 * time.Hour

// restore loads a snapshot saved by save. It fails open: a missing, stale or unreadable file is logged
// and the proxy starts with empty state, as before. It reports whether it restored the snapshot. The
// thinking counts are restored on their own: from a file up to accThinkingMaxAge old, and also when the
// snapshot is refused.
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
	var f accStateFile
	if err == nil {
		switch {
		case json.Unmarshal(data, &f) != nil:
			err = errors.New("not a state file")
		case f.Version != accStateVersion:
			err = errors.New("state file has proxy version " + strconv.Itoa(f.Version) + ", this proxy uses " + strconv.Itoa(accStateVersion))
		}
	}
	if err == nil && len(f.Thinking) > 0 {
		// The counts only refine a price, so a bad table is logged and dropped, and the snapshot is still read.
		if terr := json.Unmarshal(f.Thinking, s.thinking); terr != nil {
			slog.Warn("accountant: thinking counts not restored", "path", path, "err", terr)
		}
	}
	if err == nil && age > accStateMaxAge {
		slog.Info("accountant: state file is older than any cache entry; only the thinking counts are restored", "path", path, "age", age.Round(time.Second))
		return false
	}
	if err == nil && f.Config != s.configKey() {
		slog.Info("accountant: state file is from another configuration; only the thinking counts are restored", "path", path)
		return false
	}
	if err == nil {
		err = s.trackers.Restore(f.Snapshot)
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
