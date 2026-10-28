package counterfactual

import "sync"

// ThreadState is kept between requests and replaced after each one (§6.1).
type ThreadState struct {
	Active    []Operation  // rewrite and setting operations still applied (§4.6)
	PrevPrint *Fingerprint // this thread's previous request; nil = none (§5.2)
	Cache     CacheState   // what the previous request left cached; zero value = unknown
	Version   uint64       // +1 at each replace; the optimistic lock (R53)
}

// Tracker keeps the state of one thread between requests.
type Tracker struct {
	mu    sync.Mutex
	state ThreadState
}

// snapshot returns a copy of the thread's state. Callers must not share the slices.
func (t *Tracker) snapshot() ThreadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state
	s.Active = append([]Operation(nil), t.state.Active...)
	return s
}

// Trackers holds one Tracker for each thread. It is safe for concurrent use.
// A Tracker is never evicted, so memory grows with every thread for the life of the process. Whether
// and when to drop an idle thread is open, with persistence (design §11, question 13).
type Trackers struct {
	mu sync.Mutex
	m  map[string]*Tracker
}

// NewTrackers returns an empty set: every thread starts with no history.
func NewTrackers() *Trackers { return &Trackers{m: map[string]*Tracker{}} }

// ThreadKey is the key of a thread's Tracker: the session and the thread ID.
func ThreadKey(session string, t Thread) string { return session + "\x00" + t.ID }

func (ts *Trackers) tracker(key string) *Tracker {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.m[key]
	if !ok {
		t = &Tracker{}
		ts.m[key] = t
	}
	return t
}

// Request is the record of one request. It is private to that request: two requests of one thread
// in flight each have their own (R53).
type Request struct {
	trackers *Trackers

	Meta              RequestMeta
	Client            Body
	ClientBreakpoints []Breakpoint
	Ops               []Operation // as reported, in pipeline order

	// Set by EndRequest.
	Thread      Thread
	Forwarded   Body
	Breakpoints []Breakpoint
	Facts       EntryFacts
	start       ThreadState // the thread's state when EndRequest ran; start.Version is the lock
	ended       bool
}

// BeginRequest opens a record that belongs to no thread yet: the thread is known only at the end of
// the pass (§6.1). Call it before the plugins, with the client body before any plugin.
func (ts *Trackers) BeginRequest(meta RequestMeta, client Body, clientBreakpoints []Breakpoint) *Request {
	return &Request{trackers: ts, Meta: meta, Client: client, ClientBreakpoints: clientBreakpoints}
}

// Add records one reported operation. The framework sets Operator and DryRun first.
func (r *Request) Add(op Operation) { r.Ops = append(r.Ops, op) }

// EndRequest names the thread, records the forwarded body, its breakpoints and the facts only the
// host knows, and takes a copy of the thread's ThreadState. Call it after the last plugin.
func (r *Request) EndRequest(thread Thread, forwarded Body, breakpoints []Breakpoint, facts EntryFacts) {
	r.Thread = thread
	r.Forwarded = forwarded
	r.Breakpoints = breakpoints
	r.Facts = facts
	r.start = r.trackers.tracker(ThreadKey(r.Meta.Session, thread)).snapshot()
	r.ended = true
}

// replace stores next only if the state is still at version, and gives it version + 1 (R53).
func (t *Tracker) replace(version uint64, next ThreadState) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.Version != version {
		return false
	}
	next.Version = version + 1
	t.state = next
	return true
}
