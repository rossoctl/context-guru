package dash

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
		select {
		case got := <-c.ch:
			if got.ID != 7 {
				t.Errorf("client %d got id %d", i, got.ID)
			}
			// The live feed is a monitoring surface; content is fetched deliberately
			// through the access-gated detail route, never pushed.
			if len(got.Content) != 0 {
				t.Errorf("client %d received request CONTENT over SSE: %+v", i, got.Content)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d received nothing", i)
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

	// Publishing must never have blocked on the hung client.
	if elapsed > 10*time.Second {
		t.Fatalf("Publish stalled behind a hung client (%v)", elapsed)
	}
	select {
	case <-hung.closed:
		// evicted, as required
	case <-time.After(time.Second):
		t.Error("a client that never read was not evicted")
	}
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

	// Publish one more so the stream carries a live event after the backfill.
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.Publish(&Event{ID: 4, SessionID: "s"})
	}()

	sc := bufio.NewScanner(resp.Body)
	var seen []string
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seen = append(seen, strings.TrimPrefix(line, "id: "))
		}
		if len(seen) >= 3 {
			break
		}
	}
	want := []string{"2", "3", "4"}
	if len(seen) != 3 || seen[0] != want[0] || seen[1] != want[1] || seen[2] != want[2] {
		t.Errorf("event ids = %v; want %v (backfill then live)", seen, want)
	}
}

func TestHubCloseDisconnectsEveryone(t *testing.T) {
	h := NewHub()
	c, _ := h.subscribe("", true)
	h.Close()
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		t.Error("Close did not disconnect the client")
	}
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
