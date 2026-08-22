package dash

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures the dashboard. The zero value is usable: an in-memory
// database, content capture on, 7-day / 512 MiB retention, loopback-only access
// to per-request content and effective config.
type Options struct {
	// DBPath is the SQLite file. "" or ":memory:" keeps everything in RAM (the
	// no-persistence mode), which is also the automatic fallback when the path
	// cannot be opened — the proxy must never fail to start over a dashboard.
	DBPath string
	// Retention bounds the store by age AND size. Zero values use the defaults
	// below; a negative value disables that rule.
	RetentionAge   time.Duration
	RetentionBytes int64
	// CaptureContent enables the before/after content capture the diff view needs.
	// Opt-OUT: it is the headline feature, so it defaults on. ContentCap bounds each
	// captured blob (default 16 KiB); ContentMaxPerRequest bounds how many
	// rewritten messages are captured per request (default 24).
	CaptureContent       bool
	ContentCap           int
	ContentMaxPerRequest int
	// QueueSize is the capture channel's depth (default 4096). When it is full,
	// events are DROPPED and counted rather than blocking a request.
	QueueSize int
	// BatchSize / FlushInterval control how the writer batches inserts.
	BatchSize     int
	FlushInterval time.Duration
	// TrustedCIDRs are the networks allowed to see per-request CONTENT and the
	// effective configuration. Loopback is always allowed. Aggregates are open to
	// everyone (a proxy people bind to 0.0.0.0 still wants its numbers visible).
	TrustedCIDRs []string
	// Mode is the proxy's operating mode ("active" | "observe"), served on /api/capture
	// so the UI can render the observe banner. Empty means active.
	//
	// There is deliberately no Preset field here: per-row preset labelling comes from
	// proxy.Options.Preset at the capture site (proxy/dashcapture.go), and a second copy
	// of the same value in a second Options struct is a copy that goes stale.
	Mode string
	// Effective is the resolved, already-structured configuration to serve at
	// /api/config. It is redacted before serving; nothing sensitive should be in
	// here in the first place.
	Effective map[string]any
	// BenchDirs are directories scanned for harbor benchmark runs (summary.json +
	// rows-*.json) at startup and on demand.
	BenchDirs []string

	// DiskHighWatermark is the fraction of the FILESYSTEM in use at which the
	// janitor starts evicting the oldest sessions; DiskLowWatermark is where it
	// stops. 0 uses the defaults (0.90 / 0.85); a negative high watermark disables
	// the rule.
	//
	// Two watermarks, not one. With a single threshold the janitor deletes and
	// reclaims on every pass forever once the host is full — and the host is usually
	// full for reasons that have nothing to do with us, so it would grind away
	// destroying history without ever fixing anything.
	DiskHighWatermark float64
	DiskLowWatermark  float64
	// MinKeepBytes floors how far the disk rule will shrink this database. Below it
	// the janitor stops and logs: if the filesystem is full because of something
	// else, deleting our last megabyte does not help anyone and the dashboard going
	// blank hides the problem instead of showing it.
	MinKeepBytes int64
	// MaxRowsPerTenant caps one tenant's retained request rows. Applied BEFORE the
	// disk rule, so a heavy user is trimmed to its own quota before anyone else's
	// history is touched. 0 = no per-tenant cap.
	MaxRowsPerTenant int64

	// Remote is cold storage (Box via rclone). When set, eviction becomes MIGRATION:
	// a session is uploaded and verified before its local rows are deleted, so
	// history is bounded by the remote's capacity rather than by this disk. nil
	// keeps the old behaviour, where eviction means deletion.
	Remote Remote
	// RemoteName is the CONFIGURED cold-storage name, set by the host even when the
	// boot reachability probe failed and Remote was therefore left nil. Without it the
	// dashboard cannot tell "no cold storage on this deployment" from "cold storage is
	// configured and currently unreachable" — and it reported the first while listing
	// archived sessions. Empty falls back to Remote.Describe().
	RemoteName string
	// ArchiveContentAfter moves a session's TRANSCRIPTS to cold storage once it has
	// been idle this long. Transcripts are the overwhelming majority of the bytes and
	// are read rarely, so moving them early is what keeps the local database small
	// enough that the disk rule never fires. 0 disables.
	ArchiveContentAfter time.Duration
	// ArchiveSessionAfter moves a WHOLE session out once it has been idle this long.
	// Should be well beyond ArchiveContentAfter: metric rows are small and worth
	// keeping locally queryable for as long as anyone might browse them. 0 disables.
	ArchiveSessionAfter time.Duration
	// ArchiveInterval is how often the archiver runs. 0 = defaultArchiveInterval.
	ArchiveInterval time.Duration
	// ArchiveBatch bounds sessions moved per pass, so one catch-up cycle cannot
	// spend an hour in rclone or exhaust the remote's API quota. 0 = default.
	ArchiveBatch int
	// ArchiveRequired refuses to delete a session under disk pressure unless it was
	// successfully archived first. Safer for data, dangerous for the host: with the
	// remote down and the disk full, nothing can be reclaimed and the filesystem
	// fills, which takes down every user's agent. Default false — reclaim and say
	// loudly what was lost.
	ArchiveRequired bool
}

