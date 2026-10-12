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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, r.Header.Clone())
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
	key := keepalive.Key{Tenant: "t1", Session: "s1"}
	auth := http.Header{}
	auth.Set("Authorization", "Bearer caller-key")
	req := func() keepalive.DispatchRequest {
		return keepalive.DispatchRequest{Key: key, Provider: bschemas.Anthropic, Model: "m",
			Route: "/v1/messages", Body: []byte(`{"model":"m","max_tokens":1,"stream":false}`),
			Headers: keepalive.PingHeaders(http.Header{"Anthropic-Version": {"2023-06-01"}})}
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

	t.Run("dispatch after release carries no credential", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.Hold(key, auth)
		h.D.Release(key)
		_, status, err := h.D.Dispatch(context.Background(), req())
		if err == nil && last().Get("Authorization") == "Bearer caller-key" && status == 200 {
			t.Fatal("a released credential was used")
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
		r := req()
		if _, _, err := h.D.Dispatch(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for i := range r.Body {
			r.Body[i] = 'x' // the keeper zeroizes it; a retained alias would now be corrupt
		}
		if _, _, err := h.D.Dispatch(context.Background(), req()); err != nil {
			t.Fatal(err)
		}
		h.D.Release(key)
	})
}
