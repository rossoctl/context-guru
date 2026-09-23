# SWE-bench Verified: per-arm detail

These are the per-arm result breakdowns for the SWE-bench Verified run: baseline, context-guru, headroom, rtk, and observe mode.

## Full results — baseline (off) (SWE-bench Verified, 50 tasks)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| tasks scored | solved | rate | total billed cost | mean steps | cache-hit | agent wall (sum) |
|---|---|---|---|---|---|---|
| 50 | 43 | 86% | $31.98 | 36.1 | 98.1% | 317 min |

### Per-task

| task | reward | steps | cache_read | cache_write | billed cost |
|---|---|---|---|---|---|
| astropy__astropy-12907 | 1 | 28 | 1,486,182 | 39,072 | $0.443 |
| astropy__astropy-14365 | 1 | 26 | 1,331,718 | 30,002 | $0.428 |
| astropy__astropy-8707 | 0 | 26 | 1,287,520 | 30,970 | $0.391 |
| django__django-11095 | 1 | 31 | 1,446,457 | 28,197 | $0.422 |
| django__django-11211 | 1 | 57 | 3,336,980 | 75,407 | $0.999 |
| django__django-11477 | 1 | 56 | 3,023,951 | 38,688 | $0.881 |
| django__django-11790 | 1 | 25 | 1,131,935 | 24,533 | $0.337 |
| django__django-12050 | 1 | 7 | 241,379 | 13,657 | $0.090 |
| django__django-12308 | 1 | 25 | 1,169,638 | 22,221 | $0.347 |
| django__django-12858 | 0 | 42 | 2,212,083 | 39,423 | $0.789 |
| django__django-13128 | 1 | 123 | 10,152,118 | 212,282 | $2.985 |
| django__django-13363 | 1 | 45 | 2,586,634 | 45,535 | $0.759 |
| django__django-13568 | 1 | 32 | 1,522,384 | 26,424 | $0.438 |
| django__django-13810 | 1 | 31 | 1,783,070 | 44,681 | $0.780 |
| django__django-14034 | 1 | 84 | 5,832,079 | 59,733 | $1.794 |
| django__django-14349 | 1 | 14 | 593,392 | 22,577 | $0.216 |
| django__django-14559 | 1 | 25 | 1,164,866 | 29,554 | $0.352 |
| django__django-14792 | 0 | 69 | 4,396,102 | 52,915 | $1.184 |
| django__django-15128 | 1 | 59 | 3,602,051 | 45,027 | $0.988 |
| django__django-15380 | 1 | 21 | 966,153 | 24,392 | $0.295 |
| django__django-15572 | 1 | 10 | 385,788 | 17,396 | $0.139 |
| django__django-15930 | 1 | 34 | 1,716,231 | 29,791 | $0.533 |
| django__django-16145 | 1 | 13 | 516,372 | 16,664 | $0.165 |
| django__django-16502 | 0 | 42 | 2,120,703 | 32,030 | $0.645 |
| django__django-16667 | 0 | 22 | 971,978 | 22,685 | $0.285 |
| django__django-17087 | 1 | 11 | 420,513 | 14,777 | $0.144 |
| matplotlib__matplotlib-22719 | 1 | 27 | 1,315,560 | 27,939 | $0.395 |
| matplotlib__matplotlib-24570 | 1 | 20 | 892,408 | 25,106 | $0.322 |
| matplotlib__matplotlib-25775 | 1 | 108 | 8,921,304 | 79,696 | $2.271 |
| psf__requests-1142 | 1 | 13 | 521,493 | 19,474 | $0.181 |
| pydata__xarray-3151 | 1 | 22 | 1,004,803 | 24,540 | $0.318 |
| pydata__xarray-4966 | 1 | 30 | 1,472,856 | 28,726 | $0.453 |
| pylint-dev__pylint-4551 | 1 | 72 | 5,482,438 | 75,674 | $1.682 |
| pytest-dev__pytest-10051 | 1 | 23 | 1,003,515 | 19,024 | $0.289 |
| pytest-dev__pytest-7205 | 1 | 49 | 2,650,728 | 36,370 | $0.717 |
| scikit-learn__scikit-learn-10844 | 1 | 15 | 620,269 | 17,574 | $0.193 |
| scikit-learn__scikit-learn-13328 | 1 | 17 | 722,414 | 20,964 | $0.226 |
| scikit-learn__scikit-learn-14894 | 1 | 21 | 915,577 | 20,958 | $0.289 |
| scikit-learn__scikit-learn-9288 | 1 | 13 | 547,195 | 20,357 | $0.197 |
| sphinx-doc__sphinx-7454 | 1 | 22 | 1,028,492 | 25,064 | $0.314 |
| sphinx-doc__sphinx-8120 | 1 | 32 | 1,588,891 | 28,644 | $0.464 |
| sphinx-doc__sphinx-8638 | 1 | 71 | 3,217,605 | 82,483 | $1.141 |
| sphinx-doc__sphinx-9602 | 1 | 32 | 1,738,898 | 36,151 | $0.526 |
| sympy__sympy-13031 | 1 | 67 | 3,977,995 | 45,516 | $1.234 |
| sympy__sympy-13877 | 1 | 12 | 497,367 | 20,055 | $0.174 |
| sympy__sympy-15599 | 1 | 37 | 1,773,674 | 30,519 | $0.598 |
| sympy__sympy-17318 | 1 | 50 | 2,656,656 | 33,634 | $0.776 |
| sympy__sympy-19495 | 0 | 39 | 2,119,352 | 38,337 | $0.938 |
| sympy__sympy-21379 | 1 | 24 | 1,152,459 | 25,642 | $0.384 |
| sympy__sympy-23413 | 0 | 31 | 1,614,387 | 33,585 | $1.073 |

