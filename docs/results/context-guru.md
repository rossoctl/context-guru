# Full results — context-guru codesmart (final) (SWE-bench Verified, 50 tasks)

Live through the harness, `claude-code` agent on `aws/claude-sonnet-5`. Cache-aware billed input cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M) + output $10/M, recomputed from each trial's token tiers. See [REPRODUCE.md](REPRODUCE.md).

## Totals

| tasks scored | solved | rate | total billed cost | mean steps | cache-hit | agent wall (sum) |
|---|---|---|---|---|---|---|
| 50 | 44 | 88% | $27.77 | 31.1 | 97.7% | 293 min |

Context-guru proxy savings: **1.09%** content; own LLM cost $0.3071; added latency/req 116.9 ms; expand bounces 4.

Per-component tokens removed (cumulative): `extract_llm` 129,966, `extract` 34,293, `dedup` 1,120, `cmdfilter` 6

The pipeline that produced these numbers is not today's `codesmart`: it had no `toon`, used
`cacheinject` rather than `cachesplit`, and `failed_run` removed nothing because of a gating
bug (since fixed), which is why it is absent from the per-component line above. See
[Reproduce the results](REPRODUCE.md) for what a re-run today would execute.

## Per-task

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
