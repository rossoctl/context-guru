# llm-d compaction service

A ready-to-run example that turns context-guru into a stateless HTTP **compaction service** for
[`llm-d-router`](https://github.com/ronenkat/llm-d-router). It plugs into the router's
`request-inline-compaction` step: the router POSTs an inference request, context-guru shrinks the
`messages` array and hands back a smaller request of the exact same shape.

!!! note "Full example on GitHub"
    Source, configs, build script, and Go client:
    [`examples/llm-d-service`](https://github.com/rossoctl/context-guru/tree/main/examples/llm-d-service).

## Quickstart

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

    If the body comes back unchanged, that's expected in three cases (all return `200` with the
    original body): the output is below a component's `min_tokens` gate, the body has no
    `messages` array, or a component failed. The service never calls upstream and never errors
    your caller. Send a larger tool output to see compaction act.

## The contract

The router's `request-inline-compaction` step calls an external service with a simple contract:

```text
POST <service>/compact         body = the inference request JSON
  200 + non-empty JSON  ->  router swaps that body in (now smaller)
  anything else          ->  router keeps the original (passthrough)
```

This service is that endpoint. It parses `messages[]`, runs the pipeline, splices the result back,
and returns `200` — **with no upstream call, fail-open, and no markers**.

```text
             ┌─────────────────────── /compact ───────────────────────┐
 request ───▶│  parse messages[] ─▶ run pipeline ─▶ splice back  ─▶ 200 │───▶ smaller request
   (JSON)    │       (format · toon · extract · summarize · …)          │        (same schema)
             └──────────────────────────────────────────────────────────┘
                         no upstream call · fail-open · no markers
```

## Stateless by design

This is the [reversibility](../how-to/recover-context.md) story turned **off** on purpose:

- **`store: { enabled: false }`** — nothing is stashed.
- **`marker_mode: "off"`** — no `<<cg:HASH>>` markers appear.

The result is a clean, directly-usable inference request body — compaction is **one-way**. The
router owns request routing; context-guru just returns a smaller body it can forward as-is.

Send the step's opt-in header on requests you want compacted:

```text
x-llm-d-optimization: compaction
```

## The three committed configs

| Config | Component(s) | LLM? | What it does |
|---|---|:--:|---|
| `configs/toon.yaml` | `format` + `toon` | no | Re-encodes uniform JSON object-arrays as **TOON** (field names once, then one row per element). Deterministic, zero-cost, zero-latency. |
| `configs/extract-code.yaml` | `extract` (`strategy: code`) | yes | A cheap LLM writes a sandboxed filter that **deletes irrelevant lines** from large tool outputs — verified deletion-only, never invents text. |
| `configs/summarize.yaml` | `summarize` | yes | A cheap LLM **summarizes the middle** of a long transcript into one message, keeping the head + last few turns. |

All three set `store: { enabled: false }` and `marker_mode: "off"`.

!!! tip "Per-request overrides ride on headers/query params"
    The router forwards the body verbatim, so overrides go on the request:
    `?provider=anthropic`, `?preset=<name>`, `x-context-guru-pipeline: format,toon`,
    `x-context-guru-session: <id>`, `x-context-guru-bypass: true`.

## Choosing a config

- **Deterministic, no credentials** → `toon.yaml`. Best for structured/tabular tool output.
- **Big noisy outputs, only a slice relevant** → `extract-code.yaml`. Needs a cheap model.
- **Long transcripts where the history is the cost** → `summarize.yaml`. Needs a cheap model.

The LLM configs use a cheap model you point at any OpenAI- or Anthropic-compatible endpoint. If no
model is reachable they degrade gracefully — `extract` falls back to a deterministic line
projection and `summarize` no-ops — and the request still returns `200`.

See all three run on one input, with the full `messages` before and after, in the
[Before → After showcase](live-captures.md#before-after-showcase).