## Full results — context-guru codesmart (final) (SWE-bench Verified, 50 tasks)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| tasks scored | solved | rate | total billed cost | mean steps | cache-hit | agent wall (sum) |
|---|---|---|---|---|---|---|
| 50 | 44 | 88% | $27.77 | 31.1 | 97.7% | 293 min |

Context-guru proxy savings: **1.09%** content; own LLM cost $0.3071; added latency/req 116.9 ms; expand bounces 4.

Per-component tokens removed (cumulative): `extract_llm` 129,966, `extract` 34,293, `dedup` 1,120, `cmdfilter` 6

The pipeline that produced these numbers is not today's `codesmart`: it had no `toon`, used
`cacheinject` rather than `cachesplit`, and `failed_run` removed nothing because of a gating
bug (since fixed), which is why it is absent from the per-component line above. See
[Reproduce the results](REPRODUCE.md) for what a re-run today would execute.

### Per-task

| task | reward | steps | cache_read | cache_write | billed cost |
|---|---|---|---|---|---|
| astropy__astropy-12907 | 1 | 18 | 812,765 | 24,714 | $0.266 |
| astropy__astropy-14365 | 1 | 29 | 1,532,573 | 82,934 | $0.620 |
| astropy__astropy-8707 | 0 | 30 | 1,541,762 | 35,675 | $0.465 |
| django__django-11095 | 1 | 30 | 1,361,204 | 25,666 | $0.390 |
| django__django-11211 | 1 | 58 | 3,337,926 | 45,786 | $0.998 |
| django__django-11477 | 1 | 59 | 3,221,584 | 40,012 | $0.884 |
| django__django-11790 | 1 | 19 | 825,013 | 20,383 | $0.265 |
| django__django-12050 | 1 | 14 | 557,421 | 16,707 | $0.177 |
| django__django-12308 | 1 | 21 | 947,748 | 26,217 | $0.295 |
| django__django-12858 | 0 | 41 | 2,351,245 | 40,212 | $0.868 |
| django__django-13128 | 1 | 55 | 3,195,695 | 125,822 | $1.293 |
| django__django-13363 | 1 | 32 | 1,724,523 | 39,041 | $0.512 |
| django__django-13568 | 1 | 19 | 838,602 | 22,532 | $0.260 |
| django__django-13810 | 1 | 22 | 1,145,425 | 35,025 | $0.383 |
| django__django-14034 | 0 | 28 | 1,312,390 | 22,972 | $0.436 |
| django__django-14349 | 1 | 14 | 584,640 | 20,203 | $0.200 |
| django__django-14559 | 1 | 26 | 1,244,486 | 31,570 | $0.392 |
| django__django-14792 | 1 | 31 | 1,701,547 | 63,420 | $0.634 |
| django__django-15128 | 1 | 68 | 4,153,321 | 99,729 | $1.285 |
| django__django-15380 | 1 | 31 | 1,559,464 | 31,702 | $0.471 |
| django__django-15572 | 1 | 10 | 386,438 | 17,167 | $0.137 |
| django__django-15930 | 0 | 29 | 1,451,398 | 29,675 | $0.506 |
| django__django-16145 | 1 | 16 | 665,178 | 18,656 | $0.204 |
| django__django-16502 | 1 | 26 | 1,276,076 | 31,724 | $0.402 |
| django__django-16667 | 0 | 15 | 639,473 | 19,408 | $0.198 |
| django__django-17087 | 1 | 26 | 1,202,756 | 26,076 | $0.353 |
| matplotlib__matplotlib-22719 | 1 | 21 | 971,975 | 44,131 | $0.348 |
| matplotlib__matplotlib-24570 | 1 | 18 | 787,640 | 20,578 | $0.274 |
| matplotlib__matplotlib-25775 | 1 | 111 | 9,521,141 | 82,832 | $2.413 |
| psf__requests-1142 | 1 | 13 | 514,299 | 18,301 | $0.173 |
| pydata__xarray-3151 | 1 | 18 | 789,546 | 23,415 | $0.251 |
| pydata__xarray-4966 | 1 | 22 | 1,030,230 | 26,080 | $0.333 |
| pylint-dev__pylint-4551 | 1 | 33 | 1,967,397 | 38,806 | $0.623 |
| pytest-dev__pytest-10051 | 1 | 11 | 435,674 | 17,052 | $0.151 |
| pytest-dev__pytest-7205 | 1 | 44 | 2,295,595 | 36,023 | $0.657 |
| scikit-learn__scikit-learn-10844 | 1 | 11 | 426,716 | 15,193 | $0.139 |
| scikit-learn__scikit-learn-13328 | 1 | 14 | 575,907 | 17,453 | $0.181 |
| scikit-learn__scikit-learn-14894 | 1 | 17 | 715,414 | 19,037 | $0.233 |
| scikit-learn__scikit-learn-9288 | 1 | 7 | 256,371 | 17,174 | $0.123 |
| sphinx-doc__sphinx-7454 | 1 | 34 | 1,770,316 | 33,581 | $0.517 |
| sphinx-doc__sphinx-8120 | 1 | 40 | 1,875,120 | 59,078 | $0.627 |
| sphinx-doc__sphinx-8638 | 1 | 53 | 3,228,610 | 48,149 | $0.918 |
| sphinx-doc__sphinx-9602 | 1 | 48 | 3,025,217 | 49,905 | $1.035 |
| sympy__sympy-13031 | 1 | 42 | 2,216,165 | 35,655 | $0.713 |
| sympy__sympy-13877 | 0 | 17 | 780,305 | 46,381 | $0.368 |
| sympy__sympy-15599 | 1 | 59 | 3,017,715 | 22,004 | $0.845 |
| sympy__sympy-17318 | 1 | 44 | 2,337,329 | 33,639 | $0.720 |
| sympy__sympy-19495 | 1 | 47 | 2,725,766 | 41,171 | $1.351 |
| sympy__sympy-21379 | 1 | 29 | 1,420,186 | 26,175 | $0.458 |
| sympy__sympy-23413 | 1 | 37 | 2,261,138 | 82,294 | $1.429 |

