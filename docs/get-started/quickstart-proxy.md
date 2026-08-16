# Quickstart: Proxy

Run context-guru as a standalone proxy in front of your LLM provider, point an
agent at it, and watch the tokens drop. One port serves both the OpenAI and
Anthropic dialects.

## Prerequisites

- **Go 1.26** and a **C toolchain** — `CGO_ENABLED=1` (bifrost's tokenizer and,
  with the `cg_skeleton` tag, tree-sitter, use cgo).

bifrost is a normal module dependency (`go.mod` pins `github.com/maximhq/bifrost/core`),
so there is nothing to check out beside this repo — build straight from the repo root.

## Build

=== "go build"

    ```sh
    # from the repo root
    CGO_ENABLED=1 go build -tags cg_skeleton -o bin/context-guru-proxy ./cmd/context-guru-proxy
    ```

    The `cg_skeleton` tag is optional — it enables the tree-sitter–backed
    [`skeleton`](../components/skeleton.md) component. Drop it (and the cgo tree-sitter
    dependency) for a pure-Go build; every other component is unaffected.

    !!! warning "Without the tag, `skeleton` is not *inert* — it is not registered"
        A config or preset naming it then fails at pipeline build with
        `components: unknown component "skeleton"`, and the proxy exits rather than
        starting without it. That is deliberate — a silently-missing component would be
        worse — but it means the **`coding` preset needs a `cg_skeleton` build**, as does
        any pipeline listing `skeleton`. `make build` does **not** pass the tag; use the
        `go build` line above for a coding-agent binary.

=== "make"

    ```sh
    make build
    ```

=== "docker"

    ```sh
    docker build -t context-guru:local .
    ```

## Run

```sh
./bin/context-guru-proxy                       # default preset: codesmart; or --preset <name> / --config cg.yaml
```

It listens on `:4000` by default (set `LISTEN_ADDR` to change). See
[Config & environment](../reference/config.md) for all flags and env vars, and
[Presets](../reference/presets.md) to pick a pipeline.

## Point an agent at it

Set the base URL to the proxy — one port, both dialects:

=== "Anthropic"

    ```sh
    ANTHROPIC_BASE_URL=http://localhost:4000/anthropic
    ```

=== "OpenAI"

    ```sh
    OPENAI_BASE_URL=http://localhost:4000/openai/v1
    ```

In gateway mode, set `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` on the proxy and it
injects the real key on forward; leave them empty to pass the client's auth
through.

## Send a request

=== "OpenAI"

    ```sh
    curl -s -XPOST localhost:4000/openai/v1/chat/completions \
      -H 'content-type: application/json' \
      -H "Authorization: Bearer $YOUR_KEY" \
      -d '{
        "model": "gpt-4o-mini",
        "messages": [
          {"role": "user", "content": "list users"},
          {"role": "tool", "tool_call_id": "c1",
           "content": "[{\"id\":1,\"name\":\"Alice\"},{\"id\":2,\"name\":\"Bob\"}]"}
        ]
      }'
    ```

=== "Anthropic"

    ```sh
    curl -s localhost:4000/anthropic/v1/messages \
      -H 'content-type: application/json' \
      -H "Authorization: Bearer $YOUR_KEY" \
      -d '{
        "model": "claude-sonnet-4-5",
        "max_tokens": 64,
        "messages": [
          {"role": "user", "content": "read the config"},
          {"role": "assistant", "content": [
            {"type": "tool_use", "id": "t1", "name": "Bash", "input": {}}]},
          {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "t1",
             "content": "[{\"id\":1,\"name\":\"Alice\"},{\"id\":2,\"name\":\"Bob\"}]"}]}
        ]
      }'
    ```

    Tool outputs on the Anthropic dialect ride in `tool_result` blocks inside a user
    message; `apply` normalizes them so every component sees them the same way.

## Check savings

```sh
curl -s localhost:4000/stats | jq        # token-weighted savings rollup
```

`/stats` reports `tokens_before/after`, `saved_tokens`, `savings_pct`
(token-weighted), plus `wasted_tokens`/`bounces` and per-component rollups. See
[Measure savings](../how-to/measure-savings.md).

## Recover an offloaded original

A lossy Offload leaves a `<<cg:HASH>>` marker; recover the stashed original by
its id:

```sh
curl -s 'localhost:4000/expand?id=<HASH>'
```

In normal operation the model recovers it automatically via the
`context_guru_expand` tool — see [Recover offloaded context](../how-to/recover-context.md).

## Per-request headers

| Header | Effect |
|---|---|
| `x-context-guru-session: <id>` | Set the session key explicitly (otherwise a content hash). |
| `x-context-guru-bypass: true` | Skip the pipeline entirely for this request. |

The full route and header reference is in [Routes & headers](../reference/routes.md).
