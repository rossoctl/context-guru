package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentGateway reproduces what the packet capture on contextguru2 showed the LiteLLM gateway
// doing: it reads the WHOLE request (every byte is ACKed) and then sends nothing at all — no
// headers, no reset, no FIN — on the connections it picks, while answering on others. silent
// decides, per accepted request in arrival order, whether to go quiet.
type silentGateway struct {
	ln       net.Listener
	silent   func(n int) bool
	requests atomic.Int64
	conns    atomic.Int64
	mu       sync.Mutex
	open     []net.Conn
}

func newSilentGateway(t *testing.T, silent func(n int) bool) *silentGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &silentGateway{ln: ln, silent: silent}
	go g.serve()
	t.Cleanup(g.close)
	return g
}

func (g *silentGateway) url() string { return "http://" + g.ln.Addr().String() }

func (g *silentGateway) close() {
	g.ln.Close()
	g.mu.Lock()
	for _, c := range g.open {
		c.Close()
	}
	g.mu.Unlock()
}

func (g *silentGateway) serve() {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		g.conns.Add(1)
		g.mu.Lock()
		g.open = append(g.open, c)
		g.mu.Unlock()
		go g.handle(c)
	}
}

func (g *silentGateway) handle(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		n := int(g.requests.Add(1))
		if g.silent(n) {
			// Hold the connection open and say nothing, exactly like the gateway. Reading on
			// keeps the peer's writes ACKed; return only when the client closes it.
			io.Copy(io.Discard, br)
			return
		}
		body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
}

