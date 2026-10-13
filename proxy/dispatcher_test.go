package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/dispatch/dispatchtest"
	"github.com/rossoctl/context-guru/keepalive"
)

func TestProxyDispatcherMeetsTheContract(t *testing.T) {
	dispatchtest.Contract(t, func(t *testing.T, upstreamURL string) dispatchtest.Harness {
		d := NewDispatcher(nil)
		return dispatchtest.Harness{D: d, Hold: func(h http.Header) dispatch.Ref {
			return d.hold(keepalive.Key{}, upstream{base: upstreamURL, path: "/v1/messages"}, h)
		}}
	})
}

// Every path that drops a keep-alive entry releases its credential; Shutdown drops them all. After
// it, the proxy must hold no caller credential at all.
func TestShutdownReleasesEveryHeldRef(t *testing.T) {
	d := NewDispatcher(nil)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	hdr := http.Header{"Authorization": {"Bearer sk-caller"}}
	up := upstream{base: "http://up", path: "/v1/messages"}
	for i, s := range []string{"s1", "s1", "s2", "s2"} { // a session's FIRST request is not tracked
		key := keepalive.Key{Tenant: "t1", Session: s}
		k.Record(keepalive.Record{Key: key, StartedAt: now.Add(time.Duration(i) * time.Second),
			Body: []byte(kaBody), Provider: bschemas.Anthropic, Endpoint: up.base, Route: up.path,
			Status: http.StatusOK, Usage: keepalive.Usage{CacheRead: 48576}, UsageOK: true,
			Policy: kaPolicy(), Header: hdr,
			Hold: func() dispatch.Ref { return d.hold(key, up, hdr) }})
	}
	if n := d.heldCount(); n != 2 {
		t.Fatalf("held %d credentials for two tracked sessions, want 2", n)
	}
	k.Shutdown()
	if n := d.heldCount(); n != 0 {
		t.Fatalf("after Shutdown the proxy still holds %d credentials", n)
	}
}

// The keeper parses a ping's usage itself (keepalive.PingUsage). It must read a ping response
// exactly as the proxy's own parser reads it, or a ping's cost and its write-vs-read guard would
// change with the host.
func TestPingUsageMatchesResponseUsage(t *testing.T) {
	for name, body := range map[string]string{
		"anthropic read": `{"stop_reason":"max_tokens","usage":{"input_tokens":3,"cache_read_input_tokens":48576,` +
			`"cache_creation_input_tokens":0,"output_tokens":1}}`,
		"anthropic write 1h": `{"stop_reason":"max_tokens","usage":{"input_tokens":3,"cache_read_input_tokens":0,` +
			`"cache_creation_input_tokens":900,"cache_creation":{"ephemeral_1h_input_tokens":900},"output_tokens":1}}`,
		"responses": `{"usage":{"input_tokens":48003,"input_tokens_details":{"cached_tokens":48000},` +
			`"output_tokens":16}}`,
	} {
		t.Run(name, func(t *testing.T) {
			want, ok := responseUsage("application/json", []byte(body))
			if !ok {
				t.Fatal("responseUsage did not parse the fixture")
			}
			got := keepalive.PingUsage([]byte(body))
			if got.FreshInput != want.FreshInput || got.CacheRead != want.CacheRead ||
				got.CacheWrite != want.CacheWrite || got.CacheWrite1h != want.CacheWrite1h ||
				got.Output != want.Output {
				t.Fatalf("PingUsage = %+v, responseUsage = %+v", got, want)
			}
		})
	}
}

// A detached call (a background summarizer) keeps using the incoming model after its triggering
// request has returned. incoming must not expire a ref merely because the request ended.
func TestIncomingRefOutlivesTheRequest(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	d := NewDispatcher(nil)
	ref := d.incoming(upstream{base: srv.URL}, http.Header{"Authorization": {"Bearer caller-key"}})
	// The "request" that produced ref is long gone; nothing in the proxy references it anymore
	// except the ref itself, as a detached summarizer would hold it.
	if _, err := d.Do(context.Background(), dispatch.Request{Purpose: dispatch.PurposeCheapModel,
		Ref: ref, Method: http.MethodPost, Path: "/v1/messages", Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("sent %d requests, want 1", calls)
	}
}

// Two calls with the same (base, credential) share one ref: a session's own cheap-model calls
// and a detached summary over the same incoming model do not each hold their own copy.
func TestIncomingRefDedupsOnBaseAndCredential(t *testing.T) {
	d := NewDispatcher(nil)
	auth := http.Header{"Authorization": {"Bearer k"}}
	a := d.incoming(upstream{base: "http://up"}, auth)
	b := d.incoming(upstream{base: "http://up"}, auth)
	if a != b {
		t.Fatalf("two incoming() calls for the same base+credential returned different refs: %q, %q", a, b)
	}
	c := d.incoming(upstream{base: "http://up"}, http.Header{"Authorization": {"Bearer other"}})
	if a == c {
		t.Fatal("a different credential must not share the first ref")
	}
}

// An incoming ref unused past IncomingRefTTL is dropped: a detached call that never arrives must
// not hold a caller's credential forever.
func TestIncomingRefExpiresAfterTTL(t *testing.T) {
	d := NewDispatcher(nil)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	ref := d.incoming(upstream{base: "http://up"}, http.Header{"Authorization": {"Bearer k"}})
	now = now.Add(IncomingRefTTL + time.Second)
	if _, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Method: http.MethodPost,
		Path: "/v1/messages", Body: []byte(`{}`)}); !errors.Is(err, dispatch.ErrNotHeld) {
		t.Fatalf("Do on an expired incoming ref: err = %v, want ErrNotHeld", err)
	}
}

// A wipeReader is not *bytes.Reader, so http.NewRequest cannot auto-detect its length — Do must
// set ContentLength itself, or the request goes out chunked. That matters beyond style: a minimal
// HTTP server (reading a body by Content-Length and never dechunking — Cortex's own
// scripts/dev/verify-contextguru-keepalive.sh fake upstream among them) sees an empty body.
func TestDoSetsContentLengthRatherThanChunking(t *testing.T) {
	var gotCL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCL = r.Header.Get("Content-Length")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	d := NewDispatcher(nil)
	ref := d.hold(keepalive.Key{}, upstream{base: srv.URL, path: "/v1/messages"}, http.Header{})
	body := []byte(`{"model":"m","max_tokens":1,"stream":false}`)
	if _, err := d.Do(context.Background(), dispatch.Request{Ref: ref, Method: http.MethodPost,
		Path: "/v1/messages", Body: body}); err != nil {
		t.Fatal(err)
	}
	if gotCL == "" {
		t.Fatal("ping went out with no Content-Length header (chunked instead)")
	}
}
