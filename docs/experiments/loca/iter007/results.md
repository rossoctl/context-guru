# LOCA iteration 007 — stage 1 of the selection experiment, stopped at checkpoint

**Date:** 2026-08-21
**Config:** `cg_64k_75` (LOCA, 64k context dial, `aws-claude-sonnet-5`), 12 workers
**Arms planned:** `s1-off` (no CG) · `s1-format` (deterministic only) · `s1-coref` (`format`+`coref`)
**Arms run:** `s1-format` (complete, n=15, $113.83) · `s1-coref` (killed at checkpoint, n=14)
**Spend:** ~$215 (arm 2's cost is real but unrecorded — killed before its summary was written)
**Status:** **stopped deliberately at the checkpoint**, per the standing instruction to stop and
re-evaluate rather than run all arms to completion.

## Why it was stopped

Two independent reasons, one a defect of mine and one a property of the benchmark.

### 1. My replay shim corrupted an unknown fraction of requests

The three `400 Bad request` HTML errors in `s1-format` were **not** component failures. Root cause,
confirmed by an isolated two-process test (`echo_srv.py` + shim + `shimtest.py`):

| request framing | body delivered upstream | headers forwarded |
|---|---|---|
| `Content-Length` | 49 / 49 bytes ✅ | correct |
| `Transfer-Encoding: chunked` | **0 / 49 bytes** ❌ | **`chunked` *and* `Content-Length: 0`** |

`repair_shim.py` read the body from `content-length` only, and copied all client headers through
except a small denylist that did not include `transfer-encoding`. So for any chunked request it
forwarded an empty body under two mutually contradictory framing headers. A permissive server
accepts this (the local echo server returned 200); a real gateway answers exactly what we saw —
`Your browser sent an invalid request.` `httpx` selects chunked on its own terms, which is why the
failures looked random and intermittent.

**Fixed:** the shim now decodes chunked bodies properly and strips `transfer-encoding` before
forwarding. Both framings verified to deliver the full body under a single correct `content-length`.

This exonerates `format`, exonerates LOCA, and closes the matching unexplained error in
[iteration 005](../iter005/results.md).

**It also means the discriminating experiment was unnecessary.** The plan was to finish the `off`
arm to decide "LOCA problem or `format` problem." The mechanism lives in the transport, which no
component touches, so the answer was neither, and the `off` arm could not have distinguished them.

Supporting evidence from the data already collected: `s1-coref` — which *also* runs `format` — had
**1** such error against `s1-format`'s **3**, and succeeded on all three tasks that `format` errored
on. A component-caused failure would not behave that way.

### 2. The benchmark cannot power the reward comparison at this budget

Measured, not assumed:

- **Cost:** $7.59 per task per arm.
- **Base solve rate:** `format` solved **2 of 10** clean tasks (20%). Accuracy is **binary** (every
  value is exactly 0.0 or 1.0) — there is no continuous score to recover extra power from.
- **Grouping:** `group_by_seed` defaults to `True` and is **not exposed as a CLI flag** (it is a
  parameter of `run_claude_api` that the Typer wrapper does not surface). This is what collapsed 75
  tasks to 15 runs. Reaching n=75 requires patching LOCA's source — and would cost 5× per arm.
- **Discordance:** 1 discordant pair in 10 clean pairs, in coref's favour. Exact McNemar **p = 1.00**.

Required pairs for a *superiority* claim at the observed ~10% discordance rate:

| discordance rate | if 100% favour one arm | 80% split | 70% split |
|---|---|---|---|
| 10% | 55 | 119 | 279 |
| 20% | 28 | 60 | 140 |
| 30% | 19 | 40 | 93 |

The 100% column is fiction; 80% is optimistic. So a superiority claim costs **$911–$4,200** for a
single two-arm comparison. That is not a good use of the remaining budget, and superiority was never
the claim of interest.

## What the data does establish

**The components act, and coref acts non-trivially.** From CG's own counters (LOCA's
`context_management_events` field is empty in both arms, but that field tracks *LOCA's* native
clear-tool-uses mechanism, not a proxy — it is not evidence about CG):

| arm | component | calls | acted | tokens removed |
|---|---|---|---|---|
| `s1-format` | `format` | 1244 | most | up to 29,399 in one request |
| `s1-coref` | `format` | 1271 | most | up to 56,215 in one request |
| `s1-coref` | `coref` | 1271 | **54 (4.2%)** | **~981,000 total**, incl. single hits of 60,080 and 57,118 |

So coref no-ops on 95.8% of requests and acts rarely but large — consistent with its economic gate,
and the first LOCA run in which coref's action rate is measured rather than inferred.