const streamBody = `{"model":"m","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

func silenceBuild(url string, body string) func(context.Context) (*http.Request, error) {
	return func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	}
}

// The failure itself: an accepted, never-answered streaming request is resent once on a NEW
// connection and the client gets the answer — in about one silence timeout, not ten minutes.
func TestASilentStreamingRequestIsResentOnANewConnection(t *testing.T) {
	g := newSilentGateway(t, func(n int) bool { return n == 1 })
	c := &http.Client{Transport: upstreamTransport(10 * time.Minute)}
	before := upstreamSilenceResends.Load()
	start := time.Now()
	resp, err := doWithSilenceGuard(context.Background(), c, 150*time.Millisecond, silenceBuild(g.url(), streamBody))
	if err != nil {
		t.Fatalf("the resend should have been answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "message_stop") {
		t.Fatalf("incomplete stream after the resend: %q", b)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %s; the silent attempt should have been abandoned after ~150ms", el)
	}
	if got := g.requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want exactly 2 (one silent, one resend)", got)
	}
	if got := g.conns.Load(); got != 2 {
		t.Errorf("upstream saw %d connections, want 2: the resend must not reuse the silent one", got)
	}
	if upstreamSilenceResends.Load() != before+1 {
		t.Errorf("the resend was not counted")
	}
}

// A gateway that is genuinely not answering still fails — after two silences, with an error that
// says so, and never more than one resend.
func TestAGatewayThatNeverAnswersFailsAfterOneResend(t *testing.T) {
	g := newSilentGateway(t, func(int) bool { return true })
	c := &http.Client{Transport: upstreamTransport(10 * time.Minute)}
	before := upstreamSilenceFailures.Load()
	_, err := doWithSilenceGuard(context.Background(), c, 100*time.Millisecond, silenceBuild(g.url(), streamBody))
	if !errors.Is(err, errUpstreamSilent) {
		t.Fatalf("err = %v, want the silence error", err)
	}
	if got := g.requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2: at most ONE resend", got)
	}
	if upstreamSilenceFailures.Load() != before+1 {
		t.Errorf("the failure was not counted")
	}
}

// The guard only counts silence AFTER the first response byte has failed to come. A stream whose
// headers come promptly and whose tokens then take far longer than the timeout is never touched:
// the deadline is on the first byte, not on the generation.
func TestASlowStreamAfterTheFirstByteIsNeverResent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for i := 0; i < 4; i++ {
			time.Sleep(80 * time.Millisecond)
			fmt.Fprintf(w, "event: ping\ndata: {\"n\":%d}\n\n", i)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	var hits atomic.Int64
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return upstreamTransport(10 * time.Minute).RoundTrip(r)
	})}
	resp, err := doWithSilenceGuard(context.Background(), c, 50*time.Millisecond, silenceBuild(srv.URL, streamBody))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("a streaming body outliving the silence timeout was cut off: %v", err)
	}
	if n := strings.Count(string(b), "event: ping"); n != 4 {
		t.Errorf("got %d chunks, want 4", n)
	}
	if hits.Load() != 1 {
		t.Errorf("%d attempts for a request that answered at once", hits.Load())
	}
}

// The client going away cancels the request and is NOT turned into a resend: a cancelled turn
// must not cost a second generation.
func TestAClientThatGivesUpIsNotResent(t *testing.T) {
	g := newSilentGateway(t, func(int) bool { return true })
	c := &http.Client{Transport: upstreamTransport(10 * time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := doWithSilenceGuard(ctx, c, time.Second, silenceBuild(g.url(), streamBody))
	if err == nil || errors.Is(err, errUpstreamSilent) {
		t.Fatalf("err = %v, want the client's own cancellation", err)
	}
	if got := g.requests.Load(); got != 1 {
		t.Errorf("upstream saw %d requests after the client gave up, want 1", got)
	}
}

// End to end through the handler: a streaming /v1/messages turn whose first upstream attempt is
// silent reaches the client complete, from the resend.
func TestTheHandlerRecoversASilentStreamingTurn(t *testing.T) {
	g := newSilentGateway(t, func(n int) bool { return n == 1 })
	h := New(nil, nil, nil, Options{AnthropicUpstream: g.url(), StreamSilenceTimeout: 150 * time.Millisecond})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(streamBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "message_stop") {
		t.Fatalf("status %d body %q: the silent first attempt reached the client", resp.StatusCode, b)
	}
	if g.requests.Load() != 2 {
		t.Errorf("upstream saw %d requests, want 2", g.requests.Load())
	}
}

// A NON-streaming request sends its headers only after the whole answer is generated, so
// silence there is normal and must never trigger a resend — it would duplicate a long
// generation.
func TestANonStreamingRequestIsNeverResentForSilence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // "generating", well past the silence timeout below
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	var hits atomic.Int64
	counting := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})}
	h := New(nil, nil, nil, Options{AnthropicUpstream: srv.URL, Client: counting, StreamSilenceTimeout: 50 * time.Millisecond})
	front := httptest.NewServer(h.Mux())
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Errorf("a non-streaming request was sent %d times; silence before its headers is generation, not a hang", hits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The pool keeps a warm connection for every stream of a burst. With DefaultTransport's
// MaxIdleConnsPerHost of 2, a burst of 8 left only 2 connections and the next burst paid six new
// TCP+TLS handshakes; production showed 172 of 388 upstream connections closed that way.
func TestABurstOfStreamsKeepsItsConnectionsWarm(t *testing.T) {
	var conns, inflight atomic.Int64
	var mu sync.Mutex
	release := make(chan struct{})
	gate := func() chan struct{} { mu.Lock(); defer mu.Unlock(); return release }
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inflight.Add(1)
		<-gate() // hold every request open so the burst really is concurrent
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c := &http.Client{Transport: upstreamTransport(time.Minute)}
	burst := func() {
		inflight.Store(0)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := c.Get(srv.URL)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}()
		}
		for inflight.Load() < 8 && t.Context().Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		close(release)
		mu.Unlock()
		wg.Wait()
	}
	burst()
	if got := conns.Load(); got != 8 {
		t.Fatalf("first burst opened %d connections, want 8", got)
	}
	mu.Lock()
	release = make(chan struct{})
	mu.Unlock()
	burst()
	if got := conns.Load(); got != 8 {
		t.Errorf("second burst opened %d new connections; the pool closed warm ones after the first burst", got-8)
	}
}

// dropAfterRequestGateway answers the first request on each connection, then on the SECOND request
// of a connection reads it and closes the connection without a byte of response — what the
// packet capture showed the LiteLLM gateway doing (request ACKed, then its FIN, never a response).
type dropAfterRequestGateway struct {
	ln       net.Listener
	drop     func(n int) bool // per request on a connection, 1-based
	requests atomic.Int64
	conns    atomic.Int64
}

func newDropGateway(t *testing.T, drop func(n int) bool) *dropAfterRequestGateway {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &dropAfterRequestGateway{ln: ln, drop: drop}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.conns.Add(1)
			go g.handle(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return g
}

func (g *dropAfterRequestGateway) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for n := 1; ; n++ {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		g.requests.Add(1)
		if g.drop(n) {
			return // close with no response: the client's read sees EOF
		}
		body := `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
}

const nonStreamBody = `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`

// The 15:21:33 502: a reused connection the upstream closes after reading the request. It is sent
// again on a new connection and answered, instead of becoming "Post ...: EOF" / 502. This holds for
// non-streaming requests too: nothing came back, so nothing was generated.
func TestAReusedConnectionClosedUnansweredIsResent(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		silence    time.Duration
	}{{"streaming", streamBody, time.Minute}, {"non-streaming", nonStreamBody, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDropGateway(t, func(n int) bool { return n == 2 })
			c := &http.Client{Transport: upstreamTransport(time.Minute)}
			// Warm the pool: one answered request leaves a kept-alive connection behind.
			resp, err := doWithSilenceGuard(context.Background(), c, tc.silence, silenceBuild(g.url(), tc.body))
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			before := upstreamDroppedReuseResends.Load()
			resp, err = doWithSilenceGuard(context.Background(), c, tc.silence, silenceBuild(g.url(), tc.body))
			if err != nil {
				t.Fatalf("a request dropped on a reused connection reached the caller as %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := g.requests.Load(); got != 3 {
				t.Errorf("upstream saw %d requests, want 3 (warm-up, dropped, resend)", got)
			}
			if got := g.conns.Load(); got != 2 {
				t.Errorf("upstream saw %d connections, want 2: the resend must use a new one", got)
			}
			if upstreamDroppedReuseResends.Load() != before+1 {
				t.Errorf("the resend was not counted")
			}
		})
	}
}

