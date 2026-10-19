package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

// A streaming upstream request that has been written but not answered with a single byte is
// abandoned and sent once more, on a NEW connection, after streamSilenceTimeout.
//
// WHY THIS EXISTS. The IBM LiteLLM gateway (ete-litellm.ai-models.vpc-int) sometimes accepts a
// request and never answers it. Measured on contextguru2 from a packet capture of real traffic
// (2026-10-18, 911 upstream requests, 63 of them hung):
//
//   - The TCP connection is healthy throughout: the gateway ACKs every byte of the request and
//     answers TCP keep-alives every 5 s for the full ten minutes. It simply never sends a byte of
//     response — no headers, no reset, no FIN. Nothing at the transport level ever fails.
//   - It is not connection reuse. 34 of the 63 were the FIRST request on a brand-new connection.
//     Shortening the idle-connection pool would have added a TCP+TLS handshake to 43% of requests
//     and still left most hangs in place.
//   - It is not the request. A hung request resent unchanged on another connection is answered
//     normally. It also hangs with no context-guru in the path at all (a plain Go client against
//     the gateway hung 1 of 24 times under 4-way concurrency).
//   - Context-guru made it hurt. ResponseHeaderTimeout is 10 minutes, so each hang held the turn
//     until the client gave up: Claude Code's own timeout (logged here as "context canceled" at
//     ~300 s), the keep-alive ping's 60 s, or our 600 s 502/504. 825 of ~25,000 requests from
//     10-14 to 10-18 ended that way.
//
// WHY A STREAMING SILENCE IS A SAFE SIGNAL. A streaming upstream sends response headers as soon
// as it has accepted the request, before generation starts; the gateway's own p99.9 for the first
// byte of an answered streaming request was 58 s, and 99.06% arrived within 20 s. A
// NON-streaming request is different: its headers arrive only when the whole answer is
// generated, so silence there means "still working" and is NOT bounded here — it keeps the
// generous ResponseHeaderTimeout.
//
// WHY RESENDING IS SAFE. The second attempt happens only when NOT ONE BYTE of the response has
// arrived, so nothing has been forwarded to the client and there is no stream to replay. The
// first attempt is cancelled before the second is sent, which closes its connection, so the
// gateway sees the first request abandoned rather than two live ones. At most ONE resend, so a
// gateway that is genuinely down still fails, now in 2 x streamSilenceTimeout instead of ten
// minutes. The worst case of a resend is one duplicated generation on the provider (if the
// gateway was in fact working on the first copy): billed tokens, never a duplicated stream.
//
// THE TIMEOUT. 45 s: above every answered first byte on the measured traffic except one (58 s,
// 1 of 848, which would be resent rather than lost), and an order of magnitude below the
// minutes a hung request used to cost. A first byte under 45 s is never touched.
const streamSilenceTimeout = 45 * time.Second

// errUpstreamSilent is what a request that was abandoned for silence reports, so the log says
// what actually happened rather than "context canceled".
var errUpstreamSilent = errors.New("upstream accepted the request but sent no response byte")

// upstreamSilenceResends counts streaming requests resent because the first attempt was silent;
// upstreamSilenceFailures counts those whose resend was silent too. /metrics exports both so a
// gateway that starts hanging more is visible, rather than absorbed.
var upstreamSilenceResends, upstreamSilenceFailures atomic.Int64

// streamsRequest reports whether this upstream body asks for a streamed response.
func streamsRequest(body []byte) bool {
	return gjson.GetBytes(body, "stream").Bool()
}

// doWithSilenceGuard sends req (built by build, called once per attempt so each attempt has a
// fresh body reader) and, for a streaming request, abandons an attempt that has produced no
// response byte within timeout and sends it once more on a new connection.
//
// The returned response's body stays tied to the attempt that produced it; closing it releases
// that attempt.
func doWithSilenceGuard(ctx context.Context, client *http.Client, timeout time.Duration,
	build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	resp, err := silenceAttempt(ctx, client, timeout, build, false)
	if !errors.Is(err, errUpstreamSilent) {
		return resp, err
	}
	upstreamSilenceResends.Add(1)
	resp, err = silenceAttempt(ctx, client, timeout, build, true)
	if errors.Is(err, errUpstreamSilent) {
		upstreamSilenceFailures.Add(1)
		return nil, fmt.Errorf("%w for %s on two connections", errUpstreamSilent, timeout)
	}
	return resp, err
}

// silenceAttempt runs one attempt under a watchdog that fires if no response byte has arrived
// within timeout of the request being WRITTEN. Time spent dialling, handshaking or uploading a
// large body does not count against it: those failures have their own errors, and a slow upload
// is not a silent gateway.
func silenceAttempt(parent context.Context, client *http.Client, timeout time.Duration,
	build func(context.Context) (*http.Request, error), fresh bool) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(parent)
	var answered atomic.Bool
	var watchdog atomic.Pointer[time.Timer]
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			watchdog.Store(time.AfterFunc(timeout, func() {
				if !answered.Load() {
					cancel(errUpstreamSilent)
				}
			}))
		},
		GotFirstResponseByte: func() { answered.Store(true) },
	}
	req, err := build(httptrace.WithClientTrace(ctx, trace))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if fresh {
		// A NEW connection for the resend: the silent attempt's connection is being torn down by
		// the cancel, and the pool could otherwise hand over another connection to the same
		// gateway instance. Close forces the transport to dial.
		req.Close = true
	}
	resp, err := client.Do(req)
	stop := func() {
		if t := watchdog.Load(); t != nil {
			t.Stop()
		}
	}
	if err != nil {
		stop()
		if errors.Is(context.Cause(ctx), errUpstreamSilent) && parent.Err() == nil {
			cancel(nil)
			return nil, errUpstreamSilent
		}
		cancel(nil)
		return nil, err
	}
	// Headers arrived, so the watchdog's job is done. The attempt's context must stay alive for as
	// long as the body is read, so it is cancelled when the caller closes the body.
	answered.Store(true)
	stop()
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

// cancelOnClose releases an attempt's context once its response body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
