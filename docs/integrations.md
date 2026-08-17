# Integrations

The same `components` core runs behind three host adapters. All of them ultimately call the
same pipeline over the same bifrost schema — hosts differ only in how they obtain the body,
provider, and session id, and how they run the expand loop.

| Option | Host code | Body source | Expand loop | Status |
|---|---|---|---|---|
| Proxy / gateway | `proxy/` + `cmd/context-guru-proxy/` | HTTP request body | server-side, wraps the chat route | shipped; the eval-containers gateway |
| AuthBridge plugin | `cortex` (imports this module) | `pctx.Body` | response path (`OnResponse`) | plugin lives externally |
| bifrost `LLMPlugin` | `adapters/bifrost/` | `req.ChatRequest` | transport wrapper | adapter shipped |

## Option A — proxy / gateway

`context-guru-proxy` is a direct HTTP proxy (it reuses bifrost's `ChatMessage` **type**, not
its transport). One port serves both dialects and forwards to the configured upstream, injecting
the real provider key in gateway mode.

```mermaid
sequenceDiagram
  participant Agent
  participant Proxy as proxy.Handler
  participant Apply as apply.Body
  participant Up as Upstream
  Agent->>Proxy: POST /anthropic/v1/messages (or /openai/v1/chat/completions)
  Proxy->>Proxy: FORCE_MODEL? overwrite "model"
  Proxy->>Apply: body, provider, x-context-guru-session, bypass?
  Apply-->>Proxy: rewritten body (fail open → original)
  Proxy->>Up: forward (drop placeholder auth, inject real key)
  Up-->>Proxy: response
  loop up to 3 rounds
    Proxy->>Proxy: expand tool called (only)? resolve from Store, re-invoke
  end
  Proxy-->>Agent: response (streaming passes straight through)
```

Key behaviors (`proxy/proxy.go`):
- **Routes** — `POST /openai/v1/chat/completions`, `POST /anthropic/v1/messages`, `GET /healthz`,
  `GET /stats`, `GET /expand?id=`.
- **Gateway credential model** — when `OPENAI_API_KEY`/`ANTHROPIC_API_KEY` are set, the client's
  auth is dropped and the real key injected on forward (the agent holds only a placeholder).
  Empty keys → client auth passes through (local/dev).
- **`FORCE_MODEL`** pins every call's model (eval-containers `EVAL_MODEL`).
- **Expand loop** runs server-side before returning, capped at `maxExpandRounds = 3`. Streaming
  (`event-stream`) responses skip the loop and pass through with flushing.
- `x-context-guru-*` headers are stripped before forwarding upstream.

Run it: see the [README](https://github.com/rossoctl/context-guru/blob/main/README.md) flag table and [setup.md](setup.md).

## Option B — AuthBridge in-process plugin

The plugin lives in **cortex** (`authlib/plugins/contextguru/`) and imports only this
module — not bifrost internals. It is an outbound `WritesBody` plugin gated to inference paths.

```mermaid
sequenceDiagram
  participant Agent
  participant AB as AuthBridge plugin
  participant Apply as apply.Body
  participant Up as Upstream
  Agent->>AB: outbound request (pctx.Body)
  AB->>Apply: pctx.Body, provider, pctx.Session
  Apply-->>AB: rewritten bytes
  AB->>AB: pctx.SetBody(rewritten)
  AB->>Up: forward
  Up-->>AB: response
  AB->>AB: OnResponse: expand markers via expand/ + Store, continue
  AB-->>Agent: response
```

The plugin reuses `apply.Body` (bytes in → pipeline → bytes out), `session.Resolve` (from
`pctx.Session`), the shared `Store`, and `expand/` for the response-path continuation. Config
arrives as `json.RawMessage` into the same `config` struct the proxy uses; metrics surface via
AuthBridge's `StatsSource`.

## Option C — bifrost LLMPlugin (embed in a bifrost deployment)

`adapters/bifrost.Plugin` implements bifrost's `schemas.LLMPlugin`. Registering it via
`BifrostConfig.LLMPlugins` runs the pipeline as a `PreRequestHook` inside any bifrost proxy.

```go
p := bifrost.New(pipe, store)   // pipe from config.Build, store from config.NewStore
// register p in BifrostConfig.LLMPlugins
```

- `PreRequestHook` runs the pipeline over `req.ChatRequest` in place (the canonical mutate phase);
  non-chat requests pass through. It never aborts a request (fail-open is inside the pipeline).
- Session id comes from the `context-guru-session` context value (set by the transport from the
  header / Anthropic `metadata.user_id`), else the content-hash fallback.
- `PreLLMHook`/`PostLLMHook` are pass-throughs — the expand loop belongs in a transport wrapper
  (it must re-invoke upstream, which a hook cannot).

## Use it with an agent — Bob (BobShell)

IBM Bob is OpenAI-compatible via its `CUSTOM_BASE_URL`, but it calls **Bob-specific
paths**: its model call is `POST /inference/v1/chat/completions`, and it makes
control-plane calls (`GET /admin/v1/profile`, `/inference/v1/model/info`, …) that must
reach the backend unmodified or the CLI won't boot. The proxy has an opt-in **Bob
gateway** for exactly this shape — enable it with `BOB_UPSTREAM` (or `--bob-upstream`):

```sh
BOB_UPSTREAM=https://api.us-east.bob.ibm.com \
  context-guru-proxy --preset balanced        # any deterministic preset/config
```

Then point Bob at the proxy and let it use its own key:

```sh
CUSTOM_BASE_URL=http://localhost:4000/v1 \
BOBSHELL_DEFAULT_AUTH_TYPE=custom \
BOBSHELL_API_KEY=<your bob key> \
  bob --yolo "your task"
```

How the gateway routes Bob's traffic:

- **Model calls** (`/inference/v1/chat/completions`) run through the pipeline like any
  OpenAI chat and are forwarded to the same path on `BOB_UPSTREAM`. Bob's own auth is
  passed straight through (no key injection).
- **Control-plane calls** (everything else Bob hits) are proxied **verbatim** to
  `BOB_UPSTREAM`, so Bob authenticates and starts normally.

!!! warning "Use lossless components with Bob — it cannot expand a marker"
    Bob sends no `tools` array and parses tool calls as XML tags out of the streamed
    response text, so it has no way to call `context_guru_expand`. A `<<cg:HASH>>` marker
    is a dead end in a Bob session: whatever a lossy component removed is gone for good.

    Use the lossless components — `format`, `toon`, `cachesplit` — which need no
    cheap-model config and leave the transcript intact. Verified end to end: with
    `[format, toon]` Bob authenticates and answers correctly through the proxy with its
    model call reduced. Add a lossy component (`mask`, `dedup`, `cmdfilter`, `extract`)
    only where permanently losing that content is acceptable.

!!! note "Bob speaks its own backend protocol"
    Unlike Claude Code (`ANTHROPIC_BASE_URL`) or OpenAI-surface agents (`OPENAI_BASE_URL`),
    Bob's `CUSTOM_BASE_URL` points at the proxy **host**; Bob supplies the `/inference` and
    `/admin` paths itself. The gateway only activates when `BOB_UPSTREAM` is set, so it
    never changes behavior for the other integrations above.
