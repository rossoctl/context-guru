# LOCA-bench — iteration 003 (reward, at last)

**Date:** 2026-08-20/21 · **Goal:** the gate. Every measurement before this one was replay —
tokens removed, components fired — and none of it says whether a cut costs the agent the task.
LOCA-bench's own ReAct agent with its **deterministic GEM scorer** answers that.

**Integration:** LOCA's native Anthropic `/v1/messages` calls point straight at a context-guru
proxy via `LOCA_ANTHROPIC_BASE_URL`, with `LOCA_ANTHROPIC_API_KEY` a dummy — the proxy holds the
real credential. No `forever`, no auth hop. Copied from `forever`'s wiring rather than run inside
it. Per the requester's `CLAUDE.md` the proxy's upstream is the plain litellm gateway and
`ANTHROPIC_CUSTOM_HEADERS` is cleared, so benchmark traffic never transits Context Guru's own
service.

Run: `/tmp/cg-loca/reward.sh <arm> <preset-or-config> <port>` →
`loca run-claude-api -c task-configs/debug.json -m aws/claude-sonnet-5 --max-workers 1`.
Binary: `/tmp/cg-coref/cg-proxy-fold`.

## Result — baseline

| arm | accuracy | success | steps | cg requests | cg saved | cost |
|---|:--:|:--:|--:|--:|--:|--:|
| **`off`** (passthrough) | **1.0000** | 1/1 | 9 | 9 | **0** | $0.4303 |

Matches `forever`'s own iter001 (`raw` = 1.0) — the integration is correct. `saved = 0` with no
component acting is the transparency check: on `off` the proxy is verifiably inert.

Arms against this baseline are in progress; this page records the baseline and the three-stage
diagnosis that produced it.

## What a 1.0 baseline means here (and why it differs from forever's read)

`forever` recorded a saturated `debug` baseline as a **problem** — a task where `raw` scores 1.0
cannot show a memory *lift*. For this work the polarity is reversed: the question is whether
compaction **loses** reward while deferring summarization, so a saturated baseline is the ideal
control for a **regression** test. Any arm below 1.0 is a real loss, with no ceiling effect to hide
behind. Headroom becomes necessary only later, if the goal changes from "does not hurt" to "helps".

## The diagnosis: three silent failures, one honest signal

The first two attempts returned **`accuracy 0.0` with `Success: 1/1`** — an episode that ran cleanly
and scored zero. Worth recording in full, because each layer failed differently and only one
number told the truth.

1. **`anthropic` SDK not installed.** Loud, immediate, easy.
2. **A macOS `.venv` shipped inside `mcp_convert/`**, its binaries stripped in transit. Rebuilding
   it changed nothing — which was itself useful: it *eliminated* the venv rather than confirming it.
3. **The real cause: `python` on `PATH`.** LOCA launches its Python MCP servers with the bare
   command `python` (`gem/tools/mcp_server/config_loader.py`). Invoking `.venv/bin/loca` directly
   does **not** put `.venv/bin` on `PATH`, so those subprocesses got `/usr/bin/python` — 3.9 here,
   without `mcp`/`fastmcp`, and below LOCA's own 3.10 floor. The servers died **silently**, their
   tools never registered, and the agent — reasonably — reported the workspace as empty and wrote
   1 CSV row instead of 4.

**The tell.** The agent's tool list contained *only* `filesystem_*` and `memory_*`. Those two are
exactly the **npm**-backed servers; `canvas`, `python_execute` and `claim_done` are the **Python**
ones. A partition that clean points at one shared cause, not three coincidences.

**The signal was `tool_success_counter: 0`** — 8 tool calls, none successful, visible only in
per-task `eval.json` and not in the run summary, while everything at the top level (`Success: 1/1`,
a plausible transcript, a real cost) looked healthy.

!!! warning "But that counter is NOT a general health signal — an earlier version of this page
    over-claimed it"
    Runs that *pass* emit a different feedback shape entirely
    (`'evaluation': 'passed', 'parsed_action': 'claim_done_claim_done'`) with no such counter, and a
    later 128k run showed `tool_success_counter: 0` when the tools were working perfectly well and
    the real cause was the tool-use **budget**. So the counter appears on failure and does not
    distinguish *why*. In the case above the tools genuinely were broken, but the counter was
    incidental rather than diagnostic. The durable lesson is narrower: **read per-task `eval.json`,
    not the summary** — and confirm a cause before naming one.

