---
name: status
description: Report whether the local context-guru proxy is running, whether this project is routed through it, and what it has actually saved — reading /stats and explaining the numbers honestly. Use when the user asks about context-guru status, savings, cache hit rate, cost, tokens saved, or whether the proxy is working.
---

# context-guru status

Answer two questions in order, because the second is meaningless if the first is "no":

1. **Is this project actually routed, and is the proxy up?**
2. **What has it saved?**

## 1. Routing and liveness

First get the configured port, because you cannot read it from the environment here —
`CLAUDE_PLUGIN_OPTION_*` reaches hook environments only, so a shell default would silently report on
8787 while the user runs on something else:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" config
```

Use its `option_port=`. **Read the fallback per option, not from `source=`:** that command prints an
`option_<name>=` line only for keys the user actually configured, and reports `source=(none)` only when
nothing at all is set — so somebody with a partial config gets a real `source=` and no `option_port=`
line. Any option the output does not list is unconfigured; use the `plugin.json` default for that one
(port 8787), whatever `source=` says. Then:

```bash
PORT="<port>"
echo "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL:-(unset)}"
curl -fsS --max-time 3 "http://127.0.0.1:${PORT}/healthz" || echo "(no proxy on ${PORT})"
```

Then check where the routing is configured, in precedence order — later files win:

```bash
for f in ~/.claude/settings.json .claude/settings.json .claude/settings.local.json; do
  [ -f "$f" ] && "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" show --file "$f"
done
```

Report the combinations plainly, because they mean different things:

- **`ANTHROPIC_BASE_URL` unset in this session, but present in a settings file** — the routing
  was added after this session started. It applies to the next session. This is the normal
  state right after `/context-guru:install`, and it is not a fault.
- **Set, proxy answering** — working. Go to step 2.
- **Set, nothing answering** — this session's API requests are failing right now. Start it:
  `"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"`, and if that does not work, read
  `${TMPDIR:-/tmp}/context-guru-proxy-${PORT}.log`. If they want out immediately, that is
  `/context-guru:uninstall`.
- **Set to a base URL that is not ours** — say so and stop. Something else owns their routing.

## 2. What it saved

```bash
curl -fsS "http://127.0.0.1:${PORT}/stats"
```

Lead with the **billed token tiers** (`cache_read`, `cache_creation`, input, output). Those come
from the provider's own usage block, so they are the numbers the user can check against their
own bill or usage page. Under the `cache` preset the whole story is tokens moving from the
cache-creation tier to the cache-read tier — creation is billed at a premium, reads at a
discount, so that shift *is* the saving.

Then, if they are non-zero: `requests`, `saved_tokens`, `savings_pct`, and the keep-alive block
(`pings`, `spend_usd`, `wrote_instead_of_read`).

The dashboard shows the same thing over time: `http://127.0.0.1:<port>/dashboard/`.

## Be honest about the numbers

These caveats are not hedging; each one is a way a confident reading would be wrong:

- **`/stats` cost figures are list-price estimates.** On a Pro/Max subscription the saving lands
  in usage limits, not dollars — their bill does not change. Say so rather than quoting a dollar
  figure at a subscriber as if it were money back.
- **A fresh install shows almost nothing, and that is expected.** The cache effect appears on
  the *second and later* turns of a session; the first request of a session is nearly always
  cold — measured, 1,105 of 1,127 session starts.
- **Check whether this is even a git repository, before offering any other explanation.** The
  split works on the environment snapshot Claude Code appends to its system prompt, and outside a
  git repo there is no snapshot to split — `cachesplit` reports `verdict: skipped`, `mutated: 0`,
  and the saving is exactly zero. This is the common case for a casual first trial, and telling
  such a user "the cache warms up on later turns" is true in general and wrong here:

  ```bash
  git rev-parse --is-inside-work-tree 2>/dev/null || echo "NOT a git repo — cachesplit cannot act"
  ```
- **`acted: 0` and `saved_tokens: 0` are not evidence of failure for this preset.** Those count
  components that removed content; `cachesplit` relocates a cache breakpoint and removes nothing.
  The signals that move are `components.cachesplit.verdict` (`moved` vs `skipped`), its `mutated`
  count, and the billed tiers. Lead with the tiers, and do not quote `savings_pct` as the verdict
  on a cache-only pipeline.
- **`wrote_instead_of_read` above zero is a bug signal, not a saving.** It means a keep-alive
  ping created a cache entry instead of refreshing one, which costs money for nothing. Report it
  as a problem.
- **On non-Anthropic backends the `cache` preset does nothing at all** (vLLM, llm-d and similar
  match an implicit longest prefix on their own). Zero saving there is correct behaviour, not a
  failure.
- **`/stats` is process-wide**, not per-project: if they route several projects to one proxy,
  these totals cover all of them.

If the numbers are genuinely flat after real use, say that and offer the next step — usually
`codesmart`, which adds the offloaders — rather than reaching for a favourable reading of a
flat graph.
