# Terminal-Bench 2.0: per-arm detail

These are the per-arm result breakdowns for the Terminal-Bench 2.0 run: baseline, context-guru, headroom, and rtk.

## Full results — baseline (Terminal-Bench 2.0, 89 tasks)

Baseline arm: **no compaction** — the `claude-code` agent on `aws/claude-sonnet-5`, run LIVE through the harness against Terminal-Bench 2.0's 89 tasks. Routing goes through the context-guru `off` transparent passthrough proxy (identical plumbing to the compaction arms; zero content change), so this is the like-for-like reference the framework arms are measured against. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's own token tiers — the same model as the SWE-bench study. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| attempted | solved | solve rate | completed | timed out | total billed cost | mean steps* | cache-hit |
|--:|--:|--:|--:|--:|--:|--:|--:|
| 89 | 56 | **62.9%** | 82 | 7 | $100.81 | 31.5 | 98.2% |

\* mean steps over the 82 completed tasks (timed-out runs are truncated). Solve rate over **completed-only** tasks: **56/82 = 68.3%**.

**Time-budget policy.** Wall-clock budget = the task-authored timeout × a multiplier. Most tasks ran at **1.5×**; the long-horizon tasks that first timed out were retried at low concurrency and, if still short, given an extended **4×** budget (up to ~4 h) to measure capability rather than a latency-truncated result. 3 tasks solved only under the 4× budget (counted as solved here); the 7 below exhausted even 4×.

### Analysis — where the agent is strong / weak

- **Terminal-Bench 2.0 is a much harder, longer-horizon benchmark than SWE-bench Verified.** The
  baseline solves **62.9%** of tasks vs 86% on SWE-bench, at **~$1.13/task** (vs $0.64) and **~1.7M
  prompt tokens/task** — TB tasks are open-ended terminal goals (build/compile/train/exploit), not a
  localized patch, so the agent runs longer and reads far more.
- **Difficulty is the dominant axis.** Easy/medium solve at **71–75%**; `hard` drops to **47%** and
  costs **3.4× more per task** ($2.04 vs $0.61). Every one of the 7 unrecoverable timeouts is a
  hard/long task.
- **Category tells the same story from the other side.** The agent is reliable on bounded,
  verifiable goals — `debugging` (100%), `system-administration` (78%), `security` (75%),
  `data-processing`/`model-training` (75%) — and weak on sprawling build/implement tasks:
  `software-engineering` (38%, the largest bucket at 26 tasks) and `video-processing` (0%).
- **It is still a ~98%-cached agent** (98.2% cache-hit), so — exactly as on SWE-bench —
  **cache-read is the single biggest cost term (43% of the bill)**. That is the lever the compaction
  arms must pull; a layer that shrinks the cached context should move TB cost the same way it moved
  SWE cost.
- **Latency, not just capability, caps the ceiling.** The gateway's ~26 s/request (5–10× a normal
  endpoint) means long-horizon tasks can run out of wall-clock before finishing. 3 tasks that first
  timed out **solved once given a 4× budget** — so **62.9% is a floor**, and a compaction arm that
  reduces round-trips could recover more of the remaining 7. The timeout count is therefore itself a
  comparison metric, not just noise.

#### Token & cost accounting (cache-aware, all 89 tasks)

| tier | tokens | $/M | billed |
|---|--:|--:|--:|
| cache-read (input) | 215,971,427 | 0.20 | $43.19 |
| cache-write (input) | 4,011,068 | 2.50 | $10.03 |
| fresh (input) | 58,893 | 2.00 | $0.12 |
| completion (output) | 4,746,887 | 10.00 | $47.47 |
| **total** | | | **$100.81** |

Cache-read is **43%** of the bill at a **98.2%** cache-hit rate — as on SWE-bench, a heavily-cached agent, so the lever a compaction layer must pull is cache-read tokens.

### Timeouts (7 long-horizon tasks)

These tasks still hit the wall-clock budget under the **extended 4×** timeout (up to ~4 h each) and scored **reward 0** — counted as failures in the solve rate above. A large part of the cause is **gateway latency, not only agent capability**: Terminal-Bench's timeouts assume a fast endpoint (~2–5 s/request), but this IBM LiteLLM gateway runs **~26 s/request** (5–10× slower), so long-horizon tasks that need many round-trips run out of clock (concurrency is *not* the cause — latency was flat ~23–30 s/req from n=1 to n=24). They are all `hard`/long software-engineering and compute tasks (path-tracing, a MIPS Doom port, a metacircular evaluator, COBOL modernization, GPT-2 code-golf, CIFAR training). A compaction arm that cuts round-trips could bring some under budget, so the timeout count is itself a comparison metric.

| task | difficulty | category | steps before timeout | partial billed | budget (4×) |
|---|---|---|--:|--:|--:|
| caffe-cifar-10 | medium | machine-learning | 12 | $0.24 | 80 min |
| cobol-modernization | easy | software-engineering | 133 | $5.11 | 60 min |
| gpt2-codegolf | hard | software-engineering | 43 | $1.48 | 60 min |
| make-doom-for-mips | hard | software-engineering | 160 | $6.39 | 60 min |
| path-tracing-reverse | hard | software-engineering | 170 | $9.62 | 120 min |
| schemelike-metacircular-eval | medium | software-engineering | 74 | $4.37 | 160 min |
| write-compressor | hard | software-engineering | 6 | $0.36 | 60 min |

### By difficulty (all 89 tasks; timeouts = failures)

| difficulty | tasks | solved | rate | timed out | mean $/task |
|---|--:|--:|--:|--:|--:|
| easy | 4 | 3 | 75% | 1 | $1.561 |
| medium | 55 | 39 | 71% | 2 | $0.606 |
| hard | 30 | 14 | 47% | 4 | $2.040 |

### By category (all 89 tasks)

| category | tasks | solved | rate | mean $/task | mean steps* |
|---|--:|--:|--:|--:|--:|
| data-querying | 1 | 1 | 100% | $1.151 | 17.0 |
| debugging | 5 | 5 | 100% | $0.878 | 40.0 |
| games | 1 | 1 | 100% | $0.570 | 36.0 |
| personal-assistant | 1 | 1 | 100% | $0.329 | 8.0 |
| system-administration | 9 | 7 | 78% | $0.903 | 46.3 |
| data-processing | 4 | 3 | 75% | $0.309 | 15.0 |
| mathematics | 4 | 3 | 75% | $1.396 | 36.0 |
| model-training | 4 | 3 | 75% | $0.553 | 24.8 |
| security | 8 | 6 | 75% | $0.334 | 15.4 |
| machine-learning | 3 | 2 | 67% | $1.248 | 34.5 |
| data-science | 8 | 5 | 62% | $0.892 | 30.5 |
| file-operations | 5 | 3 | 60% | $0.577 | 26.0 |
| scientific-computing | 8 | 4 | 50% | $0.678 | 24.1 |
| software-engineering | 26 | 12 | 46% | $2.043 | 37.9 |
| optimization | 1 | 0 | 0% | $0.145 | 8.0 |
| video-processing | 1 | 0 | 0% | $2.094 | 80.0 |

### Per-task (all 89)