const (
	defaultQueueSize      = 4096
	defaultBatchSize      = 128
	defaultFlushInterval  = 250 * time.Millisecond
	defaultRetentionAge   = 7 * 24 * time.Hour
	defaultRetentionBytes = 512 << 20
	defaultDiskHigh       = 0.90
	defaultDiskLow        = 0.85
	defaultMinKeepBytes   = 1 << 30 // 1 GiB
	defaultArchiveBatch   = 50
	// Every 15 minutes: this is a background trickle against a rate-limited API, not
	// something anyone is waiting for.
	defaultArchiveInterval = 15 * time.Minute
	defaultContentCap      = 16 << 10
	defaultContentPerReq   = 24
	pruneInterval          = 5 * time.Minute
)

func (o *Options) withDefaults() {
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = defaultFlushInterval
	}
	if o.RetentionAge == 0 {
		o.RetentionAge = defaultRetentionAge
	}
	if o.RetentionBytes == 0 {
		o.RetentionBytes = defaultRetentionBytes
	}
	if o.ContentCap == 0 {
		o.ContentCap = defaultContentCap
	}
	if o.ContentMaxPerRequest == 0 {
		o.ContentMaxPerRequest = defaultContentPerReq
	}
}

// Recorder is the capture pipeline: a buffered channel, one writer goroutine that
// batches inserts, and an SSE hub the writer fans summaries out to.
//
// The contract that matters: Record NEVER blocks and never returns an error. A
// full queue drops the event and increments a counter that the dashboard itself
// displays — an observability layer that silently lies about its own coverage is
// worse than one that admits a gap. This is why the dashboard cannot add request
// latency: the hot path does one channel send with a default branch.
type Recorder struct {
	db   *DB
	opts Options
	hub  *Hub

	ch   chan *Event
	done chan struct{}
	wg   sync.WaitGroup

	captured atomic.Int64
	dropped  atomic.Int64
	written  atomic.Int64
	errors   atomic.Int64

	// observeQueue is the host's accessor for its off-path pool counters, or nil in
	// sync mode. A func rather than a value so the counters are read at serve time.
	observeQueue atomic.Pointer[func() QueueStats]

	// Cache-attribution state: the last time we saw each session and whether we
	// have seen each model, so a cold start is never reported as a bust.
	mu        sync.Mutex
	lastSeen  map[string]int64  // session -> epoch ms of previous request
	lastTail  map[string]uint64 // session -> previous request's volatile-tail hash
	seenModel map[string]bool
	// perComp accumulates unique-savings dedup keys so a per-request unique figure
	// exists at capture time. Bounded; see markUnique.
	seenKeys map[string]struct{}
	// diskProbe overrides the filesystem usage probe. Injected only by tests: the
	// disk rule is unreachable otherwise, since a test cannot fill a real disk.
	diskProbe func(dir string) (float64, bool)
	// remote is cold storage, or nil.
	remote Remote
	// tenantQuota reads a tenant's own row quota from the control plane (see
	// SetTenantQuota). A func rather than a value because a manager can change it at
	// any time, and a pointer because it is wired after the recorder starts.
	tenantQuota atomic.Pointer[func(tenantID string) int64]
}

