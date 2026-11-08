# Tool-deferral accuracy harness (#871)

Run it before a release that ships the `ultrahigh` preset. It costs about **$25 to $28**.
It uses **synthetic data only**. It reads no one's real history.

## What it measures

The right-tool count on **40 tasks** (20 direct, 20 vague). A run is right when the model calls the wanted tool.

| Setup | What it is |
|---|---|
| s1 | No deferral. `xhigh` preset, `tools` sent in full. |
| s2 | Claude Code's own search. `xhigh`, no proxy deferral. |
| s3 | `ultrahigh` with `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`. This removes `ToolSearch`, so it tests the **server-side** search path. |
| s3b | `ultrahigh` with Claude Code's `ToolSearch` (the #900 path). **This is the one that counts.** |

## Settings

| Variable | Meaning |
|---|---|
| `TD871_GATEWAY_URL` | Base URL of an Anthropic-compatible gateway. The tap forwards to it. No Context Guru in the path. |
| `TD871_API_KEY` | Key for that gateway. Name only; never print the value. |
| `TD871_ROOT` | Work folder. Default: this folder. |
| `TD871_PROXY` | Proxy binary. Default: `$TD871_ROOT/bin/proxy`. |

Each `claude` process gets `ANTHROPIC_BASE_URL` set to the test proxy and `ANTHROPIC_CUSTOM_HEADERS=` empty.
The test proxy chains to `tap.py` on 127.0.0.1:18970. `tap.py` forwards to `TD871_GATEWAY_URL`.
Free ports needed: 18970, 18972, 18981, 18990-18993, 19000-19003.
`runcc-raw.sh` is the no-proxy arm. The runner adds an `/anthropic` prefix that only the proxy strips; `runcc-raw.sh` drops it.

## Commands, in order

Run them from this folder.

1. `./setup.sh`. It writes `mcp.json`, `settings.json` and the git template `tmpl/`.
2. `./build.sh <checkout>`. It needs Go. It writes `bin/proxy`. Note the commit in your report.
3. **Check the budget.** Send one tiny request to the gateway and expect HTTP 200:
   ```
   curl -s "$TD871_GATEWAY_URL/v1/messages" -H "content-type: application/json" \
     -H "anthropic-version: 2023-06-01" -H "authorization: Bearer $TD871_API_KEY" \
     -d '{"model":"claude-haiku-4-5-20251001","max_tokens":5,"messages":[{"role":"user","content":"hi '"$(uuidgen)"'"}]}'
   ```
4. **Start the tap and the proxy.**
   ```
   mkdir -p logs
   nohup python3 -I tap.py 18970 > logs/tap.out 2>&1 & echo $! > tap.pid
   ./startproxy.sh px1 18981 xhigh -        # s1 and s2
   ```
   The driver starts and stops its own `ultrahigh` proxies for s3 and s3b.
5. **Seed the history:** `./seed.sh` (about $0.50). See below.
6. **Fresh logs.** `: > logs/tap.jsonl; : > logs/runs.jsonl; : > logs/hook.jsonl; rm -f logs/stats.jsonl`
   To save request bodies, `touch capture`. Delete `cap/*` after.
7. **Run the driver, per model.** Keep each call under 10 minutes. The driver resumes: run the same line again to finish the rest.
   ```
   NW1=3 NW2=3 NW3=3 python3 driver.py claude-sonnet-5-5 3 all s1,s2,s3,s3b 560
   NW1=3 NW3=3       python3 driver.py claude-opus-5-5   2 all s1,s3b,s3 560
   ```
   Usage: `driver.py <model> <reps> <task ids|all> <setups> <seconds budget>`. `NWn` is the number of parallel workers per setup.
   The driver warms every s3 and s3b proxy (`warm.sh`). For s3b it also runs one throwaway `claude` call, so the client probe passes
   before a thread starts. Without it, s3b would test the wrong path.
8. **Analyze.** `python3 analyze.py` (`-v` for one line per run, or a text filter as the first argument). It writes `logs/rows.json`.
9. **Clean up.** `./stopproxy.sh px1; kill $(cat tap.pid); rm -f capture tap.pid`, then delete `cap/*`. Captured bodies hold prompts.

## Reading the score

- **Pass:** for each model, s3b's right-tool count is **at least the s1 count minus 1 task per rep**, and no client sees a 4xx.
  - Sonnet, 3 reps: s3b at least s1 - 3. Opus, 2 reps: s3b at least s1 - 2. Judge s3b first. Report s3 too.
- **Baseline, 2026-11-04, main 44f8318d** (s1 / s3 / s3b): Opus 5.5 **73 / 72 / 74** of 80; Sonnet 5.5 **107 / 109 / 117** of 120.
- **Also check:** `client_search_disabled` is 0; `retries_400` is 0; the `4xx` column is 0.
- **Duplicate response ids** that `analyze.py` prints come from the gateway replaying Haiku background calls
  (the title and summary calls). Check that none is a test run before you fail a run.
- Per-proxy counters come from `/stats` (`tool_defer`). The driver saves them in `logs/stats.jsonl` before each proxy stops.

## Known traps

- `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` **removes `ToolSearch`**. Do not set it for s3b.
- The driver **skips runs that already have a row** in `logs/runs.jsonl`. Use fresh logs for a new run.
- The gateway **replays byte-identical requests**. Every prompt carries a nonce. Do not remove it.
- The **tap (18970) must be running**. Without it every proxy fails upstream.
- The s3 ledger and `wrong_guesses` add up per-restart counters. Treat s3 numbers as an estimate.
- `client_loads` stays **0**: Claude Code's search already indexes the tools the proxy defers.
- Stop at the spend line you agreed.

## The seeded history (`snap/dash.db`)

`seed.sh` builds it from scratch. The data is synthetic.

1. It starts an `ultrahigh` proxy with a fresh dashboard DB at `snap/dash.db` (port 18972, upstream = the tap).
2. It runs 4 tiny real sessions: 2 Sonnet 5.5 and 2 Opus 5.5. The prompt is "Run ls with the Bash tool, then reply with the single word done" plus a nonce.
   They run with `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`.
3. It stops the proxy.

Result: `tool_defer_history` holds **4 sessions** (empty tenant). `tool_defer_probe` holds 2 passing rows (Opus and Sonnet, route `/v1/messages`).
Every session used only Bash, so every other tool counts as unused. A new thread then defers **18 tools**:
CronCreate, DesignSync, EnterWorktree, ExitWorktree, ListAgents, Monitor, NotebookEdit, ReportFindings, ScheduleWakeup, SendMessage, WebFetch, Workflow,
the 3 `mcp__probe__*` and the 3 `mcp__ops__*` tools.
The driver copies `snap/dash.db` into a fresh DB before every s3 and s3b run.
Rebuild the seed with the current binary: an older seed may not match a newer schema. Cost: about $0.50.

## Files

`driver.py` runs tasks. `analyze.py` scores. `tap.py` is the logging forwarder. `hook.py` records tool calls and denies side effects.
`runcc.sh` / `runcc-raw.sh` start one `claude`. `startproxy.sh` / `stopproxy.sh` manage a test proxy. `warm.sh` warms a proxy.
`mcp_probe.py` / `mcp_ops.py` are fake MCP servers. `tasks.tsv` holds the 40 tasks. `show.py` prints the tap log.
`setup.sh`, `build.sh`, `seed.sh` are the helpers.
