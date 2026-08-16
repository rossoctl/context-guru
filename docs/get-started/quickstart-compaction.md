# Quickstart: compaction service

Run context-guru as a stateless HTTP service that shrinks a request body and hands it
straight back — the pattern
[`llm-d-router`](https://github.com/ronenkat/llm-d-router)'s `request-inline-compaction`
step calls.

## Steps

1. Build:

    ```sh
    ./examples/llm-d-service/build.sh        # → bin/context-guru-proxy
    ```

2. Run it. The deterministic `toon` config needs no credentials:

    ```sh
    bin/context-guru-proxy --config examples/llm-d-service/configs/toon.yaml
    # listens on :4000 (set LISTEN_ADDR to change)
    ```

3. Post a request body to `/compact`:

    ```sh
    curl -s -XPOST localhost:4000/compact -H 'content-type: application/json' -d '{
      "model": "gpt-4o-mini",
      "messages": [
        {"role": "user", "content": "list users"},
        {"role": "tool", "tool_call_id": "c1",
         "content": "[{\"id\":1,\"name\":\"Alice\",\"role\":\"admin\"},{\"id\":2,\"name\":\"Bob\",\"role\":\"user\"},{\"id\":3,\"name\":\"Carol\",\"role\":\"admin\"},{\"id\":4,\"name\":\"Dave\",\"role\":\"user\"},{\"id\":5,\"name\":\"Eve\",\"role\":\"admin\"}]"}
      ]
    }'
    ```

You get the same request back with the tool output re-encoded as TOON — field names once,
then one row per element:

```
[5]{id,name,role}:
1,Alice,admin
2,Bob,user
...
```

## The contract

```
POST <service>/compact         body = the inference request JSON
  200 + non-empty JSON  ->  caller swaps that body in (now smaller)
  anything else          ->  caller keeps the original (passthrough)
```

This mode is **one-way by design**: it sets `store: { enabled: false }` and
`marker_mode: "off"`, so nothing is stored, no `<<cg:HASH>>` markers appear, and the returned
body is directly usable as an inference request. Compaction here is irreversible.

<details markdown="1">
<summary>Troubleshooting</summary>

**The body came back unchanged.** Expected in three cases, all of which return the original
body with `200`: the output is below a component's `min_tokens` gate (or below an LLM
config's `trigger`); the body has no `messages` array or does not parse; a component failed.
The service never calls upstream and never errors your caller. Send a larger tool output to
see compaction act.

**I need markers and recovery.** Then you want the proxy, not this mode — see
[Quickstart: proxy](quickstart-proxy.md).

</details>

The full walkthrough — LLM-backed configs (`extract`, `summarize`), the Go client,
per-request overrides, and wiring into `llm-d-router` — is in the
[llm-d compaction service example](../examples/llm-d-service.md).