// NewRecorder opens the store and starts the writer goroutine. It never returns a
// fatal error for a bad path: an unopenable database degrades to in-memory, with a
// warning, because the proxy's job is to proxy.
func NewRecorder(opts Options) (*Recorder, error) {
	opts.withDefaults()
	db, err := Open(opts.DBPath)
	if err != nil {
		slog.Warn("dash: could not open the dashboard database; falling back to in-memory (history will not survive a restart)",
			"path", opts.DBPath, "err", err)
		db, err = Open(":memory:")
		if err != nil {
			return nil, err
		}
	}
	r := &Recorder{
		db:        db,
		opts:      opts,
		hub:       NewHub(),
		ch:        make(chan *Event, opts.QueueSize),
		done:      make(chan struct{}),
		lastSeen:  map[string]int64{},
		lastTail:  map[string]uint64{},
		seenModel: map[string]bool{},
		seenKeys:  map[string]struct{}{},
		remote:    opts.Remote,
	}
	r.wg.Add(1)
	go r.run()
	// The archiver gets its OWN goroutine. It must never share the writer's, because
	// an rclone round trip takes seconds and the writer owes the request path a fast
	// insert — a blocked writer means a full queue means dropped events, which is
	// observability failing precisely when the system is busy.
	if r.remote != nil && (opts.ArchiveContentAfter > 0 || opts.ArchiveSessionAfter > 0) {
		r.wg.Add(1)
		go r.archiveLoop()
	}
	// And the one-time prompt-text backfill, on its own goroutine for the same reason. It is
	// unconditional and needs no flag: it is a no-op (one indexed query) on a database that has
	// nothing left to move, which after the first run is every start. See dedupetext.go.
	r.wg.Add(1)
	go r.dedupeLoop()
	return r, nil
}

// archiveLoop runs the age-based archival passes until the recorder closes.
func (r *Recorder) archiveLoop() {
	defer r.wg.Done()
	every := r.opts.ArchiveInterval
	if every <= 0 {
		every = defaultArchiveInterval
	}
	// A context cancelled on shutdown, so an in-flight rclone transfer is abandoned
	// rather than holding Close open for its full timeout.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-r.done; cancel() }()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.archiveIdle(ctx)
		}
	}
}

// DB exposes the store for queries (read-only use by the API).
func (r *Recorder) DB() *DB { return r.db }

// Opts exposes the effective options (read-only).
func (r *Recorder) Opts() Options { return r.opts }

// Hub exposes the SSE fan-out.
func (r *Recorder) Hub() *Hub { return r.hub }

// Record hands an event to the writer. It is safe from any goroutine, never
// blocks, and never fails: a full queue drops and counts. Callers must not touch
// the event afterwards — the writer owns it.
func (r *Recorder) Record(e *Event) {
	if r == nil || e == nil {
		return
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	r.captured.Add(1)
	select {
	case r.ch <- e:
	default:
		r.dropped.Add(1)
	}
}

// Close stops the writer after draining what is already queued.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	close(r.done)
	r.wg.Wait()
	r.hub.Close()
	return r.db.Close()
}