## Full results — headroom (hd-cache) (SWE-bench Verified, 50 tasks)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| tasks scored | solved | rate | total billed cost | mean steps | cache-hit | agent wall (sum) |
|---|---|---|---|---|---|---|
| 50 | 40 | 80% | $30.30 | 35.1 | 98.0% | 304 min |

### Per-task

| task | reward | steps | cache_read | cache_write | billed cost |
|---|---|---|---|---|---|
| astropy__astropy-12907 | 1 | 18 | 748,919 | 57,946 | $0.337 |
| astropy__astropy-14365 | 1 | 26 | 1,272,402 | 58,903 | $0.476 |
| astropy__astropy-8707 | 0 | 69 | 4,138,129 | 51,704 | $1.107 |
| django__django-11095 | 1 | 31 | 1,345,510 | 25,951 | $0.407 |
| django__django-11211 | 1 | 88 | 5,431,961 | 60,005 | $1.474 |
| django__django-11477 | 1 | 67 | 4,107,126 | 51,990 | $1.150 |
| django__django-11790 | 1 | 21 | 861,970 | 20,021 | $0.266 |
| django__django-12050 | 1 | 15 | 779,835 | 36,263 | $0.268 |
| django__django-12308 | 1 | 25 | 1,215,734 | 32,452 | $0.366 |
| django__django-12858 | 0 | 46 | 2,429,734 | 40,825 | $0.827 |
| django__django-13128 | 1 | 44 | 2,494,094 | 119,734 | $1.042 |
| django__django-13363 | 1 | 37 | 2,141,441 | 43,113 | $0.637 |
| django__django-13568 | 1 | 21 | 874,455 | 22,660 | $0.270 |
| django__django-13810 | 1 | 25 | 1,331,718 | 40,535 | $0.456 |
| django__django-14034 | 0 | 17 | 728,678 | 21,269 | $0.276 |
| django__django-14349 | 1 | 9 | 326,266 | 18,376 | $0.128 |
| django__django-14559 | 1 | 25 | 1,082,511 | 28,069 | $0.331 |
| django__django-14792 | 0 | 60 | 3,626,095 | 41,427 | $1.067 |
| django__django-15128 | 1 | 43 | 2,294,587 | 39,154 | $0.657 |
| django__django-15380 | 1 | 26 | 1,208,799 | 28,913 | $0.369 |
| django__django-15572 | 1 | 14 | 558,435 | 19,735 | $0.189 |
| django__django-15930 | 1 | 28 | 1,295,892 | 26,081 | $0.399 |
| django__django-16145 | 1 | 24 | 1,028,867 | 22,790 | $0.312 |
| django__django-16502 | 0 | 38 | 1,916,315 | 30,526 | $0.604 |
| django__django-16667 | 0 | 15 | 602,641 | 19,496 | $0.191 |
| django__django-17087 | 1 | 20 | 828,058 | 21,272 | $0.253 |
| matplotlib__matplotlib-22719 | 1 | 28 | 1,338,523 | 28,324 | $0.407 |
| matplotlib__matplotlib-24570 | 1 | 22 | 961,212 | 21,954 | $0.364 |
| matplotlib__matplotlib-25775 | 1 | 135 | 13,265,857 | 104,983 | $3.485 |
| psf__requests-1142 | 1 | 14 | 541,350 | 19,393 | $0.190 |
| pydata__xarray-3151 | 1 | 19 | 824,871 | 24,550 | $0.270 |
| pydata__xarray-4966 | 1 | 25 | 1,195,388 | 31,643 | $0.380 |
| pylint-dev__pylint-4551 | 1 | 45 | 2,801,827 | 52,055 | $0.861 |
| pytest-dev__pytest-10051 | 1 | 17 | 682,749 | 17,574 | $0.213 |
| pytest-dev__pytest-7205 | 1 | 48 | 2,502,813 | 39,914 | $0.752 |
| scikit-learn__scikit-learn-10844 | 1 | 10 | 363,542 | 15,746 | $0.130 |
| scikit-learn__scikit-learn-13328 | 1 | 21 | 866,537 | 20,972 | $0.267 |
| scikit-learn__scikit-learn-14894 | 1 | 20 | 828,462 | 20,742 | $0.271 |
| scikit-learn__scikit-learn-9288 | 1 | 30 | 1,476,615 | 31,687 | $0.476 |
| sphinx-doc__sphinx-7454 | 1 | 26 | 1,287,627 | 32,187 | $0.406 |
| sphinx-doc__sphinx-8120 | 1 | 74 | 4,375,724 | 52,043 | $1.231 |
| sphinx-doc__sphinx-8638 | 0 | 32 | 1,068,489 | 56,612 | $0.543 |
| sphinx-doc__sphinx-9602 | 0 | 56 | 3,376,186 | 45,137 | $1.036 |
| sympy__sympy-13031 | 1 | 42 | 1,924,725 | 30,000 | $0.727 |
| sympy__sympy-13877 | 1 | 16 | 649,900 | 49,201 | $0.302 |
| sympy__sympy-15599 | 1 | 45 | 2,261,362 | 36,297 | $0.718 |
| sympy__sympy-17318 | 1 | 75 | 4,015,695 | 38,471 | $1.127 |
| sympy__sympy-19495 | 1 | 45 | 2,311,024 | 33,835 | $0.818 |
| sympy__sympy-21379 | 0 | 26 | 1,164,192 | 23,205 | $0.422 |
| sympy__sympy-23413 | 0 | 32 | 1,645,502 | 33,325 | $1.046 |