| task | difficulty | category | outcome | steps | cache_read | cache_write | billed | wall |
|---|---|---|:--:|--:|--:|--:|--:|--:|
| adaptive-rejection-sampler | medium | scientific-computing | ❌ failed | 11 | 462,474 | 17,913 | $0.351 | 6.7 min |
| bn-fit-modify | hard | scientific-computing | ✅ solved | 19 | 806,095 | 12,961 | $0.304 | 6.0 min |
| break-filter-js-from-html | medium | security | ✅ solved | 13 | 524,418 | 6,145 | $0.236 | 11.0 min |
| build-cython-ext | medium | debugging | ✅ solved | 75 | 5,004,079 | 52,076 | $1.342 | 17.8 min |
| build-pmars | medium | software-engineering | ❌ failed | 32 | 1,614,846 | 25,937 | $0.478 | 6.2 min |
| build-pov-ray | medium | software-engineering | ❌ failed | 46 | 2,311,209 | 22,883 | $0.597 | 10.9 min |
| caffe-cifar-10 | medium | machine-learning | ⏱ timeout | 12 | 530,037 | 22,399 | $0.245 | 5.8 min |
| cancel-async-tasks | hard | software-engineering | ✅ solved | 9 | 334,567 | 4,618 | $0.224 | 3.3 min |
| chess-best-move | medium | games | ✅ solved | 36 | 1,676,787 | 16,013 | $0.570 | 8.8 min |
| circuit-fibsqrt | hard | software-engineering | ✅ solved | 50 | 3,331,130 | 69,283 | $3.849 | 109.3 min |
| cobol-modernization | easy | software-engineering | ⏱ timeout | 133 | 9,627,750 | 62,902 | $5.107 | 60.0 min |
| code-from-image | medium | software-engineering | ✅ solved | 5 | 165,696 | 4,230 | $0.049 | 0.4 min |
| compile-compcert | medium | system-administration | ✅ solved | 63 | 3,765,545 | 152,729 | $1.294 | 51.6 min |
| configure-git-webserver | hard | system-administration | ✅ solved | 26 | 1,145,910 | 12,625 | $0.356 | 4.7 min |
| constraints-scheduling | medium | personal-assistant | ✅ solved | 8 | 309,526 | 9,879 | $0.329 | 4.7 min |
| count-dataset-tokens | medium | model-training | ✅ solved | 10 | 410,163 | 10,860 | $0.140 | 7.4 min |
| crack-7z-hash | medium | security | ✅ solved | 21 | 904,687 | 10,512 | $0.232 | 9.4 min |
| custom-memory-heap-crash | medium | debugging | ✅ solved | 37 | 2,605,935 | 55,794 | $1.394 | 20.1 min |
| db-wal-recovery | medium | file-operations | ❌ failed | 34 | 1,645,855 | 26,864 | $1.083 | 20.2 min |
| distribution-search | medium | machine-learning | ✅ solved | 11 | 439,452 | 9,718 | $0.291 | 4.2 min |
| dna-assembly | hard | scientific-computing | ❌ failed | 28 | 1,443,693 | 25,382 | $0.689 | 13.9 min |
| dna-insert | medium | scientific-computing | ❌ failed | 31 | 1,617,092 | 26,810 | $0.854 | 13.5 min |
| extract-elf | medium | file-operations | ✅ solved | 12 | 488,419 | 9,549 | $0.272 | 4.2 min |
| extract-moves-from-video | hard | file-operations | ❌ failed | 2 | 38,801 | 2,561 | $0.017 | 0.3 min |
| feal-differential-cryptanalysis | hard | mathematics | ✅ solved | 25 | 1,158,755 | 16,179 | $1.430 | 28.9 min |
| feal-linear-cryptanalysis | hard | mathematics | ✅ solved | 54 | 3,170,572 | 140,161 | $2.436 | 69.2 min |
| filter-js-from-html | medium | security | ❌ failed | 12 | 496,608 | 11,505 | $0.530 | 7.6 min |
| financial-document-processor | medium | data-processing | ❌ failed | 35 | 1,217,231 | 63,060 | $0.547 | 5.7 min |
| fix-code-vulnerability | hard | security | ✅ solved | 10 | 392,091 | 19,737 | $0.144 | 1.3 min |
| fix-git | easy | software-engineering | ✅ solved | 10 | 387,268 | 17,054 | $0.146 | 3.3 min |
| fix-ocaml-gc | hard | software-engineering | ✅ solved | 52 | 3,262,223 | 91,713 | $1.251 | 25.1 min |
| gcode-to-text | medium | file-operations | ✅ solved | 64 | 3,465,831 | 84,253 | $1.151 | 15.4 min |
| git-leak-recovery | medium | software-engineering | ✅ solved | 11 | 422,092 | 5,796 | $0.120 | 1.5 min |
| git-multibranch | medium | system-administration | ✅ solved | 38 | 1,947,898 | 23,971 | $0.545 | 9.0 min |
| gpt2-codegolf | hard | software-engineering | ⏱ timeout | 43 | 2,163,376 | 27,198 | $1.476 | 60.0 min |
| headless-terminal | medium | software-engineering | ✅ solved | 19 | 860,979 | 14,958 | $0.329 | 6.3 min |
| hf-model-inference | medium | data-science | ✅ solved | 10 | 380,807 | 5,907 | $0.113 | 3.0 min |
| install-windows-3.11 | hard | system-administration | ❌ failed | 102 | 6,968,317 | 56,473 | $2.002 | 30.4 min |
| kv-store-grpc | medium | software-engineering | ✅ solved | 9 | 341,540 | 6,033 | $0.103 | 2.5 min |
| large-scale-text-editing | medium | file-operations | ✅ solved | 18 | 752,118 | 8,475 | $0.360 | 7.3 min |
| largest-eigenval | medium | mathematics | ✅ solved | 25 | 1,138,904 | 15,330 | $0.429 | 6.1 min |
| llm-inference-batching-scheduler | hard | machine-learning | ✅ solved | 58 | 5,548,609 | 156,412 | $3.209 | 42.0 min |
| log-summary-date-ranges | medium | data-processing | ✅ solved | 7 | 253,952 | 5,872 | $0.082 | 1.3 min |
| mailman | medium | system-administration | ✅ solved | 91 | 7,495,690 | 72,125 | $2.326 | 26.6 min |
| make-doom-for-mips | hard | software-engineering | ⏱ timeout | 160 | 18,623,369 | 227,155 | $6.391 | 60.0 min |
| make-mips-interpreter | hard | software-engineering | ❌ failed | 99 | 10,602,706 | 145,552 | $4.276 | 47.2 min |
| mcmc-sampling-stan | hard | data-science | ✅ solved | 47 | 3,348,755 | 70,024 | $0.944 | 27.0 min |
| merge-diff-arc-agi-task | medium | debugging | ✅ solved | 33 | 1,489,291 | 54,882 | $0.550 | 16.4 min |
| model-extraction-relu-logits | hard | mathematics | ❌ failed | 40 | 2,186,378 | 33,983 | $1.288 | 17.4 min |
| modernize-scientific-stack | medium | scientific-computing | ✅ solved | 8 | 309,419 | 8,771 | $0.108 | 1.3 min |
| mteb-leaderboard | medium | data-science | ✅ solved | 4 | 124,910 | 325 | $0.075 | 2.2 min |
| mteb-retrieve | medium | data-science | ✅ solved | 9 | 338,332 | 7,062 | $0.112 | 3.0 min |
| multi-source-data-merger | medium | data-processing | ✅ solved | 8 | 299,556 | 7,022 | $0.142 | 2.8 min |
| nginx-request-logging | medium | system-administration | ✅ solved | 11 | 439,741 | 8,750 | $0.137 | 1.8 min |
| openssl-selfsigned-cert | medium | security | ❌ failed | 11 | 426,962 | 6,501 | $0.125 | 2.2 min |
| overfull-hbox | easy | debugging | ✅ solved | 45 | 2,464,056 | 30,433 | $0.901 | 16.1 min |
| password-recovery | hard | security | ✅ solved | 33 | 1,579,302 | 19,069 | $0.874 | 15.0 min |
| path-tracing | hard | software-engineering | ✅ solved | 262 | 26,460,778 | 346,898 | $10.795 | 109.7 min |
| path-tracing-reverse | hard | software-engineering | ⏱ timeout | 170 | 17,971,303 | 373,040 | $9.621 | 120.0 min |
| polyglot-c-py | medium | software-engineering | ❌ failed | 11 | 422,257 | 5,552 | $0.370 | 5.8 min |
| polyglot-rust-c | hard | software-engineering | ❌ failed | 3 | 80,536 | 2,653 | $0.080 | 13.3 min |
| portfolio-optimization | medium | optimization | ❌ failed | 8 | 313,695 | 11,094 | $0.145 | 4.3 min |
| protein-assembly | hard | scientific-computing | ✅ solved | 41 | 2,573,426 | 50,569 | $1.849 | 30.7 min |
| prove-plus-comm | easy | software-engineering | ✅ solved | 7 | 239,431 | 12,162 | $0.090 | 0.9 min |
| pypi-server | medium | software-engineering | ✅ solved | 15 | 603,607 | 6,981 | $0.165 | 3.9 min |
| pytorch-model-cli | medium | model-training | ✅ solved | 32 | 1,505,970 | 19,926 | $0.438 | 9.3 min |
| pytorch-model-recovery | medium | model-training | ✅ solved | 18 | 814,226 | 16,239 | $0.344 | 14.1 min |
| qemu-alpine-ssh | medium | system-administration | ✅ solved | 39 | 2,026,593 | 30,041 | $0.724 | 21.2 min |
| qemu-startup | medium | system-administration | ✅ solved | 25 | 1,086,800 | 12,472 | $0.470 | 12.1 min |
| query-optimize | medium | data-science | ❌ failed | 14 | 564,257 | 8,717 | $0.262 | 14.3 min |
| raman-fitting | medium | scientific-computing | ❌ failed | 34 | 1,731,134 | 29,874 | $0.894 | 13.9 min |
| regex-chess | hard | software-engineering | ❌ failed | 4 | 129,443 | 1,453 | $0.057 | 2.1 min |
| regex-log | medium | data-processing | ✅ solved | 10 | 380,065 | 5,785 | $0.463 | 7.4 min |
| reshard-c4-data | medium | data-science | ❌ failed | 59 | 3,513,094 | 70,607 | $3.219 | 64.2 min |
| rstan-to-pystan | medium | data-science | ✅ solved | 48 | 2,690,995 | 95,456 | $0.950 | 35.5 min |
| sam-cell-seg | hard | data-science | ❌ failed | 53 | 3,259,944 | 66,464 | $1.458 | 39.5 min |
| sanitize-git-repo | medium | security | ✅ solved | 12 | 712,805 | 57,064 | $0.395 | 2.9 min |
| schemelike-metacircular-eval | medium | software-engineering | ⏱ timeout | 74 | 5,585,032 | 175,235 | $4.370 | 160.0 min |
| sparql-university | hard | data-querying | ✅ solved | 17 | 773,789 | 15,944 | $1.151 | 15.3 min |
| sqlite-db-truncate | medium | debugging | ✅ solved | 10 | 380,755 | 7,161 | $0.203 | 2.8 min |
| sqlite-with-gcov | medium | system-administration | ❌ failed | 22 | 980,315 | 15,320 | $0.275 | 7.2 min |
| torch-pipeline-parallelism | hard | software-engineering | ❌ failed | 33 | 1,964,911 | 41,056 | $0.863 | 16.9 min |
| torch-tensor-parallelism | hard | software-engineering | ❌ failed | 24 | 1,086,495 | 18,363 | $0.440 | 18.5 min |
| train-fasttext | hard | model-training | ❌ failed | 39 | 2,647,796 | 260,108 | $1.289 | 53.2 min |
| tune-mjcf | medium | scientific-computing | ✅ solved | 21 | 927,646 | 13,076 | $0.374 | 8.5 min |
| video-processing | hard | video-processing | ❌ failed | 80 | 5,345,972 | 55,105 | $2.094 | 30.7 min |
| vulnerable-secret | medium | security | ✅ solved | 11 | 434,737 | 12,474 | $0.140 | 1.9 min |
| winning-avg-corewars | medium | software-engineering | ✅ solved | 57 | 3,293,598 | 38,085 | $1.507 | 29.5 min |
| write-compressor | hard | software-engineering | ⏱ timeout | 6 | 208,218 | 3,710 | $0.357 | 60.0 min |