// Stats reports the capture pipeline's own health — including its drops, which is
// the number that keeps every other number honest.
type Stats struct {
	Captured int64  `json:"captured"`
	Written  int64  `json:"written"`
	Dropped  int64  `json:"dropped"`
	Errors   int64  `json:"errors"`
	Queued   int    `json:"queued"`
	QueueCap int    `json:"queue_cap"`
	Clients  int    `json:"sse_clients"`
	DBPath   string `json:"db_path"`
	DBBytes  int64  `json:"db_bytes"`
	// Mode is the proxy's operating mode ("active" | "observe"). The UI renders an
	// unmissable banner in observe mode: every request was forwarded UNTOUCHED, so a
	// reader who mistakes these figures for enforced savings has drawn exactly the wrong
	// conclusion. That is worth a banner rather than a field on a detail tab.
	Mode string `json:"mode"`
	// ObserveQueue is the off-path measurement pool's counters, supplied by the host
	// (the pool lives in `modes`, above this package). Omitted when no pool is running,
	// so a sync deployment shows no phantom queue. Its `dropped` matters most: a drop is
	// an observation given up, so the projection UNDERSTATES what compaction would save.
	ObserveQueue *QueueStats `json:"observe_queue,omitempty"`
}

// QueueStats mirrors metrics.QueueStats / modes.Stats. Declared here rather than
// imported because the dependency runs the other way.
type QueueStats struct {
	Queued    int64 `json:"queued"`
	Pending   int64 `json:"pending"`
	Processed int64 `json:"processed"`
	Dropped   int64 `json:"dropped"`
	Errors    int64 `json:"errors"`
}

// SetObserveQueue lets the host publish its off-path pool's counters. Safe from any
// goroutine and safe to call on a nil Recorder.
func (r *Recorder) SetObserveQueue(fn func() QueueStats) {
	if r == nil {
		return
	}
	r.observeQueue.Store(&fn)
}

// Stats snapshots the pipeline counters.
func (r *Recorder) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	size, _ := r.db.sizeBytes()
	s := Stats{
		Captured: r.captured.Load(), Written: r.written.Load(),
		Dropped: r.dropped.Load(), Errors: r.errors.Load(),
		Queued: len(r.ch), QueueCap: cap(r.ch),
		Clients: r.hub.Clients(), DBPath: r.db.Path(), DBBytes: size,
		Mode: r.opts.Mode,
	}
	if s.Mode == "" {
		s.Mode = ModeActive
	}
	if fn := r.observeQueue.Load(); fn != nil {
		q := (*fn)()
		s.ObserveQueue = &q
	}
	return s
}

// run is the single writer goroutine: batch, insert in one transaction, fan out,
// and prune on a timer. Nothing else writes to the database.
func (r *Recorder) run() {
	defer r.wg.Done()
	batch := make([]*Event, 0, r.opts.BatchSize)
	flush := time.NewTicker(r.opts.FlushInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()

	write := func() {
		if len(batch) == 0 {
			return
		}
		// Redact on THIS goroutine, before the insert. This is the expensive half of
		// capture (nine regexes over up to ContentMaxPerRequest x 2 blobs) and it lives
		// here rather than at the capture site deliberately: `finish` is called from the
		// handler's defer, which runs before the handler returns, so redacting there makes
		// a keep-alive client's next request wait on it (~53 ms measured, ~25% of a
		// request). Nothing reaches the database unredacted either way — the security
		// property is the ordering against the INSERT, which this preserves.
		for _, e := range batch {
			e.Redact()
		}
		if err := r.db.insertBatch(batch); err != nil {
			r.errors.Add(int64(len(batch)))
			slog.Warn("dash: dropping a batch of captured requests", "n", len(batch), "err", err)
		} else {
			r.written.Add(int64(len(batch)))
			// The capture path is asynchronous, so "my request is not on the dashboard" has
			// three possible answers: never captured, dropped by a full channel (counted, and
			// WARNed above), or still sitting in this batch. This line is the third one.
			slog.Debug("dash: wrote a batch of captured requests", "n", len(batch),
				"written_total", r.written.Load(), "dropped_total", r.dropped.Load())
			for _, e := range batch {
				r.hub.Publish(e)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.opts.BatchSize {
				write()
			}
		case <-flush.C:
			write()
		case <-prune.C:
			r.janitorPass()
		case <-r.done:
			// Drain whatever is queued so a clean shutdown does not lose the tail.
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) >= r.opts.BatchSize {
						write()
					}
					continue
				default:
				}
				break
			}
			write()
			return
		}
	}
}