## Full results — rtk (Rust Token Killer) (SWE-bench Verified, 50 tasks)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

### Totals

| tasks scored | solved | rate | total billed cost | mean steps | cache-hit | agent wall (sum) |
|---|---|---|---|---|---|---|
| 50 | 43 | 86% | $29.09 | 33.2 | 97.9% | 320 min |

**vs the no-compaction baseline** ($31.98 / 43 solved / 36.1 steps): rtk is **−9.0%**
billed cost, **reward-neutral** (43 = 43), **−8%** steps, at **zero request-path latency**
and **$0 tool cost**. It is the 2nd-cheapest of the four arms — cheaper than headroom
(−5.3%), behind context-guru (−13.2%). Full four-way table: [comparison.md](comparison.md).

### What rtk did (its own ledger)

rtk is a Claude Code `PreToolUse` hook that rewrites Bash commands in-container
(`pytest`→`rtk pytest`, `cat`→`rtk read`, `git status`→`rtk git status`, …) and compresses
the command output **at the shell, before it enters the transcript**. Because the compressed
form is what gets cached from the first turn, rtk **never mutates already-cached content** —
it sidesteps cache invalidation by construction (lowest cache-write of the four arms, 1.83M).

Across the 50 clean trials it rewrote **637 bash commands** and removed **338k bash-output
tokens (65.8% of bash output**, its own `bytes/4` estimate). Note the denominator: that
65.8% is of *bash output only* — a small slice of a ~98%-cached agent's total context —
which is why the end-to-end saving is −9% billed cost, not −66%. (This is rtk's own
documented caveat: it cuts bash output, not the bill, one-to-one.)

