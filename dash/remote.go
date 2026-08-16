package dash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cold storage for the dashboard's history.
//
// The design decision worth writing down: this drives rclone as a SUBPROCESS and
// never mounts anything. A FUSE mount of an object store looks like the obvious
// answer — point DASHBOARD_DB at ~/mnt/box and get unlimited space for free — and it
// is the wrong answer for a database. SQLite needs POSIX byte-range locking and an
// fsync that means something; a FUSE-over-HTTPS layer provides neither, and rclone's
// own mount documentation excludes database files for exactly that reason. The
// failure mode is not slow queries, it is silent corruption. Beyond that, every page
// read would be an HTTPS round trip, so a dashboard query touching a few thousand
// pages would take minutes, and SQLite's WAL checkpointing would run straight into
// Box's API rate limits.
//
// So: the live database stays on local disk, where it is small (a metrics row is
// well under a kilobyte) and correct. What goes to Box is WHOLE COLD SESSIONS as
// single compressed objects — write-once, read-rarely, one object per session rather
// than per request, which is also what keeps the API call count sane.

// Remote is the cold-storage backend. Small on purpose: whole objects in and out,
// no partial reads, no seeking — because that is all archival needs, and anything
// richer would invite someone to try using it as a filesystem.
type Remote interface {
	// Put writes an object, overwriting any existing one.
	Put(ctx context.Context, path string, data []byte) error
	// Get reads an object whole.
	Get(ctx context.Context, path string) ([]byte, error)
	// Size reports an object's size, for confirming an upload landed before the
	// local copy is deleted.
	Size(ctx context.Context, path string) (int64, error)
	// Delete removes an object. Absent is not an error.
	Delete(ctx context.Context, path string) error
	// Describe names the destination, for logs and the dashboard.
	Describe() string
}

// ErrRemoteMissing means the object is not there — distinguishable from a transport
// failure, because "this session was never archived" and "Box is down" call for very
// different responses.
var ErrRemoteMissing = errors.New("dash: object not found in cold storage")

// Rclone drives the rclone binary. It holds no credential: rclone's own config file
// owns the OAuth token, which is also what lets a token refresh happen without this
// process knowing anything about it.
type Rclone struct {
	// Bin is the rclone executable ("rclone" by default).
	Bin string
	// Base is the remote root, e.g. "box:context-guru". The remote name and its
	// credentials live in rclone's config, not here.
	Base string
	// ConfigPath is rclone's config file. Set it explicitly for a service: under
	// systemd, $HOME is not what an interactive shell had, and a remote rclone
	// cannot find looks exactly like a remote that is empty.
	ConfigPath string
	// Timeout bounds one transfer. 0 = DefaultRemoteTimeout.
	Timeout time.Duration
	// BWLimit is passed to --bwlimit (e.g. "8M"). Empty = unlimited. Worth setting:
	// rclone will otherwise use all the upload bandwidth it can get, and this box is
	// also serving everybody's agent traffic.
	BWLimit string

	// mu serialises transfers. Box rate-limits per account, and the archiver has no
	// reason to run uploads in parallel — it is a background trickle, not a race.
	mu sync.Mutex
}

// DefaultRemoteTimeout bounds a single object transfer. Generous: a large session
// archive over a slow link is normal, and a spurious timeout means either a retry
// storm or, worse, a local delete that never had a confirmed upload behind it.
const DefaultRemoteTimeout = 5 * time.Minute

func (r *Rclone) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "rclone"
}

func (r *Rclone) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultRemoteTimeout
}

// args builds a command line with the global flags every call needs.
func (r *Rclone) args(rest ...string) []string {
	a := []string{"--config", r.ConfigPath}
	if r.ConfigPath == "" {
		a = a[:0] // let rclone find its own config
	}
	if r.BWLimit != "" {
		a = append(a, "--bwlimit", r.BWLimit)
	}
	// Machine-readable failures, no progress bar, no interactive prompt: this runs
	// unattended and must never block waiting for a human.
	a = append(a, "--use-json-log", "--log-level", "ERROR", "--retries", "3")
	return append(a, rest...)
}

func (r *Rclone) full(path string) string {
	return strings.TrimRight(r.Base, "/") + "/" + strings.TrimLeft(path, "/")
}

func (r *Rclone) Describe() string { return r.Base }

// Put streams data to an object via `rclone rcat`, which takes the body on stdin —
// so nothing is staged in a temporary file that a crash could leave behind.
func (r *Rclone) Put(ctx context.Context, path string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), r.args("rcat", r.full(path))...)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rclone rcat %s: %w: %s", path, err, trimErr(stderr.String()))
	}
	return nil
}

