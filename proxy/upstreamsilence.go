package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

// An upstream request that provably reached nobody is sent once more on a NEW connection, instead
// of turning into a ten-minute wait or a 502. Two shapes qualify, and only these two:
//
//   - SILENCE (streaming requests only): the request was written and no response byte arrived
//     within streamSilenceTimeout.
//   - A DROPPED REUSED CONNECTION (every request): the request was written on a kept-alive
//     connection, and the upstream closed it (EOF) before sending one byte of response.
//
// WHY THIS EXISTS. Both shapes were measured on contextguru2 against the IBM LiteLLM gateway
// (ete-litellm.ai-models.vpc-int), from a packet capture of real traffic on 2026-10-18 (911
// upstream requests):
//
//   - 63 hung. The gateway ACKed every byte of the request and answered TCP keep-alives every 5 s
//     for the full ten minutes, and never sent a byte of response — no headers, no reset, no FIN.
//     34 of the 63 were the FIRST request on a brand-new connection, so this is not connection
//     reuse: a shorter idle timeout would add TCP+TLS handshakes and leave most hangs in place. The
//     same request on another connection is answered, and a plain Go client with no context-guru
//     in the path hangs the same way.
//   - Separately, the gateway closes a reused connection about a second after our request is
//     written, with no response: Go reports "EOF". Go's transport retries exactly this race by
//     itself, but only for idempotent requests, and a POST is not one — so each became a 502 (55
//     on contextguru2 since September, including a user's benchmark failure at 15:21:33).
//   - Context-guru made the hangs hurt: ResponseHeaderTimeout is ten minutes, so each one held the
//     turn until something else gave up — Claude Code at ~300 s ("context canceled" in our log),
//     the keep-alive ping at 60 s, or our own 502/504 at 600 s. 825 of ~25,000 requests from 10-14
//     to 10-18 ended that way.
//
// WHY STREAMING SILENCE IS A SAFE SIGNAL. A streaming upstream sends its response headers when it
// accepts the request, before it generates. On the measured traffic 99.06% of answered streaming
// requests had their first byte within 20 s, and the slowest within 58 s. A NON-streaming request
// sends headers only when the whole answer is generated, so silence there means "still working":
// it gets no silence deadline and keeps the generous ResponseHeaderTimeout.
//
// WHAT A RESEND CAN AND CANNOT DO.
//
//   - It never replays a stream: it happens only when not one response byte has arrived, so
//     nothing has been forwarded to the client.
//   - It is at most one per request (plus, for a dropped reused connection only, as Go's own retry
//     does, further attempts while the pool keeps handing over connections that are already dead —
//     bounded by maxDroppedReuseRetries). A gateway that is down still fails, now in about two
//     silence timeouts instead of ten minutes.
//   - It always uses a NEW connection: idle pooled connections to the upstream are closed first
//     (see freshAttempt), because a gateway that just dropped or ignored one connection may have
//     done the same to its siblings.
//   - The client going away is never turned into a resend.
//   - Its worst case is ONE duplicated generation on the provider, if the gateway was in fact
//     working on the first copy and failed to say so: billed tokens, never a duplicated stream. A
//     dropped reused connection is the least likely to hide that — the close comes about a second
//     after the request, the documented keep-alive race — but the response never says so, so it
//     is not claimed as impossible.
//
// THE TIMEOUT. 45 s: above every answered first byte on the measured traffic except one (58 s, 1
// of 848, which would be resent rather than lost), and an order of magnitude below the minutes a
// hung request used to cost.
const streamSilenceTimeout = 45 * time.Second

// maxDroppedReuseRetries bounds the attempts for a request whose reused connection keeps being
// closed unanswered. Each resend uses a freshly dialled connection, so this is reached only if
// freshly dialled connections are themselves being dropped, which is a real failure.
const maxDroppedReuseRetries = 2

// errUpstreamSilent is what a request abandoned for silence reports, so the log says what
// happened rather than "context canceled".
var errUpstreamSilent = errors.New("upstream accepted the request but sent no response byte")

// errUnansweredReuse marks an attempt whose reused connection was closed without a byte of
// response. It never leaves this file: the attempt is resent, or, out of retries, the transport's
// own error is returned.
var errUnansweredReuse = errors.New("upstream closed a reused connection without answering")

// droppedReuse carries the transport's error for an errUnansweredReuse attempt.
type droppedReuse struct{ err error }

func (d *droppedReuse) Error() string        { return errUnansweredReuse.Error() + ": " + d.err.Error() }
func (d *droppedReuse) Is(target error) bool { return target == errUnansweredReuse }
func (d *droppedReuse) Unwrap() error        { return d.err }

// Counters for /metrics. A resend that succeeds is invisible to the client, so without these a
// gateway hanging one request in ten would look exactly like a healthy one.
var (
	upstreamSilenceResends      atomic.Int64 // silent streaming attempts resent
	upstreamSilenceFailures     atomic.Int64 // ... whose resend was silent too (became a 502)
	upstreamDroppedReuseResends atomic.Int64 // attempts resent after a reused connection was dropped
)

