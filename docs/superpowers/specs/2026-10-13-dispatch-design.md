# Design: one host Dispatcher for every external call

Date: 2026-10-13
Branch: `feat/export-keepalive-keeper` (engine), `feat/contextguru-keepalive` (Cortex)

## Summary

context-guru (CG) is a library that edits context. It runs under different hosts: the
standalone proxy, Cortex, bifrost. **The core must not interact with external services.** It
must not create HTTP clients, read API keys or hold credentials. The host does all of that.

Today `keepalive.Dispatcher` gives this split to the keep-alive ping only. This design makes
it general:

- A new core package `dispatch` defines one `Dispatcher` that every core component uses for
  every external call.
- The core builds the wire format (path, body, non-secret headers) and parses the response.
- The host resolves the destination and the credential, applies its transport policy and sends
  the request.
- The `Dispatcher` also prices calls (`Price`), so the host owns the rate table.

## Goal and success criteria

1. No core package uses `http.Client`, `http.DefaultClient`, `http.Get`, `http.Post`, or
   `os.Getenv` to read a key. A test enforces this.
2. No core package holds or masks a credential. Credential masking is each host's own business.
3. The same core runs unchanged under the proxy and under Cortex.
4. The proxy's behavior does not change, except the one planned config break (see
   "Component `model:` config").

## What exists today

| Component | External call | How it sends |
|---|---|---|
| `keepalive` | Cache-warming ping | Host `keepalive.Dispatcher` (`Dispatch` + `Release`), prices through `Options.Prices` |
| `internal/cheapmodel` | Cheap-model completion, used by `offload` and summaries | Own `*http.Client`; stores `BaseURL`, `APIKey`, `AuthScheme` |
| `internal/modelinfo.LiteLLM` | `GET` of the LiteLLM price table | Own `*http.Client`, 10 s timeout |
| `components/offload/modelcfg.go` | (none, but reads secrets) | Reads `api_key` from component config, else `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` |
| `credmask` | (none) | Shared mask, used by `keepalive`, `proxy` and Cortex |

Out of scope: the proxy's forwarding of the client's own request (`proxy/proxy.go`,
`proxy/counttokens.go`), `dash`, `proxy/mail.go`, `proxy/promexport.go`. These are host code.

## Design

### The `dispatch` package

```go
package dispatch

// Ref is an opaque handle that the HOST issues. It names a destination and a credential that
// only the host can resolve. The core passes it back and never reads it.
type Ref string

// ErrNotHeld is returned by Do when the host holds nothing for Ref (unknown or released).
var ErrNotHeld = errors.New("dispatch: nothing held for ref")

type Request struct {
	Purpose  string                 // "keepalive.ping", "cheapmodel.complete": for host policy and metrics
	Ref      Ref
	Provider bschemas.ModelProvider
	Method   string
	Path     string                 // e.g. "/v1/messages". The host owns the base URL.
	Header   http.Header            // non-secret headers only
	Body     []byte                 // owned by the caller and zeroed after Do returns
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte                   // bounded by the host at 1 MiB
}

type Dispatcher interface {
	// Do sends one request. A non-nil error is a transport failure. A non-2xx status is
	// returned in Response with a nil error, and the core decides what it means.
	Do(ctx context.Context, req Request) (Response, error)
	// Release drops what the host holds for ref. The core calls it exactly once for each Ref
	// that a Hold returned. It may run under a core lock, so it must not call back into the core.
	// A Do already in flight for ref must keep working on its own copy.
	Release(ref Ref)
	// Price returns rates for a model at an endpoint. ok=false means unknown, never free.
	Price(ctx context.Context, endpoint, model string, promptTokens int64) (modelinfo.Price, bool)
}
```

`dispatch` imports `net/http` for the `http.Header` type only.

### Where a `Ref` comes from

| Use | Who issues it | Lifetime |
|---|---|---|
| Keep-alive session | Host, from `Record.Hold`, which changes from `func()` to `func() dispatch.Ref` | Until the keeper calls `Release` |
| Cheap model, `source: incoming` | Host, when it resolves `Ctx.Model` for a request | The request. The host drops it internally; the core does not call `Release` |
| Cheap model, `source: config` | Host, a fixed handle for the operator's configured model | The host process |

### Host contract

- Resolve `Ref` to a base URL and credential. If nothing is held, return `ErrNotHeld` and
  send nothing.
- Add the credential, in the form the credential needs (`x-api-key` or `Authorization: Bearer`).
  The auth scheme is host business; the proxy records the caller's scheme at hold time.
- Never follow a redirect to another host.
- Read at most 1 MiB of the response.
- Never keep `Request.Body`, and never copy it into a `string`. The core zeroes it after `Do`
  returns, and a string copy cannot be zeroed. This fixes the copy at
  `proxy/keepalivedispatch.go:101` (`strings.NewReader(string(req.Body))`).

### Component changes

**`keepalive`**
- `Options.Dispatcher` becomes a `dispatch.Dispatcher`. `Options.Prices` is removed; pings are
  priced through `Dispatcher.Price`.
- `keepalive.Dispatcher`, `keepalive.DispatchRequest` and `keepalive.Pricer` are removed.
- The keeper stores the `Ref` that `Hold` returns and passes it to `Do` and `Release`.
- Pings use `Do` with `Purpose: "keepalive.ping"`, `Method: POST`, `Path: route` and
  `Header: PingHeaders(...)`.
- Usage parsing (`responseUsage` today, in `proxy`) moves into `keepalive`. It reads wire
  format; it does no I/O.
- The held ping body stays masked, with a small private mask inside `keepalive`. It is held
  data, not a credential.
- `keepalive/keepalivetest` is replaced by `dispatch/dispatchtest`.