func (r *Rclone) Get(ctx context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), r.args("cat", r.full(path))...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if notFound(err, stderr.String()) {
			return nil, ErrRemoteMissing
		}
		return nil, fmt.Errorf("rclone cat %s: %w: %s", path, err, trimErr(stderr.String()))
	}
	return out.Bytes(), nil
}

// Size confirms an object exists and how big it is. `lsjson --stat` is the one
// rclone subcommand that answers "is this exact object there" without listing a
// directory, which matters when a tenant's archive folder has thousands of entries.
func (r *Rclone) Size(ctx context.Context, path string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), r.args("lsjson", "--stat", r.full(path))...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if notFound(err, stderr.String()) || isNotFound(out.String()) {
			return 0, ErrRemoteMissing
		}
		return 0, fmt.Errorf("rclone lsjson %s: %w: %s", path, err, trimErr(stderr.String()))
	}
	// Parse just the Size field rather than the whole document: this is one scalar
	// out of a stable JSON object, and a full struct would be four more fields to
	// keep in step with rclone's output for no gain.
	s := out.String()
	i := strings.Index(s, `"Size"`)
	if i < 0 {
		return 0, ErrRemoteMissing
	}
	rest := s[i+len(`"Size"`):]
	rest = strings.TrimLeft(rest, ": \t")
	end := 0
	for end < len(rest) && (rest[end] == '-' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	n, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("rclone lsjson %s: unparseable Size", path)
	}
	return n, nil
}

func (r *Rclone) Delete(ctx context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), r.args("deletefile", r.full(path))...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if notFound(err, stderr.String()) {
			return nil // already gone: the caller's intent is satisfied
		}
		return fmt.Errorf("rclone deletefile %s: %w: %s", path, err, trimErr(stderr.String()))
	}
	return nil
}

// Check verifies the remote is usable, at startup, so a misconfigured remote is a
// log line on boot rather than a surprise the first time the archiver runs — by
// which point it would be reported as "archiving failed" with no hint that the
// remote was never reachable at all.
func (r *Rclone) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), r.args("lsd", r.Base)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A missing base directory is fine — rcat creates the path on first write.
		if notFound(err, stderr.String()) {
			return nil
		}
		return fmt.Errorf("rclone cannot reach %s: %w: %s", r.Base, err, trimErr(stderr.String()))
	}
	return nil
}

// notFound decides whether a failed rclone call means "the object is not there".
//
// The EXIT CODE is the reliable signal: rclone documents 3 as directory-not-found and
// 4 as file-not-found, distinct from 2 (uncategorised) and 5 (temporary). Discovered
// the hard way — `lsjson --stat` on a missing object exits 3 having printed nothing at
// all to stderr, so a message-only check silently reported every missing archive as a
// transport failure. String matching stays as a fallback for the subcommands that do
// explain themselves, because getting this backwards is expensive in both directions:
// a transport failure read as "never archived" makes a Box outage look like data loss,
// and a genuine miss read as a failure makes the archiver retry forever.
func notFound(err error, stderr string) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 3, 4:
			return true
		}
	}
	return isNotFound(stderr)
}

// isNotFound recognises rclone's several ways of SAYING an object is not there, for
// the calls that print a reason.
func isNotFound(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{
		"directory not found", "object not found", "not found",
		"no such file", "error 404", "notfound",
	} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// trimErr keeps a stderr excerpt short enough to log. rclone's JSON log lines are
// verbose and repeat themselves; the first few hundred characters carry the cause.
func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// memRemote is an in-memory Remote for tests. It lives here rather than in a test
// file so the archiver's tests and the API's tests can both use it.
type memRemote struct {
	mu      sync.Mutex
	objects map[string][]byte
	// failPut / failGet let a test exercise the paths that matter most: what happens
	// when the upload fails (the local copy must survive) and when the download
	// fails (an archived session must report unavailable, not empty).
	failPut, failGet error
	puts             int
}

func newMemRemote() *memRemote { return &memRemote{objects: map[string][]byte{}} }

func (m *memRemote) Put(_ context.Context, path string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut != nil {
		return m.failPut
	}
	m.puts++
	m.objects[path] = append([]byte(nil), data...)
	return nil
}

func (m *memRemote) Get(_ context.Context, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet != nil {
		return nil, m.failGet
	}
	b, ok := m.objects[path]
	if !ok {
		return nil, ErrRemoteMissing
	}
	return append([]byte(nil), b...), nil
}

func (m *memRemote) Size(_ context.Context, path string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[path]
	if !ok {
		return 0, ErrRemoteMissing
	}
	return int64(len(b)), nil
}

func (m *memRemote) Delete(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, path)
	return nil
}

func (m *memRemote) Describe() string { return "mem:" }
