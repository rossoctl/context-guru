# Use with Claude Code

[Claude Code](https://docs.claude.com/en/docs/claude-code) speaks the **Anthropic**
dialect and honors the `ANTHROPIC_BASE_URL` override, so routing it through context-guru
takes one environment variable — no changes to Claude Code itself.

```
Claude Code ──ANTHROPIC_BASE_URL──▶ context-guru :4000/anthropic ──▶ Anthropic
                                     (compacts tool outputs)          (or Bedrock/Vertex)
```

## The one-liner

Start the proxy, then point Claude Code at it:

```sh
context-guru-proxy --preset agent          # long-session preset; mask is the big lever

ANTHROPIC_BASE_URL=http://localhost:4000/anthropic claude
```

That's the whole integration. Every request Claude Code makes now flows through the
pipeline; the response path resolves any `context_guru_expand` calls automatically.

!!! warning "A `settings.json` `env` block silently overrides the environment variable"
    If `~/.claude/settings.json` contains an `env` block that sets `ANTHROPIC_BASE_URL`,
    **it wins over the variable you exported**, and the one-liner above does nothing.

    This failure is silent and it looks exactly like success: Claude Code answers
    normally, there is no error and no warning, and the only symptom is that you get no
    savings and your dashboard stays empty — because the traffic never reached the proxy
    at all. It is worth 30 seconds to rule out before debugging anything else.

    **Check first:**

    ```sh
    python3 -c "import json;print(json.load(open('$HOME/.claude/settings.json')).get('env',{}).keys())"
    ```

    **Confirm routing** — the honest test is to point Claude Code somewhere impossible
    and check that it *fails*. If it still answers, it is not using your base URL:

    ```sh
    ANTHROPIC_BASE_URL=https://127.0.0.1:1/nope ANTHROPIC_AUTH_TOKEN=bogus \
      claude -p 'say PONG' --max-turns 1     # must FAIL. If it prints PONG, see below.
    ```

    Then verify positively, by watching the proxy's own counter move:

    ```sh
    curl -s localhost:4000/stats | python3 -c 'import json,sys;print(json.load(sys.stdin)["requests"])'
    ```

    **Two ways to fix it.** Either put the override in the settings file (where it will
    actually be read), or pass a per-invocation settings file so your global config is
    left alone:

    ```sh
    claude --settings '{"env":{"ANTHROPIC_BASE_URL":"http://localhost:4000/anthropic"}}'
    ```

!!! tip "Which preset?"
    Claude Code sessions are long and dominated by re-sent tool outputs (file reads,
    command logs). The default **`codesmart`** preset is the cache-aware config that won
    the live [benchmarks](../RESULTS.md) — the cheapest arm (**−13.2%** billed cost vs a
    no-compaction baseline) at the **highest** reward (88% tasks solved). Use `coding` if
    you want `skeleton` to strip function bodies from big source reads. See
    [Choose a preset](choose-a-preset.md).

## With a wrapper script

[`scripts/with-guru.sh`](https://github.com/rossoctl/context-guru/blob/main/scripts/with-guru.sh)
starts the proxy, exports the base-URL env, and runs any agent command through it:

```sh
scripts/with-guru.sh agent -- claude
```

## Gateway mode (proxy holds the key)

Give the proxy the real key and hand Claude Code a placeholder — the proxy injects the
real credential on forward, so the key never lives in Claude Code's config:

```sh
ANTHROPIC_API_KEY=sk-ant-real... context-guru-proxy --preset agent

ANTHROPIC_BASE_URL=http://localhost:4000/anthropic \
ANTHROPIC_AUTH_TOKEN=placeholder \
  claude
```

Leave `ANTHROPIC_API_KEY` unset on the proxy to pass Claude Code's own auth straight
through instead.

## Project-scoped via `.claude/settings.json`

To make it sticky for a repo without exporting env each time:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4000/anthropic"
  }
}
```

## Verify it's working

```sh
curl -s localhost:4000/stats | jq
```

`requests` climbs as Claude Code makes calls, and `savings_pct` / per-component
`saved_tokens` show what each component removed. See [Measure savings](measure-savings.md).

!!! note "Pin the model (optional)"
    context-guru forwards whatever `model` Claude Code sends. To force a model
    regardless (e.g. in eval harnesses), set `FORCE_MODEL` on the proxy. Claude Code's
    own model env (`ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`) still selects the
    model it asks for.

See also: [Quickstart: Proxy](../get-started/quickstart-proxy.md) · [Run behind a proxy or gateway](../integrations.md) · [Recover offloaded context](recover-context.md)
