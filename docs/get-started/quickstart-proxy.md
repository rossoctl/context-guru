# Quickstart: proxy

Run context-guru in front of your provider and point an agent at it. One port serves both
the OpenAI and Anthropic dialects.

You need **Go 1.26**. You do **not** need a C toolchain: `make build` builds with cgo disabled, and
the result is a statically linked binary with no runtime dependencies. Everything else is a normal
module dependency — build straight from the repo root.

A C compiler is needed for exactly two things: `make test` (the race detector requires cgo) and the
optional [`skeleton`](../components/skeleton.md) component's `cg_skeleton` build tag.

## Steps

1. Build:

    ```sh
    make build                     # → bin/context-guru-proxy
    ```

2. Run it. It listens on `:4000`; set `LISTEN_ADDR` to change that.

    ```sh
    ./bin/context-guru-proxy       # default preset: house
    ```

3. Point your agent at it:

    ```sh
    ANTHROPIC_BASE_URL=http://localhost:4000/anthropic      # Anthropic dialect
    OPENAI_BASE_URL=http://localhost:4000/openai/v1         # OpenAI dialect
    ```

    Set `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` on the proxy and it injects the real key on
    forward; leave them unset to pass the client's own auth through.

4. Check the savings:

    ```sh
    curl -s localhost:4000/stats | jq
    ```

## Send one request by hand

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

<details markdown="1">
<summary>The same request on the Anthropic dialect</summary>

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

Tool outputs on the Anthropic dialect ride in `tool_result` blocks inside a user message;
`apply` normalizes them so every component sees them the same way.

</details>

## Per-request headers

| Header | Effect |
|---|---|
| `x-context-guru-session: <id>` | Set the session key explicitly (otherwise a content hash). |
| `x-context-guru-bypass: true` | Skip the pipeline entirely for this request. |

<details markdown="1">
<summary>Troubleshooting</summary>

**The proxy exits with `components: unknown component "skeleton"`.** The
[`skeleton`](../components/skeleton.md) component needs the `cg_skeleton` build tag, and
`make build` does not pass it. Build it explicitly:

```sh
CGO_ENABLED=1 go build -tags cg_skeleton -o bin/context-guru-proxy ./cmd/context-guru-proxy
```

The `coding` preset, and any pipeline listing `skeleton`, needs that binary. Without the tag
the component is not registered, and the proxy refuses to start rather than quietly running
a pipeline that is missing a component you asked for. Every other component is unaffected.

**`/stats` shows zero requests.** The agent is not reaching the proxy. Confirm the base URL
includes the dialect path (`/anthropic`, or `/openai/v1`), and note that Claude Code's
`~/.claude/settings.json` `env` block overrides an exported `ANTHROPIC_BASE_URL` — see
[Use with Claude Code](../how-to/use-with-claude-code.md).

**I need a container instead.** `docker build -t context-guru:local .`

**I want to recover an offloaded original by hand.** A lossy Offload leaves a `<<cg:HASH>>`
marker; `curl -s 'localhost:4000/expand?id=<HASH>'` returns the stashed original. In normal
operation the model recovers it automatically — see
[Reversibility & recovery](../how-to/recover-context.md).

</details>

See also: [Config & environment](../reference/config.md) ·
[Choose a preset](../how-to/choose-a-preset.md) · [Routes & headers](../reference/routes.md) ·
[Measure savings](../how-to/measure-savings.md)
