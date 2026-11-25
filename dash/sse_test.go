package dash

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/testutil"
)

func TestHubFansOutAndStripsContent(t *testing.T) {
	h := NewHub()
	defer h.Close()
	c1, _ := h.subscribe("", true)
	c2, _ := h.subscribe("", true)
	if h.Clients() != 2 {
		t.Fatalf("clients = %d; want 2", h.Clients())
	}

	e := &Event{ID: 7, SessionID: "s", TokensBefore: 100, TokensAfter: 50}
	e.Content = []ContentRow{{Path: "messages.1", Before: "a customer's source code", After: "x"}}
	h.Publish(e)

	for i, c := range []*client{c1, c2} {
		got := awaitSignal(t, fmt.Sprintf("client %d to receive the event", i), c.ch)
		if got.ID != 7 {
			t.Errorf("client %d got id %d", i, got.ID)
		}
		// The live feed is a monitoring surface; content is fetched deliberately
		// through the access-gated detail route, never pushed.
		if len(got.Content) != 0 {
			t.Errorf("client %d received request CONTENT over SSE: %+v", i, got.Content)
		}
	}
	// The caller's event must not have been mutated by the strip.
	if len(e.Content) != 1 {
		t.Error("Publish mutated the caller's event")
	}
}

// TestHubEvictsHungClient is the test the issue names: a subscriber that stops
// reading must be dropped, not allowed to wedge the writer goroutine.
func TestHubEvictsHungClient(t *testing.T) {
	h := NewHub()
	defer h.Close()
	hung, _ := h.subscribe("", true)
	live, _ := h.subscribe("", true)

	// Overflow the hung client's buffer while draining the live one.
	drained := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range live.ch {
			drained++
			if drained > sseClientBuffer*3 {
				return
			}
		}
	}()

	start := time.Now()
	for i := 0; i < sseClientBuffer*4; i++ {
		h.Publish(&Event{ID: int64(i + 1)})
		time.Sleep(time.Millisecond) // let the live reader keep up
	}
	elapsed := time.Since(start)

	// Publishing must never have blocked on the hung client. The wall-clock check is a timing
	// check; a Publish that really blocked would not return at all.
	if testutil.TimingEnabled() && elapsed > 10*time.Second {
		t.Fatalf("Publish stalled behind a hung client (%v)", elapsed)
	}
	awaitSignal(t, "the hung client to be evicted (a client that never read must be evicted)", hung.closed)
	if h.Clients() > 1 {
		t.Errorf("clients = %d after eviction; want at most 1", h.Clients())
	}
	<-done
}

func TestHubBacklogHonorsLastEventID(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 1; i <= 5; i++ {
		h.Publish(&Event{ID: int64(i)})
	}
	got := h.backlogSince(3, &client{all: true})
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 5 {
		t.Fatalf("backlog after id 3 = %v; want ids 4,5", ids(got))
	}
	if all := h.backlogSince(0, &client{all: true}); len(all) != 5 {
		t.Errorf("backlog from 0 = %d events; want 5", len(all))
	}
	if none := h.backlogSince(99, &client{all: true}); len(none) != 0 {
		t.Errorf("backlog past the head = %d; want 0", len(none))
	}
}

func ids(evs []*Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestHubRingIsBounded(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 1; i <= h.ringCap*3; i++ {
		h.Publish(&Event{ID: int64(i)})
	}
	if len(h.ring) > h.ringCap {
		t.Errorf("ring grew to %d; cap is %d", len(h.ring), h.ringCap)
	}
	// It must retain the NEWEST events, not the oldest.
	if h.ring[len(h.ring)-1].ID != int64(h.ringCap*3) {
		t.Errorf("ring head = %d; want the newest event", h.ring[len(h.ring)-1].ID)
	}
}

func TestSSEEndpointStreamsAndBackfills(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 1; i <= 3; i++ {
		h.Publish(&Event{ID: int64(i), SessionID: "s"})
	}
	srv := httptest.NewServer(http.HandlerFunc(h.ServeHTTP))
	defer srv.Close()

	// Reconnect claiming to have seen id 1: ids 2 and 3 must be backfilled.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?last_event_id=1", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}

	// Publish one more so the stream carries a live event after the backfill. Wait until the
	// handler has subscribed the client, so the event is live and not a backfill.
	testutil.WaitFor(t, "the handler subscribes the client", func() bool { return h.Clients() == 1 })
	h.Publish(&Event{ID: 4, SessionID: "s"})

	// The read blocks until a line arrives, so it runs in a goroutine with a bound on the wait.
	// The deferred resp.Body.Close ends the goroutine if the stream stalls.
	got := make(chan []string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var seen []string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "id: ") {
				seen = append(seen, strings.TrimPrefix(line, "id: "))
			}
			if len(seen) >= 3 {
				break
			}
		}
		got <- seen
	}()
	seen := awaitSignal(t, "the stream to carry 3 event ids", got)
	want := []string{"2", "3", "4"}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Errorf("event ids = %v; want %v (backfill then live)", seen, want)
	}
}

func TestHubCloseDisconnectsEveryone(t *testing.T) {
	h := NewHub()
	c, _ := h.subscribe("", true)
	h.Close()
	awaitSignal(t, "Close to disconnect the client", c.closed)
	if _, ok := h.subscribe("", true); ok {
		t.Error("subscribe succeeded after Close")
	}
	// Publishing after Close must be a harmless no-op, not a panic.
	h.Publish(&Event{ID: 1})
}

// TestHubConcurrent drives publish, subscribe and unsubscribe at once; run under
// -race this is the mandatory SSE-hub check.
func TestHubConcurrent(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.Publish(&Event{ID: int64(i), SessionID: "s"})
		}
	}()
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				c, ok := h.subscribe("", true)
				if !ok {
					return
				}
				// Read a little, then leave — the churn a browser tab actually produces.
				select {
				case <-c.ch:
				case <-time.After(time.Millisecond):
				}
				h.unsubscribe(c)
				_ = h.Clients()
				_ = h.backlogSince(int64(i), &client{all: true})
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	h.Close()
}

// THE RING DOES NOT KEEP AN EVENT'S CLOSURES. Each one captures its request's whole body, and the
// ring holds the last 256 events for late subscribers: kept closures pinned 256 bodies.
func TestPublishDropsTheBodyHoldingClosures(t *testing.T) {
	h := NewHub()
	body := make([]byte, 1<<20)
	e := &Event{ID: 1, TenantID: "t",
		WaitCause:   func() WaitCauseResult { _ = body; return WaitCauseResult{} },
		LifecycleFn: func() *LifecycleRow { _ = body; return nil },
		BypassCount: func() (int, int) { _ = body; return 0, 0 }}
	h.Publish(e)
	h.mu.Lock()
	kept := h.ring[len(h.ring)-1]
	h.mu.Unlock()
	if kept.WaitCause != nil || kept.LifecycleFn != nil || kept.BypassCount != nil {
		t.Errorf("the ring kept a closure that captures the request body: %+v", kept)
	}
	if e.WaitCause == nil || e.LifecycleFn == nil || e.BypassCount == nil {
		t.Error("Publish cleared the CALLER's closures; it must only strip its own copy")
	}
}