**`cheapmodel`**
- `internal/cheapmodel` becomes the public package `cheapmodel`, so other hosts can build a
  cheap model.
- `Anthropic` and `OpenAI` lose `BaseURL`, `APIKey`, `AuthScheme` and `Client`. They hold
  `Dispatcher`, `Ref`, `Model` and `MaxTokens`.
- They build the provider body, call `Do` with `Purpose: "cheapmodel.complete"`, and parse
  the text and usage.
- They still implement `components.Model`, `components.Remodeler` and `components.Budgeter`, so
  no component changes.

**`modelinfo`**
- `Price`, `Pricer`, `Resolver`, `ExactResolver`, `DefaultStatic` and `NormalizeID` stay in the
  core. They do no I/O.
- `LiteLLM` moves to the host-side package `internal/modelinfo/litellm`. Only `proxy`,
  `proxyapp` and `cmd/` import it. The host gives the core a `Resolver`, as it does today.

**`credmask`**
- Moves into `proxy` as unexported code. Cortex keeps its own copy.

**`proxy`**
- One `proxy` `Dispatcher` serves every purpose. It replaces `httpDispatcher`.
- It resolves keep-alive refs from held, masked credentials; per-request refs from the
  current request's caller credential; and the operator ref from `AnthropicKey` / `OpenAIKey`.
- `cheapModel` (`proxy/proxy.go:800-828`) builds `cheapmodel` values over the `Dispatcher`.
- `modelPricer` becomes the `Dispatcher`'s `Price` method.

### Component `model:` config

The `model:` block keeps only `source` (`incoming` / `config`) and `model`.

- `api_key` and `base_url` under a component are rejected at load. The error names the host
  config that replaces them (the operator model, `source: config`).
- The environment-key fallback (`AllowEnvModelKey`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`) moves from `components/offload/modelcfg.go` to the proxy's host
  config.
- **This breaks configs that set `api_key` or `base_url` under a component.** The release notes
  must say so.

## Data flow

**Ping**
1. The keeper sweep selects a due entry and builds the ping body.
2. The keeper calls `Do` with the entry's `Ref`.
3. The host resolves the `Ref`, adds the credential and base URL, and sends the request.
4. The keeper parses usage, prices it with `Price`, and applies the cost cap and the
   read-not-write check.
5. The keeper zeroes the body.

**Cheap model**
1. The component calls `ctx.Model.Complete(...)`. The host built that model as
   `cheapmodel` over its `Dispatcher` and a `Ref`.
2. `cheapmodel` builds the body, calls `Do`, and parses the reply.

## Errors

| Case | Host | Core |
|---|---|---|
| `Ref` unknown or released | Returns `ErrNotHeld`, sends nothing | Ping: failed, not retried on that entry. Cheap model: the component fails open. |
| Transport error or timeout | Returns the error | Ping: retried on the next due tick. Cheap model: fails open. |
| Non-2xx status | Returns `Response`, nil error | A 4xx stops a keep-alive entry, as today. |
| Redirect | Does not follow it to another host | Sees the 3xx status |
| Response over 1 MiB | Cuts it at 1 MiB | Parse failure: ping counts as failed; cheap model fails open |

## Tests

1. **`dispatchtest.Contract(t, newHost)`.** Every host runs it. It checks:
   - `Do` with an unknown or released `Ref` returns `ErrNotHeld` and sends nothing.
   - A `Do` in flight still works when `Release` runs at the same time.
   - A redirect to another host is not followed, and the credential does not reach it.
   - The response body is bounded at 1 MiB.
   - Changing `Request.Body` after `Do` returns has no effect on the host.
   - `Price` for an unknown model returns `ok=false`.
2. **`dispatchtest.Fake`.** A recording `Dispatcher` with canned responses for core tests. The
   `cheapmodel` wire tests move from `httptest` servers to `Fake` and still check the exact
   request bytes and headers.
3. **Import guard.** Fails if a core package uses `http.Client`, `http.DefaultClient`,
   `http.Get`, `http.Post`, or `os.Getenv` for a key. Allow-list: `proxy`, `proxyapp`, `dash`,
   `cmd/`, `internal/modelinfo/litellm`, `examples/`.
4. **Proxy.** The `./proxy/...` tests pass without change, except the new rejection of
   `api_key` / `base_url` in a component `model:` block.
5. **Cortex.** `scripts/dev/verify-contextguru-keepalive.sh` still proves a correct ping, and
   Cortex's dispatcher passes `dispatchtest.Contract`.

## Rollout

The work goes on the current engine branch, `feat/export-keepalive-keeper`, because
`keepalive.Dispatcher` is not released yet. Merging it first would publish an API and then break
it.

**Engine.** Each step keeps the proxy tests green.
1. Add `dispatch` and `dispatchtest`. Move `keepalive` to `Do` / `Release` / `Price` and to
   `Hold func() Ref`. Move usage parsing into `keepalive`. Move `credmask` into `proxy` and give
   `keepalive` its private body mask.
2. Make `cheapmodel` public over the `Dispatcher`. The proxy `Dispatcher` serves operator and
   per-request refs.
3. Move `LiteLLM` to `internal/modelinfo/litellm`. Change the `model:` config block. Add the
   import guard. Update `docs/design.md` and `CLAUDE.md`.

**Cortex.**
1. `core/plugins/contextguru/dispatcher.go` implements `dispatch.Dispatcher`. `pricer.go`
   becomes its `Price` method. Cortex gets its own masking code. `Hold` returns a `Ref`.
2. Cortex gives components no cheap model in this change. That is a separate feature, which this
   design makes possible.
3. A tagged engine release replaces the dev `replace` directive in the three `go.mod` files
   before merge.

## Open questions

None.
