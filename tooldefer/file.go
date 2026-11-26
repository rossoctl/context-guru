package tooldefer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/store"
	_ "modernc.org/sqlite" // the driver behind OpenFile
)

// File is tool deferral's own on-disk state, for a proxy whose session store does not survive a
// restart (no dashboard database, the session state off, hosted mode). Without it, a restart loses
// every thread's record and moves deferred tools back into live cached prefixes: a full rewrite of
// every live conversation.
//
// It holds, in one SQLite file:
//   - each thread's record and each session's tool search opt-out (store keys cg:tdf: and cg:tso:,
//     tool names and a flag only), through a store.Memory with a metadata-only store.Disk;
//   - the tool history and the probe verdicts, when there is no dashboard database to hold them.
//
// Metadata only: tool names, session ids, project paths, flags, times. No tool text.
type File struct {
	DB    *sql.DB
	Store *store.Memory
	disk  *store.Disk
	once  sync.Once
}

// FileName is the file's name next to the dashboard database path.
const FileName = "context-guru-tooldefer.db"

// OpenFile opens (or creates) the file at path. retention bounds how long a row lives after its last
// write (the session-state retention).
func OpenFile(path string, retention time.Duration) (*File, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("tooldefer: no file path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// A write test: a read-only file or folder must fail here, not at the first flush.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tool_defer_meta (k TEXT PRIMARY KEY, v TEXT)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO tool_defer_meta(k, v) VALUES ('opened', ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		db.Close()
		return nil, err
	}
	d, err := store.OpenDisk(db, store.DiskOptions{MetadataOnly: true, Retention: retention})
	if err != nil {
		db.Close()
		return nil, err
	}
	mem := store.NewMemory(store.Options{StateTTLSeconds: int(store.DefaultStateTTL / time.Second)})
	mem.AttachDisk(d)
	return &File{DB: db, Store: mem, disk: d}, nil
}

// OpenForPurge opens an EXISTING file for a purge only: a plain connection, with no store.Disk
// writer and no write test, and it never creates the file (mode=rw). The caller closes it.
func OpenForPurge(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Close flushes and closes the file.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	var err error
	f.once.Do(func() {
		f.disk.Close()
		err = f.DB.Close()
	})
	return err
}
