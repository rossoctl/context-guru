---
name: status
description: Report whether the local context-guru proxy is running, whether this project is routed through it, and what it has actually saved — reading /stats and explaining the numbers honestly. Use when the user asks about context-guru status, savings, cache hit rate, cost, tokens saved, or whether the proxy is working. Accepts --stats to print the raw /stats JSON verbatim instead of the interpreted report.

---

# context-guru status

**If invoked with `--stats`**, skip everything below: resolve the port the same way step 1 does
(`settings.py port show`, then `settings.py config`, then the `plugin.json` default), run

```bash
curl -fsS "http://127.0.0.1:${PORT}/stats" | jq .
```

and print its output verbatim — no narration, no interpretation, not even the liveness check. This
is for someone who wants the JSON itself, not a reading of it; a failed curl (proxy not up) should
still be shown as-is rather than translated into prose. If `jq` is not installed, fall back to the
bare `curl` and print the unformatted body rather than failing.

**Otherwise**, answer two questions in order, because the second is meaningless if the first is "no":

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
line. Any option the output does not list is unconfigured; use the `plugin.json` default for that one,
whatever `source=` says.

**The port is the exception, and 8787 is the wrong guess for it.** Ports are allocated per project —
one project per port, because two projects sharing one proxy made every session start kill and
restart it, wiping the in-memory store each time — so the port this project runs on is usually
neither 8787 nor anything the user ever typed. It is recorded, so ask for it instead of defaulting
it:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port show
```

`result=ok port=<n>` is this project's port, and it wins over `option_port=` when the two disagree —
that is the same order the allocator uses, and it is what the proxy was actually started on.
`port=(none)` means there is no record: only then fall back to `option_port=`, and to 8787 after
that. Then:

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
from the provider's own usage block, so they are the numbers the user can check against their own
bill or usage page. Under the default preset (`off`), the whole story is the keep-alive block: a
ping moves tokens from the premium cache-creation tier to the discounted cache-read tier by
refreshing the cache before it expires, and that shift *is* the saving.

Then, if they are non-zero: `requests`, `saved_tokens`, `savings_pct`, and the keep-alive block
(`pings`, `spend_usd`, `wrote_instead_of_read`) — that block is the in-memory ping ledger, always
present, cheap, and reset on restart.

If `--dashboard` is enabled, `/stats` also carries a `savings` object with up to three scopes —
`current` (the session that most recently sent a real request), `live` (every session the keeper
still considers active, summed), and `all` (process lifetime). Each scope reports the SAME
reconciled figure the dashboard does — `keepalive_net_usd` (did the pings pay for themselves) and
`total_saved_usd` (every mechanism combined) — computed once in `dash` and shared, so `/stats` and
the dashboard can never disagree. A scope is *absent*, not zeroed, when there is nothing to report
yet (fresh proxy: no `current`; no `--dashboard`: no `savings` at all).

The dashboard shows the same numbers over time, plus a UI: `http://127.0.0.1:<port>/dashboard/`.

## Be honest about the numbers

These caveats are not hedging; each one is a way a confident reading would be wrong:

- **`/stats` cost figures are list-price estimates.** On a Pro/Max subscription the saving lands
  in usage limits, not dollars — their bill does not change. Say so rather than quoting a dollar
  figure at a subscriber as if it were money back.
- **A fresh install shows almost nothing, and that is expected.** The cache effect appears on
  the *second and later* turns of a session; the first request of a session is nearly always
  cold — measured, 1,105 of 1,127 session starts.
### Is a configuration change still pending?

`start-proxy.sh` records what the running proxy was started with, in
`$CONTEXT_GURU_STATE/proxy-<port>.fingerprint`. Compare it against what the options ask for now:

```
preset=<x> strategy=<y> idle=<z> upstream=<u> port=<p> bin=<version>
```

If they differ, the user changed something — most often `/plugin configure`, or a background proxy
upgrade landed a new `bin=` — and **the running proxy predates it**. Say which field differs and
that a new session applies it; the SessionStart hook stops and restarts the proxy when it sees the
difference. Do not restart it from here: this skill runs inside a session that is routed through
that proxy.

Beside it, `$CONTEXT_GURU_STATE/proxy-<port>.owner` names the project that proxy belongs to. It is
only interesting when it names a project that is **not** this one: that means this project is routed
at somebody else's proxy, running their preset and their strategy, and every number below is theirs.
Report it as the finding it is rather than reading the stats — and do not restart anything, because
`start-proxy.sh` deliberately will not take a proxy from the project that owns it. The fix is a port
of this project's own, which `/context-guru:install` allocates.

If the fingerprint is **absent** while a proxy is running, it was started before this was recorded (or
by something else). That is not an error and not a pending change — say so rather than guessing, and
note that the next cold start records it.

This is where a pending switch is reported, deliberately, and not from the `UserPromptSubmit` hook: a
python invocation on the critical path of every prompt is a poor price for news that the next session
delivers by itself.

### Is a newer proxy release pending?

`"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check show` — **read-only, never fetches.** It
reports what the last SessionStart check (at most once every 5 minutes, run synchronously by
whichever session happens to be due — so a release CAN be announced in the very session that
discovers it, not only the next one) already found:
`answer=`/`skipped=`/`notified=`/`latest=`/`installed=`.

Read `installed=` from THIS output, not from `skipped=` — conflating the two is a real defect this
surface used to have. And keep `skipped=` and `notified=` apart: `skipped=<tag>` means the user
EXPLICITLY declined that tag; `notified=<tag>` only means a hook once printed a note about it,
which may never have reached the user at all (a SessionStart note competes with whatever they
actually asked that session). If `notified=` names a tag but `skipped=` does not, say it was
surfaced but never actually answered — do not report it as declined.

An empty or `unknown` `latest=` means no check has succeeded yet, not that the proxy is current. If
`latest=` names something newer than `installed=` and it is not the `skipped=` tag, point at
`/context-guru:update` — which does its own live check before reporting, rather than trusting this
cached record — instead of re-explaining any of this here.

- **Check the preset before explaining a zero pipeline saving at all.** The default preset is
  `off`, which runs no components, so `acted: 0` and `saved_tokens: 0` are the expected state, not
  a symptom. Look at the keep-alive block instead — that is what actually runs and spends by
  default.
- **`wrote_instead_of_read` above zero is a bug signal, not a saving.** It means a keep-alive
  ping created a cache entry instead of refreshing one, which costs money for nothing. Report it
  as a problem.
- **On non-Anthropic backends keep-alive still spends, but a chosen pipeline preset may not
  save anything** (vLLM, llm-d and similar match an implicit longest prefix on their own). Zero
  pipeline saving there is correct behaviour, not a failure.
- **`/stats` is process-wide**, not per-project: if they route several projects to one proxy,
  these totals cover all of them.

If the numbers are genuinely flat after real use, say that and offer the next step — usually
`codesmart`, which adds the offloaders — rather than reaching for a favourable reading of a
flat graph.