**Trajectories are short and not cap-limited.** Tool-call counts ran 9–53, far below the ~106 ceiling
observed earlier, and the agent terminated on its own. Task failures are genuine task failures, not
truncation artifacts. This removes a hypothesis but also explains the low headroom: most LOCA tasks
at 64k fail for reasons context management cannot affect.

## The reframe this forces

Superiority on reward was always the wrong primary endpoint. Two endpoints, correctly ordered:

1. **Primary — savings.** Continuous, measured per request, thousands of observations per arm,
   already precise. coref's ~981k tokens over 1271 calls needs no additional n.
2. **Secondary — non-inferiority on reward**, with a pre-declared margin. Ties are *informative*
   here, which is why this is affordable where superiority is not:

| pairs | cost (2 arms) | if 0 harmed | with harm at the observed rate |
|---|---|---|---|
| 20 | $304 | ≤ 14% harmed | ≤ 22% |
| 40 | $607 | ≤ 7% harmed | ≤ 11% |
| 60 | $911 | ≤ 5% harmed | ≤ 8% |
| 100 | $1,518 | ≤ 3% harmed | ≤ 6% |

The honest statement of what stage 1 bought: **n=10 pairs bounds harm at ≤26%** — too wide to be
worth anything. The margin has to be declared *before* the run, and the budget chosen to buy it.

## Addendum — the model knobs, and the band table

Two things checked after the arms were stopped, both of which change the next move.

**The agent is already Sonnet 5.** Run directories read `aws-claude-sonnet-5`, so the 20% base solve
rate *is* Sonnet's. There is no Haiku→Sonnet upgrade available; the only step up is Opus, which
raises the $7.59/task figure. Separately, `extract_llm` is configured `model: {source: config}` —
it inherits the request's model, so it is Sonnet 5 too, and Haiku is the *untried, cheaper* option
there rather than a starting point. Stage 1's arms were `[format, coref]` with no `extract_llm` at
all, so no model knob could have affected these numbers.

**The 32k and 96k bands were never measured.** Both probes failed with
`[Errno 11] write could not complete without blocking` — EAGAIN from my own pipe handling — with
`messages: 0`, before the agent ran. The 128k band's reported zero floor was collected during the
broken-shim window and is equally unestablished. So "64k is the only viable band" rested on one
measured point between a saturated 8k and two rows that are rig artifacts.

Since solve rate falls from 1.0 at 8k to 0.20 at 64k, an intermediate band plausibly has both
pressure and headroom, and **the thin-headroom problem may be an artifact of band choice rather than
a property of LOCA.** Measuring 32k on the fixed rig is the cheapest next step and should come before
buying more pairs at 64k or moving to a larger agent model.

## Checked and dismissed: the broken terminal MCP is not the cause of the 20%

`cli-mcp-server` fails to start in every run here — `AttributeError: 'Server' object has no
attribute 'list_tools'` — and LOCA logs `Failed to list tools from mounted server
'FastMCPProxy-MCP_terminal-*': Connection closed` (30 occurrences in `s1-format`, 29 in `s1-coref`,
92 of 181 terminal mounts in the 32k probe). Given three prior rig artifacts in one day, the obvious
hypothesis was a fourth: tasks failing because a tool was missing.

**It does not hold, and the check is worth recording so it is not repeated.** In the 64k arm the
agent called `canvas_*`, `google_cloud_bigquery_run_query`, `python_execute`, `filesystem_*`,
`email_get_emails`, `google_sheet_get_sheet_data`, `pdf_tools_read_pdf_pages`, `snowflake_write_query`
and `woocommerce_*` — and issued **zero** terminal calls across every task, with **no** step
observation referencing the terminal failure. These tasks do not use the terminal server; its failure
to register is startup noise.

So the 20% base solve rate stands as a real property of the 64k band, not a rig artifact. Recorded as
a caveat worth fixing for tasks that *would* need a shell, not as an explanation of these numbers.

## Carried forward

- Stage-1 numbers are **not** reportable as a reward comparison: contaminated by the shim bug, and
  underpowered regardless. The *component-activity* numbers above are unaffected (they come from CG's
  own counters on requests that reached it) and are reportable.
- **Do not relaunch the same cut.** Repeating an underpowered design with cleaner errors buys a
  cleaner p = 1.00.
- Open, unchanged by this iteration: the **merged** design (co-reference criterion inside
  `extract_llm`'s prompt) is still untested; the deferral claim still needs `summarize` on
  provider-validated traffic; `summarize` + `cachesplit` still untested.
