package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A streaming turn that keeps producing must not be cut off for taking a while.
//
// The default client carried http.Client{Timeout: 5 * time.Minute}, and that timeout covers
// reading the response BODY — so it was a ceiling on how long a generation could last, not a
// detector for a dead upstream. On one account eleven large streamed turns died at ~298s of
// upstream time and returned 502, which the agent reports as an API error after four or five
// minutes of apparently healthy work, while 160 shorter streamed turns to the same upstream
// succeeded.
//
// The wall is simulated rather than waited on: the client is built the way New builds it and
// pointed at a server whose stream outlives a deliberately tiny header timeout. Slow FIRST
// byte must still fail (a dead upstream is still caught); slow to FINISH must not.
func TestAStreamIsNotCutOffForLasting(t *testing.T) {
	t.Parallel()
	body := func(w http.ResponseWriter, firstByteAfter, betweenChunks time.Duration) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(firstByteAfter)
		for i := 0; i < 4; i++ {
			fmt.Fprintf(w, "event: ping\ndata: {\"n\":%d}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(betweenChunks)
		}
	}

	t.Run("long stream survives", func(t *testing.T) {
		t.Parallel()
		// Chunks keep arriving well past the header budget: under a whole-request timeout
		// this is the 502 the account saw.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body(w, 10*time.Millisecond, 40*time.Millisecond)
		}))
		defer srv.Close()
		c := &http.Client{Transport: upstreamTransport(50 * time.Millisecond)}
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("a healthy stream failed: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("the stream was cut off after %d bytes: %v", len(b), err)
		}
		if n := strings.Count(string(b), "event: ping"); n != 4 {
			t.Errorf("got %d chunks, want all 4:\n%s", n, b)
		}
	})

	t.Run("a silent upstream still fails", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body(w, 2*time.Second, time.Millisecond)
		}))
		defer srv.Close()
		c := &http.Client{Transport: upstreamTransport(50 * time.Millisecond)}
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
			t.Fatal("an upstream that sent nothing was treated as healthy")
		}
	})

	// And the handler's own default really is this transport, with no whole-request ceiling.
	t.Run("New builds it that way", func(t *testing.T) {
		t.Parallel()
		h := New(nil, nil, nil, Options{})
		if h.client.Timeout != 0 {
			t.Errorf("the default upstream client has a whole-request timeout of %s; that is a "+
				"ceiling on a streamed generation, not a liveness check", h.client.Timeout)
		}
		tr, ok := h.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T, want *http.Transport", h.client.Transport)
		}
		if tr.ResponseHeaderTimeout != defaultUpstreamHeaderTimeout {
			t.Errorf("ResponseHeaderTimeout = %s, want %s — with neither timeout set a dead "+
				"upstream would hang the agent forever", tr.ResponseHeaderTimeout,
				defaultUpstreamHeaderTimeout)
		}
	})
}