!!! danger "Third instance of the same meta-pattern in this project"
    A broken tool environment produces a plausible transcript and a real-looking score:

    - Harbor's Claude Code segfault surfaced as `NonZeroAgentExitCodeError` → reads as `reward=0`
      ([REPRODUCE.md](../../../results/REPRODUCE.md))
    - a stale proxy binary nulled iter002's fold arm, caught only because gate counters were
      byte-identical to the previous arm
    - this

    In all three the failure was **upstream of the model** and the score was **numerically valid**.
    The lesson is procedural: before believing any benchmark number, check that the tools worked —
    `tool_success_counter`, gate counters, a transparency assertion on the `off` arm.

## The 128k band: three more layers, two of them mine

Escalating to a band where compaction would actually engage produced three further failures, and
the sequence is the useful part.

1. **`[Errno 11] write could not complete without blocking` (EAGAIN) on every band above 8K.**
   Chased through a full band bisect (32k/64k/96k/128k all failed; a *small* task at 128k failed
   too, proving it was the band not the task) and attributed to LOCA's MCP stdio transport.
   **It was my own runner:** `| tail -25` on LOCA's stdout made it a pipe, and Rich writing a
   band-scaled environment description overflowed it. `debug` printed little and survived. The
   message named the writer — *stdout* — and I read "write" and reached for the transport twice
   before checking my own harness. **Four runs and a bisect spent on a self-inflicted bug.**

2. **HTTP 400 — 42 orphaned `tool_use` ids.** With the pipe gone, the real blocker appeared: LOCA's
   native trimmer drops messages and orphans `tool_use`/`tool_result` pairs. This is predicted
   verbatim in [the proposal §8](../../../proposals/coref-compaction.md#8-consequences-for-benchmark-selection),
   *including the instruction to port `repair_tool_pairing()` from `forever` rather than rediscover
   it*. I rediscovered it first and read my own note second. The fix is now a rig-side shim
   (`/tmp/cg-loca/repair_shim.py`) that lifts the function **verbatim** so the two rigs cannot
   drift, sits **before** cg-proxy so compaction is measured on well-formed traffic, and counts
   repairs so the rate is visible. `coref` structurally cannot cause this class of bug — it
   rewrites a tool message's text in place and never removes a message.

3. **`--max-tool-uses 100` is too small for this band.** With the shim in place the run completed
   (`Success: 1/1`, no 400) but scored 0.0 with `tool_use_counter: 105` — it hit the cap. At 128k
   the environment holds **49 quizzes and 30 assignments** to enumerate; the agent wrote 1 row
   before running out of calls. `trim_events: 0` rules out both context rot and the shim. Cost was
   **$5.23** for the one task, 12× the 8K band.

**What the 128k band did establish:** the shim fired **354 repairs across 42 requests**, so pair
orphaning is constant there — and the band produces genuine scale (105 tool calls, 42 steps) rather
than the saturated triviality of `debug`. It is the right band; it needs a real tool budget.

## What this does NOT prove

- **n = 1 task, 1 trial.** `debug` is a single Canvas task. Nothing here is a distribution.
- **8K band, so almost no context pressure.** The point of the higher bands
  (`final_64k/128k/256k`) is that compaction has something to do; at `debug` it may not fire at all,
  in which case reward parity is trivially satisfied and says little.
- **Cost is not "cheap" as the dive suggested.** $0.43 for one 8K task on sonnet, ~4× `forever`'s
  own $0.10 estimate and ~1.4× their measured $0.30. Budget the higher bands accordingly.

## Next levers

1. Arms at `debug` — confirm no reward regression and observe whether components fire at all.
2. **Escalate the dial** to a band where compaction genuinely engages, which is the only place
   deferral can be shown to be reward-neutral rather than vacuous.
3. `--use-clear-tool-uses`, LOCA's native context editing, as the baseline to beat.
4. A second benchmark on the long-horizon axis (UltraHorizon).

## Artifacts

`/tmp/cg-loca/out-baseline{,2,3}.log`, `/tmp/cg-loca/st-baseline*.json`, and LOCA's own run dirs
`/tmp/cg-loca/outputs/inf_claude_api_debug_*/` (per-task `eval.json`, `trajectory.json`) on the
eval box.
