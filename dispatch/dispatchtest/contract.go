// Package dispatchtest holds what every host of dispatch.Dispatcher tests against: the exported
// contract (Contract), a recording fake for core tests (Fake), and a minimal HTTP host for tests
// that talk to an httptest server (HTTPHost).
package dispatchtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
)

// Harness is one host's Dispatcher under test, plus the host-side hook that captures a credential
// for the upstream under test. Hold receives the request headers the host captures it from and
// returns the Ref it issued.
type Harness struct {
	D    dispatch.Dispatcher
	Hold func(reqHeaders http.Header) dispatch.Ref
}

// NewHarness builds a host whose held targets point at upstreamURL (scheme://host, no path).
type NewHarness func(t *testing.T, upstreamURL string) Harness

const usageJSON = `{"usage":{"input_tokens":1,"cache_read_input_tokens":4000,` +
	`"cache_creation_input_tokens":0,"output_tokens":1}}`

// recorder is an upstream that records what reached it.
type recorder struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
}

func (r *recorder) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.headers = append(r.headers, req.Header.Clone())
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, usageJSON)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.headers)
}

func (r *recorder) last() (http.Header, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.headers) == 0 {
		return nil, nil
	}
	return r.headers[len(r.headers)-1], r.bodies[len(r.bodies)-1]
}

func request(ref dispatch.Ref, body []byte) dispatch.Request {
	return dispatch.Request{Purpose: dispatch.PurposeKeepAlivePing, Ref: ref, Provider: bschemas.Anthropic,
		Method: http.MethodPost, Path: "/v1/messages", Body: body,
		Header: http.Header{"Anthropic-Version": {"2023-06-01"}, "Content-Type": {"application/json"}}}
}

func pingBody() []byte { return []byte(`{"model":"m","max_tokens":1,"stream":false}`) }

