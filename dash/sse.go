package dash

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// sseWriteTimeout is how long a client has to accept an event before it is
// considered dead and evicted. A browser tab that is suspended, or a curl that
// stopped reading, must not be able to stall the writer goroutine — which would
// stall persistence, which would stall nothing on the request path but would still
// silently stop the dashboard. Bounded per-client buffering plus this timeout
// makes a hung client the hung client's own problem.
const sseWriteTimeout = 2 * time.Second

// sseClientBuffer is how many events a slow-but-alive client may fall behind by
// before it is dropped.
const sseClientBuffer = 64

// Hub fans captured events out to SSE clients. Published events carry SUMMARY
// rows only — no before/after content, ever. The live feed is a monitoring
// surface; content is fetched deliberately, per request, through the access-gated
// detail route.
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	nextID  uint64
	closed  bool
	// ring is a small backlog so a reconnecting client with Last-Event-ID gets the
	// gap replayed instead of silently losing it. Bounded on purpose: a client that
	// was away longer than the ring reloads from /api/requests, which is the
	// authoritative history.
	ring    []*Event
	ringCap int
}

type client struct {
	ch     chan *Event
	closed chan struct{}
	once   sync.Once
	// tenant is the only tenant this client may see; all is a manager's
	// service-wide feed. Filtering happens at FAN-OUT, not in the browser: a live
	// feed that ships every tenant's session ids and models to every connected
	// dashboard has already leaked them, whatever the client then chooses to render.
	tenant string
	all    bool
}

// wants reports whether this client should receive an event.
func (c *client) wants(e *Event) bool { return c.all || e.TenantID == c.tenant }

func (c *client) stop() { c.once.Do(func() { close(c.closed) }) }

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{clients: map[*client]struct{}{}, ringCap: 256}
}

// Clients reports the current subscriber count.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Publish sends one event to every live client, dropping (and evicting) any
// client whose buffer is full. Called only from the writer goroutine.
func (h *Hub) Publish(e *Event) {
	// Summary only: strip content before it can reach a live-feed client.
	sum := *e
	sum.Content = nil
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.ring = append(h.ring, &sum)
	if len(h.ring) > h.ringCap {
		h.ring = h.ring[len(h.ring)-h.ringCap:]
	}
	var evict []*client
	for c := range h.clients {
		if !c.wants(&sum) {
			continue
		}
		select {
		case c.ch <- &sum:
		default:
			evict = append(evict, c) // buffer full: the client is not keeping up
		}
	}
	for _, c := range evict {
		delete(h.clients, c)
	}
	h.mu.Unlock()
	for _, c := range evict {
		c.stop()
	}
}

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	cs := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	h.clients = map[*client]struct{}{}
	h.mu.Unlock()
	for _, c := range cs {
		c.stop()
	}
}

// backlogSince returns ring events with an id greater than lastID (the
// Last-Event-ID a reconnecting browser sends), so a reconnect backfills the gap
// instead of pretending nothing happened while it was away.
// The ring is shared across clients, so the REPLAY has to be filtered exactly like
// the live fan-out. Filtering one and not the other is the natural bug here, and it
// would leak on every dashboard reconnect rather than continuously — which is worse,
// because it would not show up in a casual test.
func (h *Hub) backlogSince(lastID int64, c *client) []*Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Event
	for _, e := range h.ring {
		if e.ID > lastID && c.wants(e) {
			out = append(out, e)
		}
	}
	return out
}

func (h *Hub) subscribe(tenant string, all bool) (*client, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	c := &client{ch: make(chan *Event, sseClientBuffer), closed: make(chan struct{}),
		tenant: tenant, all: all}
	h.clients[c] = struct{}{}
	return c, true
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.stop()
}

// ServeHTTP streams events as SSE. It honors Last-Event-ID (header or the
// ?last_event_id= query param, for clients that cannot set headers), enforces a
// per-write timeout, and evicts itself on a stalled write.
// ServeHTTP serves the service-wide feed. Correct for a single-tenant proxy and
// for a manager; the hosted dashboard mounts ServeScoped instead.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ServeScoped(w, r, "", true)
}

// ServeScoped streams only one tenant's events (or everything, for a manager).
func (h *Hub) ServeScoped(w http.ResponseWriter, r *http.Request, tenant string, all bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	lastID := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		lastID, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("last_event_id"); v != "" {
		lastID, _ = strconv.ParseInt(v, 10, 64)
	}

	c, ok := h.subscribe(tenant, all)
	if !ok {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.unsubscribe(c)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Backfill the gap first, in order, so the client's view is continuous.
	for _, e := range h.backlogSince(lastID, c) {
		if !writeEvent(w, flusher, e) {
			return
		}
	}

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.closed:
			return
		case e := <-c.ch:
			if !writeEvent(w, flusher, e) {
				return
			}
		case <-keepalive.C:
			// A comment frame keeps intermediaries from reaping an idle stream.
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent writes one SSE frame under a deadline. A write that does not
// complete in sseWriteTimeout means the client is not reading; we give up on it
// rather than block. Uses ResponseController so the deadline applies to the
// underlying connection, not just to our own bookkeeping.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, e *Event) bool {
	rc := http.NewResponseController(w)
	// SetWriteDeadline is unsupported on some ResponseWriters (e.g. httptest's);
	// that is fine — the timeout is a safety net, not a correctness requirement.
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	b, err := json.Marshal(e)
	if err != nil {
		return true // skip a malformed event; do not kill the stream
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: request\ndata: %s\n\n", e.ID, b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