| rtk command | invocations | ~bash tokens removed | share |
|---|--:|--:|--:|
| `rtk read` (`cat`) | 39 | ~209,000 | 62% |
| `rtk grep` | 285 | ~80,400 | 24% |
| `rtk git` (status/diff/stash) | 134 | ~20,800 | 6% |
| `rtk ls` | 44 | ~12,600 | 4% |
| `rtk pytest` | 57 | ~12,000 | 4% |
| `rtk diff` / `pip` / `wc` / `find` / `curl` | 81 | ~3,100 | 1% |

Savings concentrate in **file reads via `cat`** (62%) and **`grep`** (24%, the most-invoked
command). Claude Code's built-in `Read`/`Grep`/`Glob` tools bypass the Bash hook, so rtk only
sees output the agent routed through Bash — this is the structural ceiling on its reach.

#### Real before → after (rtk is deterministic; these reproduce its in-container behavior)

**`cat` a source file → `rtk read -l aggressive`** (332 B → 164 B): keeps imports + every
signature, elides bodies.
```
import os
import sys
def alpha(a, b):
    // ... implementation
def beta(x):
    // ... implementation
class Widget:
    def render(self):
    // ... implementation
```

**a failing `pytest` run → `rtk test`** (1,055 B → 195 B, ~81%): failures + summary kept,
213 passing lines dropped.
```
[FAIL] FAILURES:
  FAILED tests/test_utils.py::test_parse_edge_case - AssertionError: assert None...
SUMMARY:
  ======================== 1 failed, 213 passed in 4.21s =========================
```