// streamsRequest reports whether this upstream body asks for a streamed response. Every dialect
// this proxy forwards on the guarded path (Anthropic Messages, OpenAI chat/completions) streams by
// the body's "stream" field.
func streamsRequest(body []byte) bool {
	return gjson.GetBytes(body, "stream").Bool()
}

// idleCloser is the part of *http.Transport that freshAttempt needs. A custom Options.Client
// whose transport cannot close idle connections still gets resends, just without the guarantee
// of a new connection.
type idleCloser interface{ CloseIdleConnections() }

// doWithSilenceGuard sends the request built by build (called once per attempt, so each attempt
// gets a fresh body reader), resending it on a new connection in exactly the two cases described
// at the top of this file. silence <= 0 disables the silence deadline (non-streaming requests).
//
// The returned response's body stays tied to the attempt that produced it: closing it releases
// that attempt.
func doWithSilenceGuard(ctx context.Context, client *http.Client, silence time.Duration,
	build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	resp, err := silenceAttempt(ctx, client, silence, build)
	silentResent := false
	for drops := 0; ; {
		switch {
		case errors.Is(err, errUpstreamSilent) && !silentResent:
			silentResent = true
			upstreamSilenceResends.Add(1)
		case errors.Is(err, errUpstreamSilent):
			upstreamSilenceFailures.Add(1)
			return nil, fmt.Errorf("%w within %s, on two connections", errUpstreamSilent, silence)
		case errors.Is(err, errUnansweredReuse) && drops < maxDroppedReuseRetries:
			drops++
			upstreamDroppedReuseResends.Add(1)
		case errors.Is(err, errUnansweredReuse):
			// Out of retries: hand back the transport's own error, not our marker.
			var d *droppedReuse
			errors.As(err, &d)
			return nil, d.err
		default:
			return resp, err
		}
		freshAttempt(client)
		resp, err = silenceAttempt(ctx, client, silence, build)
	}
}

// freshAttempt makes the next attempt dial a new connection. Request.Close would NOT do that — it
// only closes the connection after the exchange, and the transport would still hand the request an
// idle pooled one — so the idle pool is closed instead. Connections that are carrying a response
// are not idle and are not affected; the cost is at most one handshake per pooled connection, and
// only on the rare request that is resent.
func freshAttempt(client *http.Client) {
	if t, ok := client.Transport.(idleCloser); ok {
		t.CloseIdleConnections()
	} else if client.Transport == nil {
		if t, ok := http.DefaultTransport.(idleCloser); ok {
			t.CloseIdleConnections()
		}
	}
}

// silenceAttempt runs one attempt. A watchdog, started once the request is fully WRITTEN, cancels
// it if no response byte arrives within silence: time spent dialling, handshaking or uploading a
// large body does not count, because those fail with their own errors and a slow upload is not a
// silent gateway.
func silenceAttempt(parent context.Context, client *http.Client, silence time.Duration,
	build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(parent)
	var (
		answered, reused, wrote atomic.Bool
		mu                      sync.Mutex
		watchdog                *time.Timer
		done                    bool // Do has returned: a late WroteRequest must not start a timer
	)
	stopWatchdog := func() {
		mu.Lock()
		done = true
		if watchdog != nil {
			watchdog.Stop()
		}
		mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			wrote.Store(info.Err == nil)
			if silence <= 0 || info.Err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if done {
				return
			}
			watchdog = time.AfterFunc(silence, func() {
				if !answered.Load() {
					cancel(errUpstreamSilent)
				}
			})
		},
		GotFirstResponseByte: func() { answered.Store(true) },
	}
	req, err := build(httptrace.WithClientTrace(ctx, trace))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp, err := client.Do(req)
	stopWatchdog()
	if err != nil {
		silent := errors.Is(context.Cause(ctx), errUpstreamSilent)
		cancel(nil)
		if parent.Err() != nil {
			return nil, err // the client went away: never a resend
		}
		if silent {
			return nil, fmt.Errorf("%w: %w", errUpstreamSilent, err)
		}
		// Only the exact shape of a dropped keep-alive connection: our request fully written on a
		// REUSED connection, not one byte of response, then EOF. A partial response ends in
		// io.ErrUnexpectedEOF instead and is not matched; a reset, a timeout, or a fresh connection
		// closed on us are real failures and are returned as they are.
		if reused.Load() && wrote.Load() && !answered.Load() && errors.Is(err, io.EOF) {
			return nil, &droppedReuse{err: err}
		}
		return nil, err
	}
	// Headers arrived, so the watchdog's job is done. The attempt's context must stay alive while
	// the body is read, so it is released when the caller closes the body.
	answered.Store(true)
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
