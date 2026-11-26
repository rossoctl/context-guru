// Package dispatch is the one way a context-guru core package reaches an external service.
//
// The core edits context and makes decisions. It never creates an HTTP client, reads an API key
// or holds a credential: those are the host's (cmd/context-guru-proxy, the Cortex plugin, any
// other embedder). So the split is fixed here, once, for every call:
//
//   - The core builds the request — path, body, non-secret headers — and parses the response.
//   - The host resolves Ref to a base URL and a credential, applies its transport policy, and
//     sends it.
//
// The core names a destination only by a Ref the host issued, so it cannot send anywhere the host
// did not choose, and it never sees the credential that goes with it.
package dispatch

import (
	"context"
	"errors"
	"net/http"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Ref is an opaque handle the HOST issues. It names a destination and a credential that only the
// host can resolve. The core passes it back unchanged and never reads it.
type Ref string

// Price is one model's per-token rates in USD. An alias, so every host and the core price with one
// formula (Price.CostWithCacheWrite1h).
type Price = modelinfo.Price

// ErrNotHeld is what Do returns when the host holds nothing for the Ref: unknown, or released.
// A host returning it has sent nothing — never an unauthenticated request in its place.
var ErrNotHeld = errors.New("dispatch: nothing held for ref")

// MaxResponseBytes is the most of a response body a host reads. Every call the core makes is
// small and non-streaming; an upstream answering with more is not one to buffer.
const MaxResponseBytes = 1 << 20

// Purposes the core sends, for host policy and metrics.
const (
	PurposeKeepAlivePing = "keepalive.ping"
	PurposeCheapModel    = "cheapmodel.complete"
)

// Request is one call, built by the core.
type Request struct {
	Purpose  string
	Ref      Ref
	Provider bschemas.ModelProvider
	Method   string // "" means POST
	// Path is the upstream path, e.g. "/v1/messages". The host owns the base URL.
	Path string
	// Header is non-secret only. No core path puts a credential here; the host adds the one it
	// holds for Ref.
	Header http.Header
	// Body is owned by the caller. Some callers (keepalive, holding a conversation transcript)
	// zero it once Do returns, because it is security-sensitive; others (cheapmodel, an ordinary
	// request body) do not, because it is not. Either way, a host must not keep this slice past
	// Do returning, and must not copy it into a string, which cannot be zeroed. A host that needs
	// the bytes after Do returns (a transport may still be writing them) copies them into a
	// []byte it wipes itself — never assuming the caller will do that wiping for it.
	Body []byte
}

// Response is what the upstream answered. Body is bounded at MaxResponseBytes.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Dispatcher is the host's half of every external call: transport and credential custody, never a
// decision.
//
// A host must:
//   - return ErrNotHeld, and send nothing, for a Ref it holds nothing for;
//   - never follow a redirect, so a credential cannot reach a host nobody chose;
//   - read at most MaxResponseBytes of a response.
type Dispatcher interface {
	// Do sends one request. A non-nil error is a transport failure (or ErrNotHeld). A non-2xx
	// status is returned in Response with a nil error, and the core decides what it means.
	Do(ctx context.Context, req Request) (Response, error)
	// Release drops what the host holds for ref. The core calls it exactly once for each Ref a
	// hold handed it. It may run under a core lock, so it must not call back into the core. A Do
	// already in flight for ref must keep working on its own copy.
	Release(ref Ref)
	// Price resolves a model's rates at an endpoint (the host's own form: base URL, host name).
	// promptTokens is the billed prefix, for tiered tables. ok=false means unknown, never free.
	Price(ctx context.Context, endpoint, model string, promptTokens int64) (Price, bool)
}