// Observe records the session/model facts needed for cache attribution and
// returns them. Called on the request path (one map lookup under a short mutex),
// before Record.
// tenant namespaces both maps. The session id already carries its tenant (see
// session.Scoped), but the MODEL name does not: model ids are shared vocabulary, so
// unscoped, one tenant's first-ever request for a model would be attributed as a
// warm cache because a DIFFERENT tenant had used it. That is both wrong and a small
// disclosure — it tells you which models other people are running.
func (r *Recorder) Observe(tenant, session, model string, now int64) (seenSession, seenModel bool, sinceLastMs int64) {
	seenSession, seenModel, sinceLastMs, _ = r.ObserveSplit(tenant, session, model, now, 0)
	return seenSession, seenModel, sinceLastMs
}

// ObserveSplit is Observe plus the volatile-tail comparison: tailChanged reports whether this
// request's tail differs from the previous request's in the same session, which is the turn on
// which the split is worth money (see dash.Event.cachesplitSavedUSD). A session's FIRST request
// counts as changed — there was nothing there to match.
//
// tailHash 0 means nothing split, and then tailChanged is false: there is no split to credit.
func (r *Recorder) ObserveSplit(tenant, session, model string, now int64, tailHash uint64) (seenSession, seenModel bool, sinceLastMs int64, tailChanged bool) {
	if r == nil {
		return true, true, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mk := tenant + "\x00" + model
	prev, seenSession := r.lastSeen[session]
	if seenSession {
		sinceLastMs = now - prev
	}
	seenModel = r.seenModel[mk]
	// Bound both maps: a proxy runs for weeks and every distinct session id would
	// otherwise be retained forever. Sessions are keyed by content hash or client id, so
	// the working set is small.
	//
	// By AGE, not by clearing the map. The old code emptied lastSeen entirely at 20,000
	// entries, which made every live session's next turn report a cold start at once —
	// and `seenSession` is no longer only a label: it decides whether a cache hit is
	// credited as ours (see Event.cachesplitSavedUSD), so a wholesale reset was a
	// wholesale over-claim. Dropping entries by age cannot do that: a session with no
	// request for staleSession really has lost its provider cache, so treating its next
	// turn as a cold start is not a guess, it is correct.
	if len(r.lastSeen) > 20000 {
		r.pruneSessionsLocked(now)
	}
	if len(r.seenModel) > 1000 {
		r.seenModel = map[string]bool{}
	}
	if tailHash != 0 {
		prevTail, had := r.lastTail[session]
		switch {
		case had:
			tailChanged = prevTail != tailHash
		case !seenSession:
			// Genuinely the first request of this session: nothing to match, so it counts as
			// moved. This is the same rule the read-time recomputation applies (see the SQL
			// in Overview, splitMoved), and the two now count the same thing.
			tailChanged = true
		default:
			// We have SEEN this session but do not remember its tail: this map is
			// process-scoped and pruned by age, so a proxy restart or an eviction lands here
			// mid-session. That is amnesia, not a moved snapshot, and treating it as one paid
			// real credit for nothing — measured, all three credited requests on the
			// production corpus were this case, and a faithful recomputation calls none of
			// them moved. Refusing the credit is the only safe direction.
			tailChanged = false
		}
		r.lastTail[session] = tailHash
	}
	r.lastSeen[session] = now
	r.seenModel[mk] = true
	return seenSession, seenModel, sinceLastMs, tailChanged
}

// staleSession is how long a session must be silent before its state is forgettable. Well
// past every provider cache TTL this code knows about (Anthropic 5m, or 1h with the extended
// beta), so a pruned session's next request genuinely is a cold start.
const staleSession = 2 * 60 * 60 * 1000

// pruneSessionsLocked drops sessions idle longer than staleSession. Caller holds r.mu.
//
// If that frees nothing — 20,000 sessions all active inside two hours, which would be a
// different kind of day — it falls back to emptying the map, because an unbounded map on a
// long-running proxy is the worse failure.
func (r *Recorder) pruneSessionsLocked(now int64) {
	for k, ts := range r.lastSeen {
		if now-ts > staleSession {
			delete(r.lastSeen, k)
			delete(r.lastTail, k)
		}
	}
	if len(r.lastSeen) > 20000 {
		r.lastSeen, r.lastTail = map[string]int64{}, map[string]uint64{}
	}
}

// SeedSessions primes the session-recency map from the database, so a RESTART does not make
// every live conversation's next turn look like the start of a new one.
//
// It matters because seenSession is not just a label any more: it is one of the three
// conditions that decide whether a cache read is credited as our saving, so a proxy restart
// used to hand out one bonus credit per live session — small, but exactly the kind of quiet
// over-claim the prefix-cache figure exists to avoid. It also fixes cache_miss_reason, which
// reported a cold start for every session in flight across a restart.
//
// One indexed group-by at startup, bounded to sessions young enough to still matter.
func (r *Recorder) SeedSessions(now int64) (int, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	// The tail hash of each session's LATEST request, alongside its recency: without it the
	// first turn after a restart reads as a tail change and earns a credit it did not.
	rows, err := r.db.sql.Query(`SELECT r.session_id, r.ts, r.split_tail_hash FROM requests r
		WHERE r.ts >= ? AND r.id = (SELECT r2.id FROM requests r2
			WHERE r2.session_id = r.session_id ORDER BY r2.ts DESC, r2.id DESC LIMIT 1)`,
		now-staleSession)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seeded := map[string]int64{}
	tails := map[string]uint64{}
	for rows.Next() {
		var id string
		var ts, tail int64
		// int64, not uint64: SQLite's INTEGER is signed, so a hash whose top bit is set
		// comes back negative and scanning it straight into a uint64 fails the whole
		// query — which it did on every start, silently costing the seeding this function
		// exists for ("could not recover session recency" in the log). The bits are the
		// same; only the Go type of the container differs.
		if err := rows.Scan(&id, &ts, &tail); err != nil {
			return 0, err
		}
		seeded[id] = ts
		if tail != 0 {
			tails[id] = uint64(tail)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, ts := range seeded {
		// Never overwrite something this process has already observed: that is newer by
		// construction, and its sinceLastMs is the one the request path measured.
		if _, ok := r.lastSeen[id]; !ok {
			r.lastSeen[id] = ts
			if t, ok := tails[id]; ok {
				r.lastTail[id] = t
			}
		}
	}
	return len(seeded), nil
}

// MarkUnique attributes a component's savings to NEW content only, deduping by
// the content keys the component stashed — the same rule metrics.Aggregator uses,
// so the dashboard's unique figure and /stats' agree. Returns saved tokens
// attributable to content not seen before.
// tenant namespaces the seen-key set, and it must. The keys are CONTENT hashes, so
// two tenants working on the same repository produce the same ones — unscoped, the
// second tenant's genuinely new saving is silently attributed as a repeat and
// reported as zero. Their dashboard would show the tool doing nothing.
func (r *Recorder) MarkUnique(tenant, component string, keys []string, saved int) int {
	if r == nil || saved <= 0 {
		return 0
	}
	if len(keys) == 0 {
		return saved // no key to dedup on: count the run once (Aggregator's rule)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// ponytail: clear-on-overflow, same reasoning as Observe. Over-reporting a
	// repeat as unique after a reset is bounded and visible via overcount_ratio.
	if len(r.seenKeys) > 200000 {
		r.seenKeys = map[string]struct{}{}
	}
	newKeys := 0
	for _, k := range keys {
		ck := tenant + "\x00" + component + "\x00" + k
		if _, seen := r.seenKeys[ck]; !seen {
			r.seenKeys[ck] = struct{}{}
			newKeys++
		}
	}
	if newKeys == 0 {
		return 0
	}
	return saved * newKeys / len(keys)
}