### Per-task

| task | reward | steps | cache_read | cache_write | billed cost |
|---|---|---|---|---|---|
| astropy__astropy-12907 | 1 | 14 | 616,309 | 8,012 | $0.180 |
| astropy__astropy-14365 | 1 | 31 | 1,761,659 | 46,069 | $0.590 |
| astropy__astropy-8707 | 0 | 65 | 3,821,411 | 48,206 | $1.039 |
| django__django-11095 | 1 | 27 | 1,230,383 | 26,847 | $0.357 |
| django__django-11211 | 1 | 48 | 2,844,253 | 45,130 | $0.865 |
| django__django-11477 | 1 | 41 | 2,111,490 | 32,771 | $0.634 |
| django__django-11790 | 1 | 21 | 971,393 | 16,828 | $0.280 |
| django__django-12050 | 1 | 14 | 571,174 | 17,935 | $0.185 |
| django__django-12308 | 1 | 23 | 1,177,172 | 32,079 | $0.355 |
| django__django-12858 | 1 | 51 | 2,876,052 | 39,900 | $0.903 |
| django__django-13128 | 1 | 51 | 3,066,423 | 75,346 | $1.005 |
| django__django-13363 | 1 | 16 | 791,803 | 32,944 | $0.281 |
| django__django-13568 | 1 | 25 | 1,137,721 | 26,294 | $0.349 |
| django__django-13810 | 1 | 31 | 1,771,868 | 40,978 | $0.534 |
| django__django-14034 | 0 | 25 | 1,141,289 | 59,052 | $0.501 |
| django__django-14349 | 1 | 22 | 992,170 | 24,206 | $0.306 |
| django__django-14559 | 1 | 25 | 1,162,737 | 29,116 | $0.344 |
| django__django-14792 | 1 | 23 | 1,195,900 | 26,221 | $0.381 |
| django__django-15128 | 1 | 51 | 3,252,766 | 197,694 | $1.302 |
| django__django-15380 | 1 | 25 | 1,187,466 | 26,767 | $0.367 |
| django__django-15572 | 1 | 19 | 832,903 | 22,241 | $0.275 |
| django__django-15930 | 1 | 39 | 2,047,139 | 32,769 | $0.604 |
| django__django-16145 | 1 | 29 | 1,404,848 | 27,753 | $0.431 |
| django__django-16502 | 1 | 51 | 2,861,378 | 39,610 | $0.853 |
| django__django-16667 | 0 | 13 | 524,431 | 17,518 | $0.166 |
| django__django-17087 | 1 | 23 | 1,018,514 | 22,426 | $0.298 |
| matplotlib__matplotlib-22719 | 1 | 33 | 1,675,521 | 29,577 | $0.486 |
| matplotlib__matplotlib-24570 | 1 | 17 | 758,768 | 21,599 | $0.287 |
| matplotlib__matplotlib-25775 | 1 | 102 | 7,436,574 | 67,271 | $1.968 |
| psf__requests-1142 | 1 | 15 | 626,137 | 20,165 | $0.213 |
| pydata__xarray-3151 | 1 | 25 | 1,202,343 | 27,163 | $0.359 |
| pydata__xarray-4966 | 1 | 30 | 1,562,847 | 34,090 | $0.475 |
| pylint-dev__pylint-4551 | 1 | 75 | 5,995,589 | 74,292 | $1.614 |
| pytest-dev__pytest-10051 | 1 | 15 | 628,585 | 18,041 | $0.202 |
| pytest-dev__pytest-7205 | 1 | 46 | 2,494,414 | 38,266 | $0.713 |
| scikit-learn__scikit-learn-10844 | 1 | 6 | 207,791 | 15,094 | $0.090 |
| scikit-learn__scikit-learn-13328 | 1 | 16 | 683,722 | 21,121 | $0.223 |
| scikit-learn__scikit-learn-14894 | 1 | 21 | 915,359 | 19,868 | $0.278 |
| scikit-learn__scikit-learn-9288 | 1 | 18 | 816,867 | 56,210 | $0.361 |
| sphinx-doc__sphinx-7454 | 1 | 7 | 266,124 | 20,532 | $0.120 |
| sphinx-doc__sphinx-8120 | 1 | 80 | 4,876,037 | 54,263 | $1.384 |
| sphinx-doc__sphinx-8638 | 0 | 50 | 2,855,816 | 40,367 | $0.920 |
| sphinx-doc__sphinx-9602 | 0 | 10 | 390,339 | 18,665 | $0.184 |
| sympy__sympy-13031 | 0 | 17 | 737,132 | 20,161 | $0.292 |
| sympy__sympy-13877 | 1 | 22 | 1,114,720 | 32,317 | $0.412 |
| sympy__sympy-15599 | 1 | 73 | 4,190,786 | 43,343 | $1.209 |
| sympy__sympy-17318 | 1 | 52 | 2,768,890 | 31,688 | $0.805 |
| sympy__sympy-19495 | 1 | 54 | 3,092,829 | 42,633 | $1.368 |
| sympy__sympy-21379 | 1 | 43 | 2,317,605 | 36,288 | $0.677 |
| sympy__sympy-23413 | 0 | 32 | 1,683,504 | 37,181 | $1.063 |

