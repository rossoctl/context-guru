// Package keepalivetest is the exported contract test for keepalive.Dispatcher. Any host
// implementation calls RunDispatcherContract from its own test package to verify it.
package keepalivetest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/keepalive"
)

// Harness is one host's dispatcher under test, plus the host-side hook the keeper's Record.Hold
// would call. Hold receives the request headers the host captures the credential from.
type Harness struct {
	D    keepalive.Dispatcher
	Hold func(k keepalive.Key, reqHeaders http.Header)
}

// NewHarness builds a dispatcher whose pings go to upstreamURL (scheme://host, no path).
type NewHarness func(t *testing.T, upstreamURL string) Harness

const usageJSON = `{"usage":{"input_tokens":1,"cache_read_input_tokens":4000,` +
	`"cache_creation_input_tokens":0,"output_tokens":1}}`

// RunDispatcherContract is what every keep-alive host's Dispatcher must satisfy. Phase 3's
// Cortex PR runs it against cortexDispatcher; a context-guru change that breaks it fails here
// first.
func RunDispatcherContract(t *testing.T, newH NewHarness) {
	var mu sync.Mutex
	var seen []http.Header
	var seenBodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		seenBodies = append(seenBodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON)
	}))
	t.Cleanup(srv.Close)
	last := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return nil
		}
		return seen[len(seen)-1]
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(seen)
	}
	lastBody := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		if len(seenBodies) == 0 {
			return nil
		}
		return seenBodies[len(seenBodies)-1]
	}
	key := keepalive.Key{Tenant: "t1", Session: "s1"}
	auth := http.Header{}
	auth.Set("Authorization", "Bearer caller-key")
	reqWithBody := func(body []byte) keepalive.DispatchRequest {
		return keepalive.DispatchRequest{Key: key, Provider: bschemas.Anthropic, Model: "m",
			Route: "/v1/messages", Body: body,
			Headers: keepalive.PingHeaders(http.Header{"Anthropic-Version": {"2023-06-01"}})}
	}
	req := func() keepalive.DispatchRequest {
		return reqWithBody([]byte(`{"model":"m","max_tokens":1,"stream":false}`))
	}

	t.Run("dispatch attaches the held credential and returns usage", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		u, status, err := h.D.Dispatch(context.Background(), req())
		if err != nil || status != 200 {
			t.Fatalf("Dispatch = %d, %v", status, err)
		}
		if u.CacheRead != 4000 || u.CacheWrite != 0 {
			t.Fatalf("usage = %+v, want cache_read 4000", u)
		}
		if got := last().Get("Authorization"); got != "Bearer caller-key" {
			t.Fatalf("upstream saw Authorization %q", got)
		}
		if last().Get("Anthropic-Version") != "2023-06-01" {
			t.Fatal("non-secret header not forwarded")
		}
		h.D.Release(key)
	})

	// The Dispatcher doc: with nothing held for the key, Dispatch must return an error rather
	// than send unauthenticated. So it must fail, AND nothing may reach the upstream.
	t.Run("dispatch after release carries no credential", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		h.D.Release(key)
		before := count()
		_, status, err := h.D.Dispatch(context.Background(), req())
		if err == nil {
			t.Fatalf("Dispatch after Release = %d, nil error; want an error", status)
		}
		if n := count() - before; n != 0 {
			t.Fatalf("Dispatch after Release sent %d request(s) upstream (last Authorization %q); want none",
				n, last().Get("Authorization"))
		}
	})

	// Release is called while a ping is already on the wire (the keeper does not wait for it).
	// The in-flight Dispatch must finish on its own copy of the credential.
	t.Run("release during an in-flight dispatch", func(t *testing.T) {
		arrived := make(chan http.Header, 1)
		unblock := make(chan struct{})
		blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			arrived <- r.Header.Clone()
			<-unblock
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, usageJSON)
		}))
		t.Cleanup(blockSrv.Close)
		h := newH(t, blockSrv.URL)
		h.Hold(key, auth)
		type result struct {
			status int
			err    error
		}
		done := make(chan result, 1)
		go func() {
			_, status, err := h.D.Dispatch(context.Background(), req())
			done <- result{status, err}
		}()
		got := <-arrived
		h.D.Release(key)
		close(unblock)
		r := <-done
		if r.err != nil || r.status != 200 {
			t.Fatalf("in-flight Dispatch after Release = %d, %v; want 200, nil", r.status, r.err)
		}
		if a := got.Get("Authorization"); a != "Bearer caller-key" {
			t.Fatalf("upstream saw Authorization %q, want the held credential intact", a)
		}
	})

	t.Run("release of an unknown key is a no-op", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.D.Release(keepalive.Key{Tenant: "nobody", Session: "x"})
	})

	t.Run("hold replaces, and keys do not cross", func(t *testing.T) {
		h := newH(t, srv.URL)
		other := keepalive.Key{Tenant: "t1", Session: "s1", Thread: "a:sub"}
		a2 := http.Header{}
		a2.Set("Authorization", "Bearer second")
		h.Hold(key, auth)
		h.Hold(other, a2)
		h.D.Release(other)
		if _, _, err := h.D.Dispatch(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
		if got := last().Get("Authorization"); got != "Bearer caller-key" {
			t.Fatalf("releasing another thread affected this one: %q", got)
		}
		h.D.Release(key)
	})

	t.Run("dispatch does not retain the body", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		b1 := []byte(`{"model":"m","max_tokens":1,"stream":false,"n":1}`)
		r := reqWithBody(b1)
		if _, _, err := h.D.Dispatch(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if got := lastBody(); string(got) != string(b1) {
			t.Fatalf("upstream saw body %q, want %q", got, b1)
		}
		for i := range r.Body {
			r.Body[i] = 'x' // the keeper zeroizes it; a retained alias would now be corrupt
		}
		b2 := []byte(`{"model":"m","max_tokens":1,"stream":false,"n":2}`)
		if _, _, err := h.D.Dispatch(context.Background(), reqWithBody(b2)); err != nil {
			t.Fatal(err)
		}
		if got := lastBody(); string(got) != string(b2) {
			t.Fatalf("Dispatch retained the previous body: upstream saw %q, want %q", got, b2)
		}
		h.D.Release(key)
	})
}
