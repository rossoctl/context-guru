package keepalive

import (
	"context"
	"net/http"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// DispatchRequest is one ping, assembled by the keeper and handed to the host's Dispatcher.
type DispatchRequest struct {
	Key      Key
	Provider bschemas.ModelProvider
	Model    string
	// Route is the upstream path the recorded request used ("/v1/messages", "/v1/responses").
	Route string
	// Body is the exact ping bytes. Owned by the keeper and zeroized after Dispatch returns:
	// a Dispatcher must not retain it.
	Body []byte
	// Headers is non-secret only, built by PingHeaders from a fixed allow-list. No path puts a
	// credential here; the Dispatcher attaches the one it Held for Key.
	Headers http.Header
}

// Dispatcher is the host's half of a ping: transport and credential custody, never a decision.
//
// A host resolves the held credential for req.Key when Dispatch RUNS, not when the sweep that
// scheduled this ping selected it — the two can be seconds apart, since the ping is sent on its
// own goroutine outside the keeper's lock. If the entry was released in between (a real request
// Arrived, the hard deadline fired, Shutdown ran), Dispatch finds nothing held for that key and
// must return an error rather than send unauthenticated. The keeper counts that as a failed ping
// and does not retry it on that entry: the entry is already gone by the time the error comes
// back.
type Dispatcher interface {
	// Dispatch sends one ping. A non-nil error is transient (the keeper retries on the next due
	// tick); a 4xx status stops the entry; 2xx usage feeds the write-vs-read guard.
	Dispatch(ctx context.Context, req DispatchRequest) (u Usage, status int, err error)
	// Release zeroizes whatever the host Held for key. Called exactly once per Hold, UNDER THE
	// KEEPER'S LOCK — it must not call back into the Keeper. A Dispatch already in flight for
	// key must keep working on its own copy.
	Release(key Key)
}

// PingHeaders copies the non-secret headers a ping needs from the recorded request.
func PingHeaders(src http.Header) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project"} {
		if v := src.Get(name); v != "" {
			h.Set(name, v)
		}
	}
	return h
}
