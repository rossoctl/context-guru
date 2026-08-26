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

To be filled in when both arms complete. Per the pre-registration, the **task-clustered** paired
Wilcoxon over 15 clusters governs; the 75-pair test is a sensitivity check; a harm upper bound above 25%
blocks any positive claim; and `Overall Success` from the LOCA output is **not** the solve count.