## Full results — context-guru (codesmart) (Terminal-Bench 2.0, 89 tasks)

Full per-task results for the **context-guru (codesmart)** arm on Terminal-Bench 2.0 (`claude-code` on `aws/claude-sonnet-5`, live). Same cache-aware cost model and 4× budget as the other arms. For the four-way analysis (cost decomposition, per-component, verdict) see the **[Terminal-Bench comparison](terminal-bench-comparison.md)**; the reference arm is the **[baseline](#full-results-baseline-terminal-bench-20-89-tasks)**. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| attempted | solved | solve rate | completed | timed out | total billed cost | mean steps* | cache-hit |
|--:|--:|--:|--:|--:|--:|--:|--:|
| 89 | 58 | **65.2%** | 78 | 11 | $99.29 | 34.7 | 96.9% |

\* mean steps over the 78 completed tasks (timed-out runs are truncated). Solve rate over **completed-only** tasks: **57/78 = 73.1%**.

#### Token & cost accounting (cache-aware, all 89 tasks)

| tier | tokens | $/M | billed |
|---|--:|--:|--:|
| cache-read (input) | 204,631,836 | 0.20 | $40.93 |
| cache-write (input) | 6,525,522 | 2.50 | $16.31 |
| fresh (input) | 107,482 | 2.00 | $0.21 |
| completion (output) | 4,183,424 | 10.00 | $41.83 |
| **total** | | | **$99.29** |

Cache-read is **41%** of the bill at a **96.9%** cache-hit rate — as on SWE-bench, a heavily-cached agent, so the lever a compaction layer must pull is cache-read tokens.

### Timeouts (11 long-horizon tasks)

These tasks still hit the wall-clock budget under the **extended 4×** timeout (up to ~4 h each) and scored **reward 0** — counted as failures in the solve rate above. A large part of the cause is **gateway latency, not only agent capability**: Terminal-Bench's timeouts assume a fast endpoint (~2–5 s/request), but this IBM LiteLLM gateway runs **~26 s/request** (5–10× slower), so long-horizon tasks that need many round-trips run out of clock (concurrency is *not* the cause — latency was flat ~23–30 s/req from n=1 to n=24). They are all `hard`/long software-engineering and compute tasks (path-tracing, a MIPS Doom port, a metacircular evaluator, COBOL modernization, GPT-2 code-golf, CIFAR training). A compaction arm that cuts round-trips could bring some under budget, so the timeout count is itself a comparison metric.

| task | difficulty | category | steps before timeout | partial billed | budget (4×) |
|---|---|---|--:|--:|--:|
| caffe-cifar-10 | medium | machine-learning | None | $0.00 | 80 min |
| cobol-modernization | easy | software-engineering | 110 | $4.40 | 60 min |
| gpt2-codegolf | hard | software-engineering | 28 | $1.13 | 60 min |
| make-doom-for-mips | hard | software-engineering | 141 | $5.76 | 60 min |
| mteb-retrieve | medium | data-science | None | $0.00 | 120 min |
| path-tracing-reverse | hard | software-engineering | 58 | $2.51 | 120 min |
| polyglot-rust-c | hard | software-engineering | 50 | $2.81 | 60 min |
| protein-assembly | hard | scientific-computing | 8 | $0.12 | 120 min |
| pytorch-model-recovery | medium | model-training | 21 | $0.44 | 60 min |
| schemelike-metacircular-eval | medium | software-engineering | 96 | $4.77 | 160 min |
| write-compressor | hard | software-engineering | 4 | $0.19 | 60 min |

### By difficulty (all 89 tasks; timeouts = failures)

| difficulty | tasks | solved | rate | timed out | mean $/task |
|---|--:|--:|--:|--:|--:|
| easy | 4 | 4 | 100% | 1 | $1.451 |
| medium | 55 | 41 | 75% | 4 | $0.781 |
| hard | 30 | 13 | 43% | 6 | $1.685 |

### By category (all 89 tasks)

| category | tasks | solved | rate | mean $/task | mean steps* |
|---|--:|--:|--:|--:|--:|
| data-processing | 4 | 4 | 100% | $0.226 | 11.5 |
| data-querying | 1 | 1 | 100% | $0.818 | 16.0 |
| debugging | 5 | 5 | 100% | $1.324 | 56.6 |
| optimization | 1 | 1 | 100% | $0.609 | 23.0 |
| personal-assistant | 1 | 1 | 100% | $0.229 | 6.0 |
| file-operations | 5 | 4 | 80% | $0.801 | 36.2 |
| mathematics | 4 | 3 | 75% | $0.813 | 15.5 |
| security | 8 | 6 | 75% | $0.740 | 25.8 |
| machine-learning | 3 | 2 | 67% | $0.739 | 29.5 |
| system-administration | 9 | 6 | 67% | $0.725 | 38.8 |
| data-science | 8 | 5 | 62% | $1.682 | 56.9 |
| software-engineering | 26 | 15 | 58% | $1.662 | 33.7 |
| model-training | 4 | 2 | 50% | $0.599 | 33.0 |
| scientific-computing | 8 | 3 | 38% | $0.596 | 26.9 |
| games | 1 | 0 | 0% | $0.386 | 17.0 |
| video-processing | 1 | 0 | 0% | $3.961 | 129.0 |

### Per-task (all 89)

| task | difficulty | category | outcome | steps | cache_read | cache_write | billed | wall |
|---|---|---|:--:|--:|--:|--:|--:|--:|
| adaptive-rejection-sampler | medium | scientific-computing | ❌ failed | 32 | 1,674,712 | 28,817 | $1.053 | 26.5 min |
| bn-fit-modify | hard | scientific-computing | ✅ solved | 23 | 1,010,421 | 13,731 | $0.382 | 7.2 min |
| break-filter-js-from-html | medium | security | ✅ solved | 56 | 2,925,793 | 26,244 | $1.603 | 34.7 min |
| build-cython-ext | medium | debugging | ✅ solved | 116 | 8,772,811 | 73,085 | $2.208 | 26.5 min |
| build-pmars | medium | software-engineering | ✅ solved | 38 | 1,922,393 | 24,994 | $0.530 | 10.5 min |
| build-pov-ray | medium | software-engineering | ❌ failed | 30 | 1,500,560 | 24,878 | $0.428 | 5.2 min |
| caffe-cifar-10 | medium | machine-learning | ⏱ timeout | None | 0 | 0 | $0.000 | — |
| cancel-async-tasks | hard | software-engineering | ❌ failed | 9 | 336,627 | 5,252 | $0.172 | 2.1 min |
| chess-best-move | medium | games | ❌ failed | 17 | 704,277 | 10,456 | $0.386 | 6.0 min |
| circuit-fibsqrt | hard | software-engineering | ✅ solved | 60 | 4,235,103 | 81,094 | $3.590 | 91.2 min |
| cobol-modernization | easy | software-engineering | ⏱ timeout | 110 | 7,425,465 | 59,332 | $4.395 | 60.0 min |
| code-from-image | medium | software-engineering | ✅ solved | 5 | 165,696 | 4,236 | $0.049 | 0.7 min |
| compile-compcert | medium | system-administration | ✅ solved | 69 | 3,870,275 | 180,987 | $1.425 | 74.0 min |
| configure-git-webserver | hard | system-administration | ✅ solved | 21 | 911,693 | 11,638 | $0.281 | 6.7 min |
| constraints-scheduling | medium | personal-assistant | ✅ solved | 6 | 215,662 | 6,897 | $0.229 | 3.5 min |
| count-dataset-tokens | medium | model-training | ✅ solved | 10 | 389,414 | 7,591 | $0.129 | 6.0 min |
| crack-7z-hash | medium | security | ✅ solved | 19 | 788,652 | 24,805 | $0.245 | 17.9 min |
| custom-memory-heap-crash | medium | debugging | ✅ solved | 75 | 5,344,207 | 56,082 | $2.507 | 36.1 min |
| db-wal-recovery | medium | file-operations | ✅ solved | 10 | 387,762 | 6,433 | $0.129 | 1.6 min |
| distribution-search | medium | machine-learning | ✅ solved | 21 | 958,627 | 16,830 | $0.654 | 11.5 min |
| dna-assembly | hard | scientific-computing | ❌ failed | 26 | 1,250,596 | 26,031 | $0.749 | 15.2 min |
| dna-insert | medium | scientific-computing | ❌ failed | 29 | 1,499,781 | 28,732 | $0.793 | 14.9 min |
| extract-elf | medium | file-operations | ✅ solved | 18 | 801,751 | 27,891 | $0.610 | 9.3 min |
| extract-moves-from-video | hard | file-operations | ❌ failed | 112 | 7,449,833 | 227,396 | $2.691 | 108.7 min |
| feal-differential-cryptanalysis | hard | mathematics | ✅ solved | 13 | 531,990 | 9,751 | $0.851 | 13.5 min |
| feal-linear-cryptanalysis | hard | mathematics | ✅ solved | 20 | 984,819 | 32,867 | $1.602 | 55.6 min |
| filter-js-from-html | medium | security | ❌ failed | 8 | 302,660 | 7,716 | $0.239 | 3.2 min |
| financial-document-processor | medium | data-processing | ✅ solved | 25 | 974,052 | 35,433 | $0.426 | 6.1 min |
| fix-code-vulnerability | hard | security | ✅ solved | 11 | 461,737 | 23,400 | $0.179 | 1.3 min |
| fix-git | easy | software-engineering | ✅ solved | 12 | 475,724 | 16,806 | $0.160 | 1.3 min |
| fix-ocaml-gc | hard | software-engineering | ✅ solved | 21 | 1,209,789 | 86,820 | $0.707 | 19.5 min |
| gcode-to-text | medium | file-operations | ✅ solved | 33 | 1,226,297 | 37,655 | $0.441 | 7.2 min |
| git-leak-recovery | medium | software-engineering | ✅ solved | 10 | 382,074 | 6,004 | $0.113 | 1.0 min |
| git-multibranch | medium | system-administration | ❌ failed | 37 | 1,860,897 | 21,763 | $0.581 | 5.6 min |
| gpt2-codegolf | hard | software-engineering | ⏱ timeout | 28 | 1,320,855 | 16,647 | $1.128 | 60.0 min |
| headless-terminal | medium | software-engineering | ✅ solved | 55 | 3,000,428 | 31,593 | $1.066 | 15.5 min |
| hf-model-inference | medium | data-science | ✅ solved | 10 | 383,229 | 6,288 | $0.119 | 2.4 min |
| install-windows-3.11 | hard | system-administration | ❌ failed | 49 | 2,592,143 | 29,114 | $0.729 | 8.1 min |
| kv-store-grpc | medium | software-engineering | ✅ solved | 11 | 429,312 | 6,723 | $0.126 | 1.3 min |
| large-scale-text-editing | medium | file-operations | ✅ solved | 8 | 307,912 | 7,638 | $0.135 | 3.2 min |
| largest-eigenval | medium | mathematics | ✅ solved | 20 | 881,834 | 13,592 | $0.333 | 4.0 min |
| llm-inference-batching-scheduler | hard | machine-learning | ✅ solved | 38 | 2,496,481 | 54,992 | $1.562 | 27.8 min |
| log-summary-date-ranges | medium | data-processing | ✅ solved | 6 | 210,077 | 5,648 | $0.071 | 0.6 min |
| mailman | medium | system-administration | ❌ failed | 81 | 6,947,541 | 75,092 | $2.097 | 23.8 min |
| make-doom-for-mips | hard | software-engineering | ⏱ timeout | 141 | 13,556,629 | 354,083 | $5.760 | 60.0 min |
| make-mips-interpreter | hard | software-engineering | ✅ solved | 171 | 16,819,007 | 906,773 | $7.554 | 71.4 min |
| mcmc-sampling-stan | hard | data-science | ✅ solved | 44 | 2,662,323 | 51,716 | $0.790 | 22.6 min |
| merge-diff-arc-agi-task | medium | debugging | ✅ solved | 25 | 1,114,428 | 13,596 | $0.406 | 4.6 min |
| model-extraction-relu-logits | hard | mathematics | ❌ failed | 9 | 357,509 | 13,370 | $0.467 | 7.3 min |
| modernize-scientific-stack | medium | scientific-computing | ✅ solved | 6 | 215,145 | 6,818 | $0.083 | 0.7 min |
| mteb-leaderboard | medium | data-science | ✅ solved | 147 | 12,277,618 | 1,122,303 | $6.045 | 73.5 min |
| mteb-retrieve | medium | data-science | ⏱ timeout | None | 0 | 0 | $0.000 | — |
| multi-source-data-merger | medium | data-processing | ✅ solved | 7 | 256,234 | 6,751 | $0.103 | 3.1 min |
| nginx-request-logging | medium | system-administration | ✅ solved | 12 | 477,493 | 7,266 | $0.143 | 2.1 min |
| openssl-selfsigned-cert | medium | security | ✅ solved | 10 | 383,978 | 6,629 | $0.122 | 1.9 min |
| overfull-hbox | easy | debugging | ✅ solved | 53 | 3,388,975 | 42,344 | $1.168 | 13.1 min |
| password-recovery | hard | security | ❌ failed | 69 | 3,995,130 | 40,768 | $2.480 | 39.3 min |
| path-tracing | hard | software-engineering | ✅ solved | 114 | 9,509,036 | 442,884 | $4.820 | 77.1 min |
| path-tracing-reverse | hard | software-engineering | ⏱ timeout | 58 | 4,628,243 | 258,646 | $2.507 | 34.2 min |
| polyglot-c-py | medium | software-engineering | ❌ failed | 12 | 464,910 | 5,731 | $0.360 | 6.3 min |
| polyglot-rust-c | hard | software-engineering | ⏱ timeout | 50 | 2,642,242 | 30,164 | $2.810 | 60.0 min |
| portfolio-optimization | medium | optimization | ✅ solved | 23 | 1,127,622 | 22,185 | $0.609 | 11.5 min |
| protein-assembly | hard | scientific-computing | ⏱ timeout | 8 | 269,038 | 4,867 | $0.116 | 2.4 min |
| prove-plus-comm | easy | software-engineering | ✅ solved | 6 | 197,256 | 12,101 | $0.079 | 0.7 min |
| pypi-server | medium | software-engineering | ✅ solved | 16 | 653,138 | 7,711 | $0.174 | 8.0 min |
| pytorch-model-cli | medium | model-training | ✅ solved | 25 | 1,124,334 | 57,946 | $0.461 | 5.5 min |
| pytorch-model-recovery | medium | model-training | ⏱ timeout | 21 | 943,457 | 14,990 | $0.437 | 8.6 min |
| qemu-alpine-ssh | medium | system-administration | ✅ solved | 37 | 1,717,574 | 21,608 | $0.579 | 8.0 min |
| qemu-startup | medium | system-administration | ✅ solved | 24 | 1,024,044 | 9,915 | $0.459 | 7.7 min |
| query-optimize | medium | data-science | ❌ failed | 44 | 2,213,453 | 25,584 | $0.877 | 30.2 min |
| raman-fitting | medium | scientific-computing | ❌ failed | 48 | 2,626,488 | 36,884 | $1.164 | 19.3 min |
| regex-chess | hard | software-engineering | ❌ failed | 2 | 41,954 | 0 | $0.031 | 1.3 min |
| regex-log | medium | data-processing | ✅ solved | 8 | 301,024 | 9,175 | $0.303 | 4.3 min |
| reshard-c4-data | medium | data-science | ✅ solved | 25 | 1,264,046 | 22,514 | $1.226 | 19.9 min |
| rstan-to-pystan | medium | data-science | ✅ solved | 73 | 4,191,501 | 588,048 | $2.717 | 118.7 min |
| sam-cell-seg | hard | data-science | ❌ failed | 55 | 3,065,248 | 37,729 | $1.683 | 29.0 min |
| sanitize-git-repo | medium | security | ✅ solved | 22 | 1,591,443 | 186,042 | $0.893 | 7.4 min |
| schemelike-metacircular-eval | medium | software-engineering | ⏱ timeout | 96 | 6,646,069 | 120,462 | $4.768 | 160.0 min |
| sparql-university | hard | data-querying | ✅ solved | 16 | 734,172 | 13,592 | $0.818 | 12.5 min |
| sqlite-db-truncate | medium | debugging | ✅ solved | 14 | 576,165 | 11,206 | $0.332 | 5.1 min |
| sqlite-with-gcov | medium | system-administration | ✅ solved | 19 | 827,372 | 13,175 | $0.236 | 4.8 min |
| torch-pipeline-parallelism | hard | software-engineering | ✅ solved | 8 | 293,023 | 16,998 | $0.278 | 4.4 min |
| torch-tensor-parallelism | hard | software-engineering | ❌ failed | 10 | 405,241 | 12,627 | $0.290 | 4.9 min |
| train-fasttext | hard | model-training | ❌ failed | 64 | 3,191,706 | 210,917 | $1.371 | 113.6 min |
| tune-mjcf | medium | scientific-computing | ✅ solved | 24 | 1,086,103 | 13,996 | $0.426 | 10.7 min |
| video-processing | hard | video-processing | ❌ failed | 129 | 10,928,251 | 200,113 | $3.961 | 69.7 min |
| vulnerable-secret | medium | security | ✅ solved | 11 | 439,504 | 12,575 | $0.162 | 2.2 min |
| winning-avg-corewars | medium | software-engineering | ✅ solved | 51 | 2,757,923 | 30,691 | $1.125 | 15.8 min |
| write-compressor | hard | software-engineering | ⏱ timeout | 4 | 123,825 | 3,055 | $0.194 | 60.0 min |

## Full results — headroom (hd-cache) (Terminal-Bench 2.0, 89 tasks)

Full per-task results for the **headroom (hd-cache)** arm on Terminal-Bench 2.0 (`claude-code` on `aws/claude-sonnet-5`, live). Same cache-aware cost model and 4× budget as the other arms. For the four-way analysis (cost decomposition, per-component, verdict) see the **[Terminal-Bench comparison](terminal-bench-comparison.md)**; the reference arm is the **[baseline](#full-results-baseline-terminal-bench-20-89-tasks)**. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| attempted | solved | solve rate | completed | timed out | total billed cost | mean steps* | cache-hit |
|--:|--:|--:|--:|--:|--:|--:|--:|
| 89 | 64 | **71.9%** | 78 | 11 | $114.75 | 32.0 | 94.1% |

\* mean steps over the 78 completed tasks (timed-out runs are truncated). Solve rate over **completed-only** tasks: **64/78 = 82.1%**.

#### Token & cost accounting (cache-aware, all 89 tasks)

| tier | tokens | $/M | billed |
|---|--:|--:|--:|
| cache-read (input) | 198,020,429 | 0.20 | $39.60 |
| cache-write (input) | 12,369,660 | 2.50 | $30.92 |
| fresh (input) | 97,763 | 2.00 | $0.20 |
| completion (output) | 4,402,777 | 10.00 | $44.03 |
| **total** | | | **$114.75** |

Cache-read is **35%** of the bill at a **94.1%** cache-hit rate — as on SWE-bench, a heavily-cached agent, so the lever a compaction layer must pull is cache-read tokens.

### Timeouts (11 long-horizon tasks)

These tasks still hit the wall-clock budget under the **extended 4×** timeout (up to ~4 h each) and scored **reward 0** — counted as failures in the solve rate above. A large part of the cause is **gateway latency, not only agent capability**: Terminal-Bench's timeouts assume a fast endpoint (~2–5 s/request), but this IBM LiteLLM gateway runs **~26 s/request** (5–10× slower), so long-horizon tasks that need many round-trips run out of clock (concurrency is *not* the cause — latency was flat ~23–30 s/req from n=1 to n=24). They are all `hard`/long software-engineering and compute tasks (path-tracing, a MIPS Doom port, a metacircular evaluator, COBOL modernization, GPT-2 code-golf, CIFAR training). A compaction arm that cuts round-trips could bring some under budget, so the timeout count is itself a comparison metric.

| task | difficulty | category | steps before timeout | partial billed | budget (4×) |
|---|---|---|--:|--:|--:|
| caffe-cifar-10 | medium | machine-learning | 38 | $0.77 | 80 min |
| cobol-modernization | easy | software-engineering | 69 | $2.35 | 60 min |
| extract-moves-from-video | hard | file-operations | 160 | $24.12 | 120 min |
| make-doom-for-mips | hard | software-engineering | 101 | $4.24 | 60 min |
| path-tracing | hard | software-engineering | 142 | $6.16 | 120 min |
| protein-assembly | hard | scientific-computing | 2 | $0.00 | 120 min |
| pytorch-model-cli | medium | model-training | 35 | $0.47 | 60 min |
| query-optimize | medium | data-science | 19 | $0.31 | 60 min |
| schemelike-metacircular-eval | medium | software-engineering | 77 | $4.66 | 160 min |
| tune-mjcf | medium | scientific-computing | 25 | $0.42 | 60 min |
| write-compressor | hard | software-engineering | 5 | $0.47 | 60 min |

### By difficulty (all 89 tasks; timeouts = failures)

| difficulty | tasks | solved | rate | timed out | mean $/task |
|---|--:|--:|--:|--:|--:|
| easy | 4 | 3 | 75% | 1 | $0.878 |
| medium | 55 | 42 | 76% | 5 | $0.631 |
| hard | 30 | 19 | 63% | 5 | $2.551 |

### By category (all 89 tasks)

| category | tasks | solved | rate | mean $/task | mean steps* |
|---|--:|--:|--:|--:|--:|
| data-querying | 1 | 1 | 100% | $0.351 | 8.0 |
| debugging | 5 | 5 | 100% | $0.909 | 42.8 |
| games | 1 | 1 | 100% | $0.383 | 20.0 |
| mathematics | 4 | 4 | 100% | $0.703 | 10.8 |
| optimization | 1 | 1 | 100% | $0.464 | 19.0 |
| personal-assistant | 1 | 1 | 100% | $0.244 | 6.0 |
| video-processing | 1 | 1 | 100% | $3.080 | 90.0 |
| system-administration | 9 | 8 | 89% | $0.933 | 47.2 |
| data-processing | 4 | 3 | 75% | $0.212 | 13.8 |
| data-science | 8 | 6 | 75% | $1.105 | 49.7 |
| security | 8 | 6 | 75% | $0.371 | 19.6 |
| machine-learning | 3 | 2 | 67% | $1.060 | 28.0 |
| software-engineering | 26 | 17 | 65% | $1.696 | 34.0 |
| file-operations | 5 | 3 | 60% | $5.065 | 15.8 |
| model-training | 4 | 2 | 50% | $0.801 | 33.3 |
| scientific-computing | 8 | 3 | 38% | $0.752 | 30.3 |

### Per-task (all 89)

| task | difficulty | category | outcome | steps | cache_read | cache_write | billed | wall |
|---|---|---|:--:|--:|--:|--:|--:|--:|
| adaptive-rejection-sampler | medium | scientific-computing | ❌ failed | 30 | 1,438,504 | 52,222 | $1.253 | 26.4 min |
| bn-fit-modify | hard | scientific-computing | ✅ solved | 22 | 928,502 | 13,410 | $0.375 | 7.2 min |
| break-filter-js-from-html | medium | security | ✅ solved | 35 | 1,600,416 | 17,679 | $0.879 | 21.7 min |
| build-cython-ext | medium | debugging | ✅ solved | 62 | 4,496,202 | 63,447 | $1.296 | 14.0 min |
| build-pmars | medium | software-engineering | ✅ solved | 39 | 1,926,499 | 29,817 | $0.560 | 7.3 min |
| build-pov-ray | medium | software-engineering | ✅ solved | 36 | 1,752,704 | 28,039 | $0.517 | 11.2 min |
| caffe-cifar-10 | medium | machine-learning | ⏱ timeout | 38 | 1,748,587 | 131,625 | $0.769 | 57.0 min |
| cancel-async-tasks | hard | software-engineering | ✅ solved | 8 | 279,969 | 5,770 | $0.200 | 2.5 min |
| chess-best-move | medium | games | ✅ solved | 20 | 806,656 | 9,186 | $0.383 | 4.9 min |
| circuit-fibsqrt | hard | software-engineering | ✅ solved | 53 | 3,341,016 | 46,604 | $2.881 | 178.9 min |
| cobol-modernization | easy | software-engineering | ⏱ timeout | 69 | 3,877,615 | 36,673 | $2.353 | 60.0 min |
| code-from-image | medium | software-engineering | ✅ solved | 5 | 156,544 | 4,231 | $0.047 | 0.5 min |
| compile-compcert | medium | system-administration | ✅ solved | 85 | 5,515,699 | 227,890 | $1.922 | 78.9 min |
| configure-git-webserver | hard | system-administration | ✅ solved | 13 | 494,249 | 7,317 | $0.166 | 7.2 min |
| constraints-scheduling | medium | personal-assistant | ✅ solved | 6 | 204,738 | 8,481 | $0.244 | 3.2 min |
| count-dataset-tokens | medium | model-training | ✅ solved | 13 | 526,046 | 17,052 | $0.196 | 5.3 min |
| crack-7z-hash | medium | security | ✅ solved | 20 | 814,384 | 11,301 | $0.221 | 6.3 min |
| custom-memory-heap-crash | medium | debugging | ✅ solved | 52 | 3,359,686 | 49,983 | $1.714 | 30.4 min |
| db-wal-recovery | medium | file-operations | ❌ failed | 17 | 689,904 | 13,305 | $0.408 | 5.6 min |
| distribution-search | medium | machine-learning | ✅ solved | 16 | 663,356 | 14,575 | $0.538 | 7.5 min |
| dna-assembly | hard | scientific-computing | ✅ solved | 56 | 3,250,476 | 53,252 | $2.386 | 43.6 min |
| dna-insert | medium | scientific-computing | ❌ failed | 18 | 773,481 | 13,546 | $0.320 | 5.5 min |
| extract-elf | medium | file-operations | ✅ solved | 18 | 763,108 | 13,337 | $0.390 | 5.2 min |
| extract-moves-from-video | hard | file-operations | ⏱ timeout | 160 | 7,363,596 | 7,806,411 | $24.118 | 120.0 min |
| feal-differential-cryptanalysis | hard | mathematics | ✅ solved | 13 | 502,491 | 9,262 | $1.003 | 16.9 min |
| feal-linear-cryptanalysis | hard | mathematics | ✅ solved | 13 | 532,046 | 12,513 | $1.316 | 45.3 min |
| filter-js-from-html | medium | security | ❌ failed | 22 | 989,347 | 16,881 | $0.496 | 5.7 min |
| financial-document-processor | medium | data-processing | ❌ failed | 26 | 533,975 | 61,704 | $0.360 | 2.5 min |
| fix-code-vulnerability | hard | security | ✅ solved | 16 | 633,232 | 20,508 | $0.206 | 1.5 min |
| fix-git | easy | software-engineering | ✅ solved | 11 | 412,077 | 17,470 | $0.151 | 1.0 min |
| fix-ocaml-gc | hard | software-engineering | ✅ solved | 27 | 1,340,872 | 145,817 | $0.778 | 27.9 min |
| gcode-to-text | medium | file-operations | ✅ solved | 17 | 692,582 | 11,878 | $0.227 | 4.1 min |
| git-leak-recovery | medium | software-engineering | ✅ solved | 16 | 611,482 | 7,048 | $0.172 | 1.5 min |
| git-multibranch | medium | system-administration | ✅ solved | 33 | 1,485,333 | 16,544 | $0.470 | 4.5 min |
| gpt2-codegolf | hard | software-engineering | ❌ failed | 2 | 0 | 39,156 | $0.138 | 0.9 min |
| headless-terminal | medium | software-engineering | ✅ solved | 16 | 652,521 | 15,294 | $0.289 | 13.3 min |
| hf-model-inference | medium | data-science | ✅ solved | 12 | 448,133 | 12,428 | $0.149 | 11.6 min |
| install-windows-3.11 | hard | system-administration | ❌ failed | 89 | 6,294,323 | 136,137 | $2.271 | 40.8 min |
| kv-store-grpc | medium | software-engineering | ✅ solved | 11 | 407,203 | 6,197 | $0.121 | 1.8 min |
| large-scale-text-editing | medium | file-operations | ✅ solved | 11 | 406,201 | 6,239 | $0.180 | 3.3 min |
| largest-eigenval | medium | mathematics | ✅ solved | 10 | 370,777 | 8,085 | $0.168 | 2.8 min |
| llm-inference-batching-scheduler | hard | machine-learning | ✅ solved | 40 | 2,384,925 | 52,778 | $1.873 | 28.7 min |
| log-summary-date-ranges | medium | data-processing | ✅ solved | 8 | 280,917 | 5,643 | $0.086 | 0.6 min |
| mailman | medium | system-administration | ✅ solved | 71 | 5,039,376 | 54,115 | $1.495 | 15.4 min |
| make-doom-for-mips | hard | software-engineering | ⏱ timeout | 101 | 11,022,228 | 111,639 | $4.237 | 60.0 min |
| make-mips-interpreter | hard | software-engineering | ❌ failed | 181 | 18,796,281 | 222,145 | $7.696 | 83.6 min |
| mcmc-sampling-stan | hard | data-science | ✅ solved | 39 | 2,322,900 | 54,635 | $0.702 | 20.2 min |
| merge-diff-arc-agi-task | medium | debugging | ✅ solved | 37 | 1,639,043 | 15,823 | $0.472 | 6.6 min |
| model-extraction-relu-logits | hard | mathematics | ✅ solved | 7 | 240,960 | 7,470 | $0.325 | 5.7 min |
| modernize-scientific-stack | medium | scientific-computing | ✅ solved | 6 | 202,237 | 6,055 | $0.067 | 0.6 min |
| mteb-leaderboard | medium | data-science | ✅ solved | 91 | 6,713,015 | 81,960 | $2.041 | 55.8 min |
| mteb-retrieve | medium | data-science | ✅ solved | 15 | 591,826 | 9,506 | $0.184 | 8.3 min |
| multi-source-data-merger | medium | data-processing | ✅ solved | 9 | 328,735 | 7,246 | $0.130 | 1.5 min |
| nginx-request-logging | medium | system-administration | ✅ solved | 12 | 458,387 | 9,220 | $0.144 | 2.4 min |
| openssl-selfsigned-cert | medium | security | ✅ solved | 16 | 620,007 | 8,407 | $0.184 | 1.6 min |
| overfull-hbox | easy | debugging | ✅ solved | 55 | 2,900,215 | 28,336 | $0.918 | 19.9 min |
| password-recovery | hard | security | ✅ solved | 24 | 1,018,613 | 12,598 | $0.475 | 6.0 min |
| path-tracing | hard | software-engineering | ⏱ timeout | 142 | 13,062,719 | 184,070 | $6.155 | 120.0 min |
| path-tracing-reverse | hard | software-engineering | ✅ solved | 112 | 11,500,154 | 209,909 | $6.088 | 102.2 min |
| polyglot-c-py | medium | software-engineering | ❌ failed | 11 | 400,585 | 5,841 | $0.396 | 8.5 min |
| polyglot-rust-c | hard | software-engineering | ✅ solved | 7 | 390,788 | 39,597 | $0.573 | 11.7 min |
| portfolio-optimization | medium | optimization | ✅ solved | 19 | 859,707 | 20,442 | $0.464 | 8.9 min |
| protein-assembly | hard | scientific-computing | ⏱ timeout | 2 | 0 | 0 | $0.000 | 0.1 min |
| prove-plus-comm | easy | software-engineering | ✅ solved | 7 | 225,644 | 12,207 | $0.091 | 5.8 min |
| pypi-server | medium | software-engineering | ✅ solved | 15 | 571,478 | 7,504 | $0.160 | 1.9 min |
| pytorch-model-cli | medium | model-training | ⏱ timeout | 35 | 1,595,679 | 20,587 | $0.471 | 11.1 min |
| pytorch-model-recovery | medium | model-training | ✅ solved | 19 | 844,676 | 18,634 | $0.377 | 12.7 min |
| qemu-alpine-ssh | medium | system-administration | ✅ solved | 79 | 4,459,313 | 41,132 | $1.345 | 39.3 min |
| qemu-startup | medium | system-administration | ✅ solved | 21 | 832,903 | 9,683 | $0.324 | 10.1 min |
| query-optimize | medium | data-science | ⏱ timeout | 19 | 757,494 | 15,322 | $0.307 | 10.2 min |
| raman-fitting | medium | scientific-computing | ❌ failed | 50 | 2,706,005 | 43,519 | $1.196 | 15.4 min |
| regex-chess | hard | software-engineering | ✅ solved | 75 | 6,457,203 | 482,484 | $3.309 | 79.9 min |
| regex-log | medium | data-processing | ✅ solved | 12 | 450,419 | 8,037 | $0.270 | 3.9 min |
| reshard-c4-data | medium | data-science | ✅ solved | 33 | 1,497,470 | 15,842 | $0.924 | 16.5 min |
| rstan-to-pystan | medium | data-science | ✅ solved | 88 | 5,680,153 | 442,469 | $2.661 | 106.6 min |
| sam-cell-seg | hard | data-science | ❌ failed | 70 | 4,414,374 | 60,442 | $1.873 | 35.5 min |
| sanitize-git-repo | medium | security | ❌ failed | 8 | 403,241 | 45,268 | $0.235 | 1.1 min |
| schemelike-metacircular-eval | medium | software-engineering | ⏱ timeout | 77 | 5,741,094 | 99,381 | $4.664 | 160.0 min |
| sparql-university | hard | data-querying | ✅ solved | 8 | 301,163 | 10,650 | $0.351 | 4.2 min |
| sqlite-db-truncate | medium | debugging | ✅ solved | 8 | 277,629 | 6,865 | $0.146 | 1.6 min |
| sqlite-with-gcov | medium | system-administration | ✅ solved | 22 | 924,943 | 16,688 | $0.264 | 8.7 min |
| torch-pipeline-parallelism | hard | software-engineering | ✅ solved | 41 | 2,156,546 | 25,746 | $0.994 | 35.2 min |
| torch-tensor-parallelism | hard | software-engineering | ❌ failed | 12 | 469,160 | 11,589 | $0.337 | 8.7 min |
| train-fasttext | hard | model-training | ❌ failed | 68 | 3,683,278 | 478,817 | $2.159 | 136.9 min |
| tune-mjcf | medium | scientific-computing | ⏱ timeout | 25 | 1,121,523 | 17,848 | $0.420 | 13.2 min |
| video-processing | hard | video-processing | ✅ solved | 90 | 6,490,786 | 73,613 | $3.080 | 70.4 min |
| vulnerable-secret | medium | security | ✅ solved | 16 | 654,879 | 15,453 | $0.270 | 2.8 min |
| winning-avg-corewars | medium | software-engineering | ✅ solved | 28 | 1,288,502 | 20,289 | $0.717 | 11.7 min |
| write-compressor | hard | software-engineering | ⏱ timeout | 5 | 156,724 | 3,842 | $0.471 | 60.0 min |

## Full results — rtk (Terminal-Bench 2.0, 89 tasks)

Full per-task results for the **rtk** arm on Terminal-Bench 2.0 (`claude-code` on `aws/claude-sonnet-5`, live). Same cache-aware cost model and 4× budget as the other arms. For the four-way analysis (cost decomposition, per-component, verdict) see the **[Terminal-Bench comparison](terminal-bench-comparison.md)**; the reference arm is the **[baseline](#full-results-baseline-terminal-bench-20-89-tasks)**. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| attempted | solved | solve rate | completed | timed out | total billed cost | mean steps* | cache-hit |
|--:|--:|--:|--:|--:|--:|--:|--:|
| 89 | 55 | **61.8%** | 82 | 7 | $118.83 | 40.0 | 97.3% |

\* mean steps over the 82 completed tasks (timed-out runs are truncated). Solve rate over **completed-only** tasks: **55/82 = 67.1%**.

#### Token & cost accounting (cache-aware, all 89 tasks)

| tier | tokens | $/M | billed |
|---|--:|--:|--:|
| cache-read (input) | 253,994,766 | 0.20 | $50.80 |
| cache-write (input) | 7,047,955 | 2.50 | $17.62 |
| fresh (input) | 55,363 | 2.00 | $0.11 |
| completion (output) | 5,029,700 | 10.00 | $50.30 |
| **total** | | | **$118.83** |

Cache-read is **43%** of the bill at a **97.3%** cache-hit rate — as on SWE-bench, a heavily-cached agent, so the lever a compaction layer must pull is cache-read tokens.

### Timeouts (7 long-horizon tasks)

These tasks still hit the wall-clock budget under the **extended 4×** timeout (up to ~4 h each) and scored **reward 0** — counted as failures in the solve rate above. A large part of the cause is **gateway latency, not only agent capability**: Terminal-Bench's timeouts assume a fast endpoint (~2–5 s/request), but this IBM LiteLLM gateway runs **~26 s/request** (5–10× slower), so long-horizon tasks that need many round-trips run out of clock (concurrency is *not* the cause — latency was flat ~23–30 s/req from n=1 to n=24). They are all `hard`/long software-engineering and compute tasks (path-tracing, a MIPS Doom port, a metacircular evaluator, COBOL modernization, GPT-2 code-golf, CIFAR training). A compaction arm that cuts round-trips could bring some under budget, so the timeout count is itself a comparison metric.

| task | difficulty | category | steps before timeout | partial billed | budget (4×) |
|---|---|---|--:|--:|--:|
| cobol-modernization | easy | software-engineering | 88 | $4.11 | 60 min |
| make-doom-for-mips | hard | software-engineering | 149 | $5.51 | 60 min |
| query-optimize | medium | data-science | 21 | $0.35 | 60 min |
| schemelike-metacircular-eval | medium | software-engineering | 71 | $5.57 | 160 min |
| torch-pipeline-parallelism | hard | software-engineering | 13 | $0.84 | 60 min |
| tune-mjcf | medium | scientific-computing | 26 | $0.50 | 60 min |
| write-compressor | hard | software-engineering | 7 | $0.52 | 60 min |

### By difficulty (all 89 tasks; timeouts = failures)

| difficulty | tasks | solved | rate | timed out | mean $/task |
|---|--:|--:|--:|--:|--:|
| easy | 4 | 3 | 75% | 1 | $1.470 |
| medium | 55 | 38 | 69% | 3 | $0.783 |
| hard | 30 | 14 | 47% | 3 | $2.329 |

### By category (all 89 tasks)

| category | tasks | solved | rate | mean $/task | mean steps* |
|---|--:|--:|--:|--:|--:|
| data-querying | 1 | 1 | 100% | $0.565 | 18.0 |
| debugging | 5 | 5 | 100% | $1.013 | 44.8 |
| optimization | 1 | 1 | 100% | $0.740 | 34.0 |
| personal-assistant | 1 | 1 | 100% | $0.363 | 7.0 |
| file-operations | 5 | 4 | 80% | $1.302 | 55.4 |
| data-processing | 4 | 3 | 75% | $0.321 | 14.8 |
| mathematics | 4 | 3 | 75% | $1.034 | 18.2 |
| security | 8 | 6 | 75% | $0.681 | 22.0 |
| machine-learning | 3 | 2 | 67% | $0.753 | 21.7 |
| system-administration | 9 | 6 | 67% | $1.263 | 56.0 |
| software-engineering | 26 | 15 | 58% | $1.961 | 44.0 |
| data-science | 8 | 4 | 50% | $1.345 | 51.6 |
| model-training | 4 | 2 | 50% | $2.558 | 61.5 |
| scientific-computing | 8 | 2 | 25% | $0.876 | 31.0 |
| games | 1 | 0 | 0% | $0.785 | 33.0 |
| video-processing | 1 | 0 | 0% | $1.333 | 61.0 |

### Per-task (all 89)

| task | difficulty | category | outcome | steps | cache_read | cache_write | billed | wall |
|---|---|---|:--:|--:|--:|--:|--:|--:|
| adaptive-rejection-sampler | medium | scientific-computing | ❌ failed | 22 | 1,051,805 | 22,713 | $0.697 | 19.1 min |
| bn-fit-modify | hard | scientific-computing | ✅ solved | 28 | 1,333,761 | 21,340 | $0.503 | 11.4 min |
| break-filter-js-from-html | medium | security | ✅ solved | 25 | 1,042,799 | 48,137 | $0.418 | 4.4 min |
| build-cython-ext | medium | debugging | ✅ solved | 69 | 4,517,952 | 48,120 | $1.217 | 18.3 min |
| build-pmars | medium | software-engineering | ✅ solved | 27 | 1,289,458 | 21,190 | $0.373 | 6.5 min |
| build-pov-ray | medium | software-engineering | ❌ failed | 39 | 2,052,249 | 28,593 | $0.568 | 7.9 min |
| caffe-cifar-10 | medium | machine-learning | ❌ failed | 15 | 339,775 | 58,234 | $0.413 | 14.4 min |
| cancel-async-tasks | hard | software-engineering | ❌ failed | 14 | 578,488 | 9,560 | $0.327 | 4.3 min |
| chess-best-move | medium | games | ❌ failed | 33 | 1,493,538 | 13,834 | $0.785 | 10.1 min |
| circuit-fibsqrt | hard | software-engineering | ✅ solved | 93 | 7,006,395 | 67,340 | $4.372 | 147.9 min |
| cobol-modernization | easy | software-engineering | ⏱ timeout | 88 | 5,909,121 | 58,247 | $4.109 | 60.0 min |
| code-from-image | medium | software-engineering | ✅ solved | 5 | 167,244 | 4,755 | $0.051 | 0.8 min |
| compile-compcert | medium | system-administration | ✅ solved | 63 | 3,509,261 | 155,507 | $1.265 | 75.2 min |
| configure-git-webserver | hard | system-administration | ❌ failed | 3 | 80,728 | 3,365 | $0.093 | 1.8 min |
| constraints-scheduling | medium | personal-assistant | ✅ solved | 7 | 263,567 | 7,389 | $0.363 | 5.1 min |
| count-dataset-tokens | medium | model-training | ❌ failed | 19 | 869,572 | 14,470 | $0.269 | 12.7 min |
| crack-7z-hash | medium | security | ✅ solved | 18 | 754,641 | 9,396 | $0.194 | 5.2 min |
| custom-memory-heap-crash | medium | debugging | ✅ solved | 37 | 2,130,474 | 35,044 | $1.575 | 22.9 min |
| db-wal-recovery | medium | file-operations | ✅ solved | 11 | 444,111 | 9,057 | $0.149 | 1.9 min |
| distribution-search | medium | machine-learning | ✅ solved | 9 | 354,059 | 10,033 | $0.284 | 4.0 min |
| dna-assembly | hard | scientific-computing | ❌ failed | 36 | 1,954,477 | 37,726 | $1.211 | 29.4 min |
| dna-insert | medium | scientific-computing | ❌ failed | 34 | 1,929,352 | 33,279 | $0.784 | 11.6 min |
| extract-elf | medium | file-operations | ✅ solved | 18 | 817,038 | 14,957 | $0.482 | 6.7 min |
| extract-moves-from-video | hard | file-operations | ❌ failed | 154 | 10,055,685 | 423,412 | $4.524 | 87.9 min |
| feal-differential-cryptanalysis | hard | mathematics | ✅ solved | 20 | 885,659 | 14,868 | $1.044 | 21.2 min |
| feal-linear-cryptanalysis | hard | mathematics | ✅ solved | 21 | 996,093 | 18,872 | $2.220 | 55.5 min |
| filter-js-from-html | medium | security | ❌ failed | 5 | 169,014 | 6,086 | $0.178 | 2.5 min |
| financial-document-processor | medium | data-processing | ❌ failed | 30 | 964,608 | 79,818 | $0.577 | 4.8 min |
| fix-code-vulnerability | hard | security | ✅ solved | 8 | 298,343 | 15,737 | $0.115 | 0.9 min |
| fix-git | easy | software-engineering | ✅ solved | 10 | 392,314 | 17,427 | $0.151 | 3.4 min |
| fix-ocaml-gc | hard | software-engineering | ✅ solved | 33 | 1,761,801 | 63,160 | $0.864 | 22.8 min |
| gcode-to-text | medium | file-operations | ✅ solved | 76 | 3,084,126 | 47,846 | $0.974 | 14.3 min |
| git-leak-recovery | medium | software-engineering | ✅ solved | 12 | 473,452 | 7,151 | $0.148 | 4.9 min |
| git-multibranch | medium | system-administration | ✅ solved | 33 | 1,584,684 | 20,825 | $0.466 | 4.4 min |
| gpt2-codegolf | hard | software-engineering | ❌ failed | 57 | 5,109,817 | 128,888 | $2.633 | 53.5 min |
| headless-terminal | medium | software-engineering | ✅ solved | 22 | 977,190 | 15,363 | $0.410 | 7.4 min |
| hf-model-inference | medium | data-science | ✅ solved | 7 | 254,085 | 5,762 | $0.085 | 1.9 min |
| install-windows-3.11 | hard | system-administration | ❌ failed | 41 | 2,296,949 | 31,986 | $0.680 | 7.1 min |
| kv-store-grpc | medium | software-engineering | ✅ solved | 12 | 480,425 | 7,239 | $0.139 | 2.1 min |
| large-scale-text-editing | medium | file-operations | ✅ solved | 18 | 761,039 | 9,874 | $0.379 | 8.5 min |
| largest-eigenval | medium | mathematics | ✅ solved | 18 | 771,732 | 11,251 | $0.355 | 5.2 min |
| llm-inference-batching-scheduler | hard | machine-learning | ✅ solved | 41 | 2,706,175 | 51,914 | $1.561 | 26.8 min |
| log-summary-date-ranges | medium | data-processing | ✅ solved | 8 | 302,756 | 6,793 | $0.106 | 2.7 min |
| mailman | medium | system-administration | ❌ failed | 163 | 17,616,302 | 117,016 | $5.423 | 56.5 min |
| make-doom-for-mips | hard | software-engineering | ⏱ timeout | 149 | 16,295,469 | 179,482 | $5.505 | 60.0 min |
| make-mips-interpreter | hard | software-engineering | ❌ failed | 177 | 19,611,308 | 201,572 | $6.868 | 72.0 min |
| mcmc-sampling-stan | hard | data-science | ✅ solved | 34 | 2,305,387 | 99,717 | $0.806 | 21.6 min |
| merge-diff-arc-agi-task | medium | debugging | ✅ solved | 31 | 1,482,844 | 17,610 | $0.562 | 10.7 min |
| model-extraction-relu-logits | hard | mathematics | ❌ failed | 14 | 608,883 | 15,478 | $0.519 | 8.3 min |
| modernize-scientific-stack | medium | scientific-computing | ✅ solved | 5 | 171,249 | 7,013 | $0.064 | 0.5 min |
| mteb-leaderboard | medium | data-science | ❌ failed | 134 | 9,863,579 | 565,731 | $4.007 | 109.2 min |
| mteb-retrieve | medium | data-science | ❌ failed | 6 | 210,717 | 5,444 | $0.067 | 1.4 min |
| multi-source-data-merger | medium | data-processing | ✅ solved | 8 | 302,525 | 7,448 | $0.137 | 2.1 min |
| nginx-request-logging | medium | system-administration | ✅ solved | 12 | 490,012 | 9,188 | $0.148 | 2.9 min |
| openssl-selfsigned-cert | medium | security | ❌ failed | 10 | 388,910 | 7,050 | $0.117 | 1.6 min |
| overfull-hbox | easy | debugging | ✅ solved | 77 | 4,616,543 | 34,611 | $1.537 | 17.2 min |
| password-recovery | hard | security | ✅ solved | 73 | 4,769,712 | 54,903 | $3.567 | 55.1 min |
| path-tracing | hard | software-engineering | ✅ solved | 194 | 18,483,361 | 205,361 | $7.453 | 108.5 min |
| path-tracing-reverse | hard | software-engineering | ✅ solved | 88 | 8,857,683 | 211,905 | $4.847 | 64.1 min |
| polyglot-c-py | medium | software-engineering | ❌ failed | 9 | 337,747 | 5,050 | $0.231 | 3.4 min |
| polyglot-rust-c | hard | software-engineering | ❌ failed | 12 | 471,616 | 5,765 | $0.746 | 13.1 min |
| portfolio-optimization | medium | optimization | ✅ solved | 34 | 1,844,659 | 29,258 | $0.740 | 24.4 min |
| protein-assembly | hard | scientific-computing | ❌ failed | 48 | 3,634,400 | 64,446 | $2.165 | 31.4 min |
| prove-plus-comm | easy | software-engineering | ✅ solved | 6 | 199,212 | 12,514 | $0.082 | 0.9 min |
| pypi-server | medium | software-engineering | ✅ solved | 13 | 518,027 | 6,917 | $0.140 | 2.3 min |
| pytorch-model-cli | medium | model-training | ✅ solved | 32 | 1,532,769 | 20,909 | $0.477 | 8.6 min |
| pytorch-model-recovery | medium | model-training | ✅ solved | 25 | 1,160,668 | 28,739 | $0.478 | 21.1 min |
| qemu-alpine-ssh | medium | system-administration | ✅ solved | 117 | 8,055,246 | 58,224 | $2.202 | 53.7 min |
| qemu-startup | medium | system-administration | ✅ solved | 26 | 1,206,220 | 16,004 | $0.461 | 7.8 min |
| query-optimize | medium | data-science | ⏱ timeout | 21 | 913,741 | 12,105 | $0.355 | 11.5 min |
| raman-fitting | medium | scientific-computing | ❌ failed | 44 | 2,511,383 | 39,190 | $1.080 | 15.1 min |
| regex-chess | hard | software-engineering | ✅ solved | 47 | 4,397,843 | 272,504 | $2.387 | 38.5 min |
| regex-log | medium | data-processing | ✅ solved | 13 | 528,827 | 9,122 | $0.465 | 6.8 min |
| reshard-c4-data | medium | data-science | ✅ solved | 19 | 884,423 | 16,405 | $0.933 | 18.4 min |
| rstan-to-pystan | medium | data-science | ✅ solved | 81 | 5,262,916 | 309,910 | $2.127 | 85.6 min |
| sam-cell-seg | hard | data-science | ❌ failed | 80 | 5,940,570 | 78,345 | $2.379 | 42.5 min |
| sanitize-git-repo | medium | security | ✅ solved | 20 | 1,441,011 | 70,680 | $0.578 | 4.2 min |
| schemelike-metacircular-eval | medium | software-engineering | ⏱ timeout | 71 | 5,486,995 | 91,141 | $5.573 | 160.0 min |
| sparql-university | hard | data-querying | ✅ solved | 18 | 830,088 | 14,463 | $0.565 | 18.0 min |
| sqlite-db-truncate | medium | debugging | ✅ solved | 10 | 382,532 | 7,665 | $0.174 | 3.5 min |
| sqlite-with-gcov | medium | system-administration | ✅ solved | 46 | 2,370,426 | 26,391 | $0.626 | 14.3 min |
| torch-pipeline-parallelism | hard | software-engineering | ⏱ timeout | 13 | 533,963 | 28,213 | $0.836 | 17.8 min |
| torch-tensor-parallelism | hard | software-engineering | ✅ solved | 8 | 304,302 | 6,430 | $0.230 | 3.3 min |
| train-fasttext | hard | model-training | ❌ failed | 170 | 14,587,640 | 2,255,472 | $9.010 | 232.9 min |
| tune-mjcf | medium | scientific-computing | ⏱ timeout | 26 | 1,199,162 | 15,669 | $0.500 | 10.1 min |
| video-processing | hard | video-processing | ❌ failed | 61 | 3,802,030 | 47,396 | $1.333 | 15.6 min |
| vulnerable-secret | medium | security | ✅ solved | 17 | 720,487 | 13,460 | $0.279 | 3.3 min |
| winning-avg-corewars | medium | software-engineering | ✅ solved | 46 | 2,386,296 | 25,057 | $1.416 | 30.5 min |
| write-compressor | hard | software-engineering | ⏱ timeout | 7 | 259,271 | 4,424 | $0.520 | 60.0 min |
