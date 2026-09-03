# Iteration 021 — results

**Status: RUNNING.** This file was created before launch to record the frozen inputs, which is the
control iterations 014, 016 and 018 lacked and the reason they cannot be compared to each other.

## Frozen inputs

| | |
|---|---|
| binary | `cg-proxy-v19`, 37,080,187 bytes |
| SHA-256 (first 32) | `ecc02f28417fe8d5edbcfaf3cc13505b` |
| code commit | `fcf78cd` — everything since is documentation only |
| task config | `task-configs/final_128k_set_config.json` — 15 tasks × 5 seeds (`state0..state4`) |
| arm A config | `deploy/harbor/cfg-iter021-A-baseline.yaml` |
| arm B config | `deploy/harbor/cfg-iter021-B-merged.yaml` |
| environment | `INJECT_EXPAND=always`, `CACHE_MODE=on`, 128k declared window, LOCA clearing at 128k, `--max-workers 8` |
| adjudication model | the request's own model (`aws/claude-sonnet-5`) |
| summarize model | cheap client (`aws/claude-haiku-4-5`) |

**One binary serves both arms.** Any code change before both arms finish invalidates both, not one.

## Pipelines — one component apart

```
A  [format, toon, dedup, failed_run, cmdfilter,              extract, cachesplit, summarize]
B  [format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit, summarize]
```

## Results

**A null on the primary endpoint.** Merged adds **+1.00 solve on 75 pairs, p = 1.0000, at +2% total
cost.** The pre-registered reading for this outcome, written before the run: *"clustered null, both
directions flat → no detectable marginal value at this n and this cost → close merged; keep the
deterministic pipeline; report the ceiling honestly."*

| | arm A (baseline) | arm B (+ merged) |
|---|---|---|
| **ITT solves (all 75 pairs, errored = 0)** | **14.00 / 75** | **15.00 / 75** |
| **clustered p, 15 clusters (GOVERNING)** | — | **1.0000** — 2 better, 2 worse, 11 tied |
| per-pair p, 75 pairs (sensitivity) | — | 1.0000 — 7 gained, 6 harmed, 62 unchanged |
| harm, 95% Clopper-Pearson upper bound | — | 15.2% — **does not block** |
| errors | **17** | **10** |
| LOCA cost | $217.38 | $187.22 |
| CG model spend | $6.68 | **$41.44** |
| **total cost** | **$224.06** | **$228.66 (+2%)** |
| `summarize` fired on | 71% of requests | **56%** |
| requests | 3,446 | 2,466 |
| unique tokens removed by `extract_llm` | — | 6,545,956 |
| expand restored / unresolved | 361 / 1 | 717 / 0 |

### Per-task, mean accuracy over 5 seeds

| task | A | B | B−A |
|---|---|---|---|
| AcademicWarningS2LEnv | 1.00 | 0.40 | **−0.60** |
| NhlB2bAnalysisS2LEnv | 0.20 | 0.00 | **−0.20** |
| PayableInvoiceCheckerS2LEnv | 0.60 | 1.00 | **+0.40** |
| MachineOperatingS2LEnv | 0.00 | 0.60 | **+0.60** |
| ExcelMarketResearchS2LEnv | 0.60 | 0.60 | 0 |
| SetConfCrDdlS2LEnv | 0.40 | 0.40 | 0 |
| *the other nine tasks* | 0.00 | 0.00 | 0 |

**Eleven of fifteen tasks score zero in BOTH arms.** That is the dominant fact about this benchmark's
power: the comparison rests on four tasks, two moving each way. No configuration change could have
produced a detectable difference here without a large effect.

## What merged did deliver, all secondary

* **7 fewer errored runs** (17 → 10). Both arms hit `"prompt is too long"` on oversized tool outputs;
  arm B hit it less. **But this is NOT clean evidence that model compaction prevents runaways** — see
  the `collapse` limitation below.
* **15 points less summarization** (71% → 56%). Real, and much smaller than the 4× that comparing
  against iteration 020 had suggested — which is why iteration 020 was never a valid comparator.
* **28% fewer requests** (3,446 → 2,466), consistent with fewer runaway sessions burning turns.
* **6.5M unique tokens removed**, with recovery working: 717 restores, **0 unresolved**.

**Cost prediction, stated in advance, was right:** "parity to ~20% worse" → **+2%**. LOCA spend fell
$30 and CG spend rose $35.

## Limits — none of which rescue the result

1. **Coverage 61%.** Arm B answered verdicts for 2,114 of 3,461 offered candidates, and ran below its
   own pre-registered 50% checkpoint for the first third of the run (amendment 2). Its numbers are a
   **floor**: the honest null statement is *"merged at 61% coverage adds nothing detectable"*, not
   *"merged adds nothing"*.
2. **Neither arm had `collapse` or `mask`**, so neither is any shipped preset. And `collapse` would
   probably not have helped anyway: it skips outputs of ≤40 lines (`headLines+tailLines`), and a database
   or API result serialised as JSON is often ONE line. A single-line multi-megabyte payload falls through
   `collapse` as well as through everything else — which is a product gap with no owner, not a
   configuration mistake.
3. **`summarize` ran alongside in-place offloaders**, which `config.go` explicitly advises against
   ("run it alone so no other component's in-place edits race apply's rebuild"). Its own preset is
   `{summarize}` alone.
4. **`merged_unparseable` 146** — replies that were neither a tool call nor parseable JSON, so those
   calls changed nothing.

## Conclusion

On the question asked — *what does merged add on top of the shipped deterministic pipeline?* — the
answer at this n, this coverage and this cost is **nothing detectable in reward**, with modest
operational gains (fewer errors, less summarization, fewer requests) at cost parity.

Two things would have to change before that verdict could be called final rather than
underpowered: **coverage** (61% → near 100%, and the untested lead is a SMALLER batch, since a probe
answered 6 of 6 on six candidates against ~61% on twelve), and **a benchmark with room to move** — 11
of 15 tasks scoring zero in both arms leaves almost nothing to detect.