func (g *dropAfterRequestGateway) url() string { return "http://" + g.ln.Addr().String() }

// A FRESH connection closed on us is a real failure and is returned as one — never resent, so a
// gateway that drops every request fails fast instead of looping.
func TestAFreshConnectionClosedUnansweredIsNotResent(t *testing.T) {
	g := newDropGateway(t, func(int) bool { return true })
	c := &http.Client{Transport: upstreamTransport(time.Minute)}
	_, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(g.url(), nonStreamBody))
	if err == nil {
		t.Fatal("a request the upstream never answered was reported as a success")
	}
	if errors.Is(err, errUnansweredReuse) {
		t.Fatalf("a fresh connection was treated as a stale reused one: %v", err)
	}
	if got := g.requests.Load(); got != 1 {
		t.Errorf("upstream saw %d requests, want 1", got)
	}
}

// Review finding: Request.Close does not force a new connection, so a resend could take ANOTHER
// idle pooled connection that the gateway had also dropped. With several warm connections all
// dropped together (as when the gateway drops its idle connections at once), the request must
// still be answered on a freshly dialled one.
func TestAResendNeverReusesAnotherDroppedPooledConnection(t *testing.T) {
	var dropping atomic.Bool
	g := newDropGateway(t, func(n int) bool { return n >= 2 && dropping.Load() })
	c := &http.Client{Transport: upstreamTransport(time.Minute)}
	// Warm four pooled connections with four concurrent requests.
	var wg sync.WaitGroup
	hold := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(g.url(), nonStreamBody))
			if err != nil {
				t.Error(err)
				return
			}
			<-hold
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	for g.requests.Load() < 4 && t.Context().Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	close(hold)
	wg.Wait()
	if got := g.conns.Load(); got != 4 {
		t.Fatalf("warm-up opened %d connections, want 4", got)
	}
	// Now the gateway drops every request on every connection that has already served one.
	dropping.Store(true)
	resp, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(g.url(), nonStreamBody))
	if err != nil {
		t.Fatalf("the resend landed on another dropped pooled connection: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := g.conns.Load(); got != 5 {
		t.Errorf("upstream saw %d connections, want 5: one dropped reuse, then ONE fresh dial", got)
	}
}

// An EOF after part of a response is io.ErrUnexpectedEOF, not a dropped request: the upstream
// processed it, so it must be returned, never resent.
func TestAResponseCutOffMidwayIsNotResent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var requests atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for n := 1; ; n++ {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					requests.Add(1)
					if n == 1 {
						body := `{"usage":{}}`
						fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
						continue
					}
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Le") // cut off inside the headers
					return
				}
			}(c)
		}
	}()
	url := "http://" + ln.Addr().String()
	c := &http.Client{Transport: upstreamTransport(time.Minute)}
	resp, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(url, nonStreamBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if _, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(url, nonStreamBody)); err == nil {
		t.Fatal("a truncated response was reported as a success")
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2: a response cut off midway must not be resent", got)
	}
}

// A dropped reused connection on a request whose client has already gone away is not resent.
func TestADroppedReuseIsNotResentAfterTheClientLeft(t *testing.T) {
	g := newDropGateway(t, func(n int) bool { return n == 2 })
	c := &http.Client{Transport: upstreamTransport(time.Minute)}
	resp, err := doWithSilenceGuard(context.Background(), c, 0, silenceBuild(g.url(), nonStreamBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := doWithSilenceGuard(ctx, c, 0, silenceBuild(g.url(), nonStreamBody)); err == nil {
		t.Fatal("a cancelled request succeeded")
	}
	if got := g.requests.Load(); got > 2 {
		t.Errorf("upstream saw %d requests: a request whose client left was resent", got)
	}
}

// count_tokens is a short POST on the same pooled client, and gets the same recovery.
func TestCountTokensRecoversADroppedReusedConnection(t *testing.T) {
	g := newDropGateway(t, func(n int) bool { return n == 2 })
	h := New(nil, nil, nil, Options{AnthropicUpstream: g.url()})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages/count_tokens",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := post(); s != 200 {
		t.Fatalf("warm-up status %d", s)
	}
	if s := post(); s != 200 {
		t.Fatalf("count_tokens on a dropped reused connection returned %d, want 200 from the resend", s)
	}
}
