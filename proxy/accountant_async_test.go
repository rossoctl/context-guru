package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/thread"
)

// Issue #1059: accBegin and accEnd ran on the request goroutine, so a request paid for parsing and hashing
// its own body before the first byte went upstream. These tests pin that the work runs off the request path,
// that a failure in it costs the saving only, and that the saving still reaches the row.

// asyncCapture is a capture with a savings tracker, the way newCapture builds one with the dashboard on.
func asyncCapture() *capture {
	return &capture{savings: newSavingsTracker(), provider: "anthropic"}
}

func asyncMeta() accountant.RequestMeta {
	return accountant.RequestMeta{Session: "sess-async", Model: "claude-sonnet-5", At: time.Now()}
}

// An accountant that blocks must not block the request. onEnd runs inside the accountant's end step, so a
// test that holds it holds the accountant.
func TestAccEndDoesNotWaitForTheAccountantWork(t *testing.T) {
	c := asyncCapture()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	c.savings.onEnd = func(_ *accountant.Request, _ endRecord) {
		entered <- struct{}{}
		<-release
	}
	defer close(release)
	body := manyToolResultsRequest("claude-sonnet-5", 24)

	returned := make(chan struct{})
	go func() {
		c.accBegin(asyncMeta(), body)
		c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("accEnd waited for the accountant: the request path pays for the work")
	}
}

// The guard issue #1059 asked for: on a large body, without -race, what the request goroutine pays before the
// first byte goes upstream (accBegin and accEnd; finish waits for the job later, after the response) must be a
// small part of the accountant's work. The ratio keeps the test honest on a slow or loaded runner, where an
// absolute limit would flake. A synchronous accountant pays the whole of it and fails.
func TestRequestPathCostDoesNotGrowWithBodySize(t *testing.T) {
	body := manyToolResultsRequest("claude-sonnet-5", 24) // about 670 KB
	const runs = 15

	median := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}

	// What the accountant costs: the same calls, made directly.
	var work []time.Duration
	for i := 0; i < runs; i++ {
		s := newSavingsTracker()
		start := time.Now()
		req := s.begin(asyncMeta(), body)
		s.end(req, accountant.Thread{ID: "main"}, body, nil, accountant.EntryFacts{TTL: 5 * time.Minute})
		work = append(work, time.Since(start))
	}

	// What the request pays: the capture's calls. Each run waits for the background work, so runs do not overlap.
	var path []time.Duration
	for i := 0; i < runs; i++ {
		c := asyncCapture()
		done := make(chan struct{})
		c.savings.onEnd = func(_ *accountant.Request, _ endRecord) { close(done) }
		start := time.Now()
		c.accBegin(asyncMeta(), body)
		c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
		path = append(path, time.Since(start))
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the accountant work never finished")
		}
	}

	w, p := median(work), median(path)
	if p > w/4 {
		t.Errorf("the request path pays %v of an accountant that costs %v: want under a quarter", p, w)
	}
}

// The accountant fails open. A panic in its background work must reach neither the request nor the process.
func TestAccountantPanicOffThePathCostsTheSavingOnly(t *testing.T) {
	c := asyncCapture()
	done := make(chan struct{})
	c.savings.onEnd = func(_ *accountant.Request, _ endRecord) {
		defer close(done)
		panic("boom")
	}
	body := manyToolResultsRequest("claude-sonnet-5", 2)

	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("a panic in the accountant reached the request path: %v", p)
			}
		}()
		c.accBegin(asyncMeta(), body)
		c.accEnd(thread.Result{ID: thread.Primary}, body, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the accountant work never ran")
	}
	// A panic on another goroutine would have ended the test binary by now. Give it the time to do so.
	time.Sleep(50 * time.Millisecond)
}

// Through the real handler: the request reaches the upstream while the accountant is still working, and the
// row still carries the saving once the work is done, because finish waits for it before it prices the request.
func TestUpstreamIsReachedBeforeTheAccountantFinishes(t *testing.T) {
	const accountantDelay = 2 * time.Second
	var reached atomic.Int64 // unix nanos at which the upstream saw the request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		reached.Store(time.Now().UnixNano())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":200,"output_tokens":1,"cache_creation_input_tokens":300}}`)
	}))
	defer up.Close()
	h, _, tap := accHandlerYAML(t, up.URL, "pipeline: [extract]\n")
	var accountantDone atomic.Int64 // unix nanos at which the accountant's work ended
	h.savings.onEnd = func(_ *accountant.Request, _ endRecord) {
		time.Sleep(accountantDelay)
		accountantDone.Store(time.Now().UnixNano())
	}
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
		strings.NewReader(string(manyToolResultsRequest("claude-sonnet-5", 4))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-context-guru-session", "sess-async-handler")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Compare with the moment the accountant ended, not with a fixed limit: a slow runner delays the
	// request by an unknown time, but the upstream must still be reached before the accountant is done.
	if done := accountantDone.Load(); done != 0 && reached.Load() >= done {
		t.Errorf("the upstream saw the request %v after the accountant ended: it waited for the accountant (%v)",
			time.Duration(reached.Load()-done), accountantDelay)
	}
	// waitForRows polls for about 2 s, the same as the accountant's delay, so poll longer here.
	for i := 0; i < 2000 && len(tap.all()) < 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if len(tap.all()) < 1 {
		t.Fatal("the row was never written")
	}
	if _, ok := tap.all()[0].AccountantResult(); !ok {
		t.Error("the row carries no accountant result: finish priced the request before the background work ended")
	}
}