// Contract is what every host's Dispatcher must satisfy. A change in context-guru that breaks it
// fails here first, before it reaches a host.
func Contract(t *testing.T, newH NewHarness) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(srv.Close)
	auth := http.Header{"Authorization": {"Bearer caller-key"}}

	t.Run("do attaches the held credential and returns the response", func(t *testing.T) {
		h := newH(t, srv.URL)
		ref := h.Hold(auth)
		resp, err := h.D.Do(context.Background(), request(ref, pingBody()))
		if err != nil || resp.Status != http.StatusOK {
			t.Fatalf("Do = %d, %v", resp.Status, err)
		}
		if string(resp.Body) != usageJSON {
			t.Fatalf("Response.Body = %q, want the upstream's body", resp.Body)
		}
		hdr, body := rec.last()
		if got := hdr.Get("Authorization"); got != "Bearer caller-key" {
			t.Fatalf("upstream saw Authorization %q", got)
		}
		if hdr.Get("Anthropic-Version") != "2023-06-01" {
			t.Fatal("non-secret header not forwarded")
		}
		if string(body) != string(pingBody()) {
			t.Fatalf("upstream saw body %q", body)
		}
		h.D.Release(ref)
	})

	t.Run("do after release sends nothing and returns ErrNotHeld", func(t *testing.T) {
		h := newH(t, srv.URL)
		ref := h.Hold(auth)
		h.D.Release(ref)
		before := rec.count()
		_, err := h.D.Do(context.Background(), request(ref, pingBody()))
		if !errors.Is(err, dispatch.ErrNotHeld) {
			t.Fatalf("Do after Release: err = %v, want ErrNotHeld", err)
		}
		if n := rec.count() - before; n != 0 {
			t.Fatalf("Do after Release sent %d request(s) upstream; want none", n)
		}
	})

	t.Run("do with a ref never issued returns ErrNotHeld", func(t *testing.T) {
		h := newH(t, srv.URL)
		before := rec.count()
		if _, err := h.D.Do(context.Background(), request("never-issued", pingBody())); !errors.Is(err, dispatch.ErrNotHeld) {
			t.Fatalf("err = %v, want ErrNotHeld", err)
		}
		if rec.count() != before {
			t.Fatal("an unknown ref reached the upstream")
		}
	})

	// Release can arrive while a call is on the wire (the keeper does not wait for it). The
	// in-flight Do must finish on its own copy of the credential.
	t.Run("release during an in-flight do", func(t *testing.T) {
		arrived := make(chan http.Header, 1)
		unblock := make(chan struct{})
		blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			arrived <- r.Header.Clone()
			<-unblock
			_, _ = io.WriteString(w, usageJSON)
		}))
		t.Cleanup(blockSrv.Close)
		h := newH(t, blockSrv.URL)
		ref := h.Hold(auth)
		type result struct {
			status int
			err    error
		}
		done := make(chan result, 1)
		go func() {
			resp, err := h.D.Do(context.Background(), request(ref, pingBody()))
			done <- result{resp.Status, err}
		}()
		got := <-arrived
		h.D.Release(ref)
		close(unblock)
		r := <-done
		if r.err != nil || r.status != http.StatusOK {
			t.Fatalf("in-flight Do after Release = %d, %v; want 200, nil", r.status, r.err)
		}
		if a := got.Get("Authorization"); a != "Bearer caller-key" {
			t.Fatalf("upstream saw Authorization %q, want the held credential intact", a)
		}
	})

	t.Run("release of an unknown ref is a no-op", func(t *testing.T) {
		h := newH(t, srv.URL)
		h.D.Release("nobody")
	})

	t.Run("refs do not cross", func(t *testing.T) {
		h := newH(t, srv.URL)
		a := h.Hold(auth)
		b := h.Hold(http.Header{"Authorization": {"Bearer second"}})
		if a == b {
			t.Fatalf("two holds returned the same ref %q", a)
		}
		h.D.Release(b)
		if _, err := h.D.Do(context.Background(), request(a, pingBody())); err != nil {
			t.Fatal(err)
		}
		if hdr, _ := rec.last(); hdr.Get("Authorization") != "Bearer caller-key" {
			t.Fatalf("releasing another ref affected this one: %q", hdr.Get("Authorization"))
		}
		h.D.Release(a)
	})

	t.Run("do does not retain the body", func(t *testing.T) {
		h := newH(t, srv.URL)
		ref := h.Hold(auth)
		b1 := []byte(`{"model":"m","max_tokens":1,"stream":false,"n":1}`)
		r := request(ref, b1)
		if _, err := h.D.Do(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		for i := range r.Body {
			r.Body[i] = 'x' // the core zeroes it; a retained alias would now be corrupt
		}
		b2 := []byte(`{"model":"m","max_tokens":1,"stream":false,"n":2}`)
		if _, err := h.D.Do(context.Background(), request(ref, b2)); err != nil {
			t.Fatal(err)
		}
		if _, got := rec.last(); string(got) != string(b2) {
			t.Fatalf("Do retained the previous body: upstream saw %q, want %q", got, b2)
		}
		h.D.Release(ref)
	})

	t.Run("a redirect is not followed", func(t *testing.T) {
		other := &recorder{}
		otherSrv := httptest.NewServer(http.HandlerFunc(other.serve))
		t.Cleanup(otherSrv.Close)
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			http.Redirect(w, r, otherSrv.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redir.Close)
		h := newH(t, redir.URL)
		ref := h.Hold(http.Header{"X-Api-Key": {"caller-key"}})
		resp, err := h.D.Do(context.Background(), request(ref, pingBody()))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want the 307 itself", resp.Status)
		}
		if other.count() != 0 {
			t.Fatal("the redirect target received the request (and the credential)")
		}
		h.D.Release(ref)
	})

	t.Run("the response body is bounded", func(t *testing.T) {
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, strings.Repeat("a", 2*dispatch.MaxResponseBytes))
		}))
		t.Cleanup(big.Close)
		h := newH(t, big.URL)
		ref := h.Hold(auth)
		resp, err := h.D.Do(context.Background(), request(ref, pingBody()))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Body) > dispatch.MaxResponseBytes {
			t.Fatalf("read %d bytes, want at most %d", len(resp.Body), dispatch.MaxResponseBytes)
		}
		h.D.Release(ref)
	})

	t.Run("an unknown model is unpriced, never free", func(t *testing.T) {
		h := newH(t, srv.URL)
		if p, ok := h.D.Price(context.Background(), srv.URL, "dispatchtest-unknown-model", 1000); ok {
			t.Fatalf("Price of an unknown model = %+v, ok=true; want ok=false", p)
		}
	})
}