## Results — observe mode

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`, `codesmart`
pipeline, cache-aware billed cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M ·
output $10/M) recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

**Scale caveat, stated up front.** 2 SWE-bench tasks and 2 Terminal-Bench tasks per mode at
n=1, plus one real Claude Code session per mode. Enough to answer the two questions observe
mode has to answer — does it add latency, and do its projections mean anything — and
nowhere near enough for a cost or solve-rate claim. The 50-task arms in the other results
pages are the ones to cite for savings.

### Does observe add latency to the enforced path?

**No.**

| | SWE-bench | Terminal-Bench | Live Claude Code session |
|---|---|---|---|
| `sync` added latency / req | 1,599.4 ms | 26.9 ms | 28.964 ms |
| **`observe` added latency / req** | **0.062 ms** | **0.076 ms** | **0.209 ms** |

Four orders of magnitude on SWE-bench, and it is structural rather than tuned: the request
path never runs the pipeline, so the only cost is copying the body and an enqueue.

Observe is not *free* in other respects — it moved 75.0 s of compaction off-path on
SWE-bench and spent $0.0779 of cheap-model tokens doing the measuring. It costs money and
CPU, just not request latency, and `observe_llm_notice` labels that spend for what it is.

### Do observe's projections match what sync actually achieved?

This is the question that validates the mode. Three independent lines of evidence, in
descending order of strength:

#### 1. Controlled same-traffic comparison — exact agreement

The same five turns driven through the real handler under each mode:

```
sync:    before=43445  saved=10020  (23.06%)
observe: baseline=43445 potential=10020 (23.06%)
```

Identical. A test pins this, and it fails at ratio 0.33 if observe's own store is removed.

#### 2. Terminal-Bench — correct agreement near zero (negative control)

| | `sync` | `observe` |
|---|---|---|
| content savings (enforced) | 1.02% | — (0 by construction) |
| projected savings | — | **0%** |
| added latency / req | 26.9 ms | 0.076 ms |
| enforced requests | 60 | **0** |

On traffic where sync achieves almost nothing, observe correctly projects almost nothing
rather than inventing a headline. This is the more convincing shape of the evidence: a mode
that only ever agreed on high-savings traffic would be much weaker proof that its
projections mean anything. It also correctly reported the overhead sync *would* have added
as 9.1 ms/req — small here because the pipeline made no model calls on these tasks.

#### 3. SWE-bench arms — consistent, but too noisy to confirm independently

6.40% projected against 0.82% enforced. The gap is **not** explained away:

- the arms are different agent trajectories (22.5 vs 15.5 mean steps) — observe saw 46
  requests and 492,652 baseline tokens, sync saw 35 and 244,319. These are not the same
  conversations.
- observe's projection never pays a bounce. Nothing is offloaded, so no `expand` round trip
  can claw savings back and `wasted_tokens` is structurally 0. Under `sync` some savings do
  come back. Observe's projection is an **upper bound** on content savings, documented as
  one.
- 2 tasks at n=1 cannot separate a real bias from trajectory noise.

A 50-task paired run is the honest next step for this line specifically.

#### Two bugs this question found

Answering it honestly was the most valuable thing the benchmark did, because the first
comparison was wrong twice, in opposite directions:

1. **11x overstatement.** The observe job ran without the session tracker, so its
   cached-prefix boundary was unknown, the tail gate never fired, and 50 `extract_llm`
   candidates passed where sync allowed 5 — 9.53% projected against 0.82% enforced. A
   projection that ignores cache-awareness projects what a *cache-blind* proxy would do and
   overstates by exactly what cache-awareness costs.
2. **3x understatement.** Fixing that exposed the opposite error: observe ran against a
   discarded buffer and so lost the frozen decisions offloaders replay on every later turn
   — where most of the sustained saving lives.

Both are fixed, both have a test that fails without its fix, and the exact agreement in §1
is the result.

### Namespace separation, verified in production

From the SWE-bench observe arm's live `/stats`:

- **enforced:** `requests: 0`, `saved_tokens: 0`, `sync_enforced: 0`, `components: {}` —
  all zero, all empty;
- **hypothetical:** `observe_hypothetical_requests: 46`,
  `actual_baseline_tokens: 492652`, `projected_optimized_tokens: 461112`,
  `potential_saved_tokens: 31540`, `potential_components: {…}` — fully populated.

No aggregate over the enforced savings rollups can reach a hypothetical, because they are
different accumulators with disjoint serialized names.

### Live Claude Code sessions

Same prompt and workspace through each mode against the live gateway:

| | `sync` | `observe` |
|---|---|---|
| requests (enforced) | 4 | **0** |
| `sync_enforced` | 4 | 0 |
| added latency / req | 28.964 ms | **0.209 ms** |
| baseline tokens | 6,025 | 6,025 *(as `actual_baseline_tokens`)* |
| task answered correctly | yes | yes |

Observe's `actual_baseline_tokens` = 6,025 is *exactly* sync's `tokens_before` = 6,025 on
the same prompt — the hypothetical namespace accounts for identical traffic identically,
measured independently. And `requests: 0` with every enforced savings aggregate at zero is
the machine-readable form of "context-guru did not modify anything".

### What is not established here

- Any cost or solve-rate claim per mode. 2 tasks at n=1; the billed-cost figures track
  trajectory length far more than they track mode.
- Whether observe's projection matches sync on *large-savings* traffic at scale. §1 shows
  exact agreement on controlled traffic and §2 correct agreement near zero; the SWE-bench
  arms are too small and too differently-shaped to confirm the middle of that range.
- The off-path queue under pressure: `dropped` was 0 on every arm, so that path is
  exercised only by tests, never yet by production load.
