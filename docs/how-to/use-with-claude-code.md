# Use with Claude Code

Route [Claude Code](https://docs.claude.com/en/docs/claude-code) through context-guru with
one environment variable — no changes to Claude Code itself.

## Steps

1. Start the proxy:

    ```sh
    context-guru-proxy --preset codesmart
    ```

2. Point Claude Code at it:

    ```sh
    ANTHROPIC_BASE_URL=http://localhost:4000/anthropic claude
    ```

3. Check that traffic is arriving:

    ```sh
    curl -s localhost:4000/stats | jq '.requests, .savings_pct'
    ```

That is the whole integration. Every request now flows through the pipeline, and the
response path resolves `context_guru_expand` calls automatically.

## Make it stick for a repo

Add to `.claude/settings.json` so you don't export anything by hand:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:4000/anthropic"
  }
}
```

## Keep the API key out of Claude Code

Give the proxy the real key and hand Claude Code a placeholder; the proxy injects the
credential on forward:

```sh
ANTHROPIC_API_KEY=sk-ant-real... context-guru-proxy --preset codesmart

ANTHROPIC_BASE_URL=http://localhost:4000/anthropic \
ANTHROPIC_AUTH_TOKEN=placeholder \
  claude
```

Leave `ANTHROPIC_API_KEY` unset on the proxy to pass Claude Code's own auth straight
through instead.

<details markdown="1">
<summary>Troubleshooting</summary>

**No savings, and the dashboard stays empty.** Almost always an `env` block in
`~/.claude/settings.json` that sets `ANTHROPIC_BASE_URL`. It wins over the variable you
exported, and the failure is silent — Claude Code answers normally, the traffic just never
reaches the proxy. Check it:

```sh
python3 -c "import json;print(json.load(open('$HOME/.claude/settings.json')).get('env',{}).keys())"
```

Fix it by putting the override in the settings file, or by passing a per-invocation
settings file so your global config is left alone:

```sh
claude --settings '{"env":{"ANTHROPIC_BASE_URL":"http://localhost:4000/anthropic"}}'
```

**Prove the routing rather than assume it.** Point Claude Code somewhere impossible and
check that it *fails*. If it still answers, it is not using your base URL:

```sh
ANTHROPIC_BASE_URL=https://127.0.0.1:1/nope ANTHROPIC_AUTH_TOKEN=bogus \
  claude -p 'say PONG' --max-turns 1     # must FAIL
```

**Which preset?** `codesmart` is the default and the cheapest arm in the
[benchmarks](../RESULTS.md) at the highest reward. Use `coding` if you want `skeleton` to
strip function bodies out of large source reads — it needs a `cg_skeleton` build. See
[Choose a preset](choose-a-preset.md).

**`context-guru-proxy` exits with `unknown component "skeleton"`.** The `coding` preset
needs a build with the `cg_skeleton` tag; `make build` does not pass it.

**Pinning the model.** context-guru forwards whatever `model` Claude Code sends. Set
`FORCE_MODEL` on the proxy to override it (useful in eval harnesses).

**Starting the proxy and the agent in one command.**
[`scripts/with-guru.sh`](https://github.com/rossoctl/context-guru/blob/main/scripts/with-guru.sh)
does both:

```sh
scripts/with-guru.sh codesmart -- claude
```

</details>

See also: [Quickstart: proxy](../get-started/quickstart-proxy.md) ·
[Measure savings](measure-savings.md) · [Reversibility & recovery](recover-context.md)
