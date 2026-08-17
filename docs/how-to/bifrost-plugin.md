# Run as a bifrost LLMPlugin

Embed the context-guru pipeline inside an existing bifrost deployment — no separate process,
no transport of its own.

## Steps

1. Build a pipeline and store from your config:

    ```go
    pipe, err := cfg.Build(emitter)   // cfg is a *config.Config
    st := cfg.NewStore()
    ```

2. Construct the plugin and register it:

    ```go
    p := bifrost.New(pipe, st)
    // add p to BifrostConfig.LLMPlugins
    ```

3. Wrap the transport with the expand loop if you need reversibility (see below).

## Where it sits

- **`PreRequestHook`** runs the pipeline over `req.ChatRequest` in place — the canonical
  mutate phase. Non-chat requests pass through. It never aborts a request; fail-open lives
  inside the pipeline.
- **Session id** comes from the `context-guru-session` context value (set by the transport
  from the request header or Anthropic `metadata.user_id`), falling back to a content hash.
- **`PreLLMHook` / `PostLLMHook` are pass-throughs.** The expand loop cannot be a hook — it
  must re-invoke upstream. Put it in a transport wrapper: run the pipeline, forward, then
  resolve `<<cg:HASH>>` markers with the shared [`expand/`](recover-context.md) package and
  the store before returning, capped at 3 rounds. That is exactly what the shipped proxy
  does.

<details markdown="1">
<summary>Troubleshooting</summary>

**Markers reach my clients unexpanded.** The plugin alone does not recover anything — the
expand loop lives in the transport wrapper, not in a hook. Either add the wrapper or run a
lossless-only pipeline (`format`, `toon`, `cachesplit`) so no marker is ever written.

**Every request lands in the same session.** The transport has to set the
`context-guru-session` context value. Without it the fallback is a content hash, which
changes whenever the head of the conversation changes.

**Non-chat requests are untouched.** By design — `PreRequestHook` passes them through.

</details>

For the standalone proxy and the AuthBridge plugin, see
[Host adapters](../integrations.md).
