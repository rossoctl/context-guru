# Iteration 020 — pre-registration: the merged design, run as specified

**Written before the run.** Iterations 014, 016 and 018 all measured a merged arm that was never
configured the way the design was measured good: the model was shown **1.02, then 2.63 candidates per
call** against the ~15 that produced 58% live-kept. This iteration fixes that and two other defects
found offline in [iteration 019](../iter019/results.md), then asks whether the mechanism behaves.

## Why the earlier arms did not test the design

| iteration | candidates/call | verdict on merged | why it does not stand |
|---|---|---|---|
| 014 | **1.02** | "declines to act", read as negative | prefix pre-filter removed 149,681 candidates; this is the per-output design refuted at 6% |
| 016 / 018 | 2.63 (018) | 94.6% keep | still far below bulk size; `merged_trim` 0–1 |

Offline, batch size proved to be a **yield/safety trade-off**, not a detail: at batch 3–6 the model
dropped a genuinely-spent output only **2/4** of the time; at batch 10 it dropped it **4/4** and
cleared **100%** of genuinely-spent candidates. Small batches do not make the model wrong, they make
it **unwilling to act** — which is exactly what a 94.6% keep rate looks like from the inside.

## The three changes

1. **`min_tokens` 3000 → 800** (`deploy/harbor/cfg-iter020-merged.yaml`) — config only, to reach a bulk-sized batch.
2. **The contract states the SPENT criterion and forces the evidence** (`internal/extract/bulk.go`) —
   spent only if needed by none of (a) the current step, (b) an unfinished user instruction, (c) a
   next step the agent itself stated; and the model must **name which** and **quote it verbatim**.
   Measured: stating the criterion alone was **inert** (4/4 false drops); requiring the evidence
   halved it. A `drop` that names an obligation is now **refused**.
3. **`trim` removed** — chosen 0 times in 21 offline opportunities; in production accepted once
   against eight rejected as invented. It was the only verdict requiring the model to transport text.

`mergedMaxItems` 15 → **12**: quote fidelity degraded with batch size (4 of 37 non-verbatim at 16
against 0 of 16 at 10), so the ceiling sits between them and this takes the conservative end.

**Deliberately NOT bundled:** reading the real context via a cache-read on the request model. It is
buildable and cheap (iter019 §2, full cache read, no write, ≈$0.03/call at 100k) but the capability it
was meant to enable — the Tier-2 veto — did not appear even with the window. One variable cluster at
a time.

## Scope: this is a MECHANISM run, not a reward claim

One arm cannot support a reward conclusion, and this pre-registration does not attempt one. Solves
will be reported as context only, and comparisons to iterations 014/016/018 are **indicative at
best** — different binaries, different expand behaviour. If the mechanism behaves, the reward pair
(baseline + merged, both on this binary) is iteration 021.

**Config:** `deploy/harbor/cfg-iter020-merged.yaml` (committed — the merged configs for
iterations 014/016/018 were not, which made them unreproducible), 128k band, `aws/claude-sonnet-5`, cheap model `aws/claude-haiku-4-5`,
LOCA clearing at 128k, `INJECT_EXPAND=always`, n=75. Expected ~$230.

## Primary endpoint (mechanism)

**Candidates per call ≥ 8**, computed as (sum of verdict gates) / `extract.calls`. Below that the arm
has again not run the design, and no other number from it means anything.

## Pre-registered readings

Written before the numbers exist.

| outcome | conclusion | next |
|---|---|---|
| batch ≥8 **and** unique-token yield rises materially over iter018 | small batches were the cause of "declines to act"; the design does act when asked properly | iteration 021: the reward pair |
| batch ≥8 but yield flat | willingness was not the constraint; the negative answer survives, now fairly earned | close merged; keep the deterministic index |
| batch <8 | `min_tokens` still too high, or the economic gate is suppressing small candidates | diagnose, do not interpret anything else |
| `merged_drop_contradicts_obligation` non-trivial | the model asserts an obligation then drops anyway; the coherence guard is load-bearing and the prompt needs work | report the rate; it is a safety finding either way |
| `merged_quote_not_verbatim` > 10% of quotes | batch is above this model's transport ceiling | lower `mergedMaxItems` and re-run |
| `merged_drop_unjustified` dominant | the model is skipping the criterion field, so the forcing function is not running | prompt defect, not a design result |

## Checkpoints during the run

Per the working pattern established earlier: **stop and look, do not wait for the end.**

* **~200 requests:** candidates/call, `merged_batch_truncated`, `merged_drop_unjustified`. If
  candidates/call < 6, **abort** and lower `min_tokens` again — finishing a starved arm wastes ~$230
  and produces another uninterpretable iteration.
* **~600 requests:** `merged_quote_not_verbatim` share, drop rate, `summarize` firing rate.
* **Throughout:** one proxy only, bound port verified — the stale-proxy bug in iteration 013
  invalidated two arms by running a previous arm's pipeline on a reused port.

## Secondary endpoints

* **Deferral:** `summarize.acted / requests`, against iter018's 56.1%. Deferral follows removed mass,
  so it should move with yield — and it is **not** evidence that the removals were correct.
* **Yield in unique tokens**, not reported savings: frozen replays re-credit the same removal, and the
  overcount ran 31.7× in iteration 014.
* **Expand:** `expand_restored`, `expand_unresolved_missing`, repeat rate. More removal should mean
  more recovery traffic; unresolved must stay 0.
* **CG spend**, which is attributable in a way total LOCA cost is not (iteration 012).

## Verification before launch

* `CGO_ENABLED=1 go build ./...` and the full test suite green; each new guard confirmed to FAIL when
  its subject is reverted (done: coherence check, quote check, trim degradation).
* Benchmark traffic on `$ANTHROPIC_BENCHMARK_BASE_URL` with `ANTHROPIC_CUSTOM_HEADERS=` — never
  through Context Guru.
* Shim `repairs` and HTML 400 count checked in the first minutes.

## Known limits, stated up front

Everything motivating this iteration is **decision quality on one synthetic transcript**, n=4 per arm,
with ground truth the author judged — see iter019 §8. It cannot speak to reward, which is why this run
is scoped to mechanism. The offline probes also say the Tier-2 veto remains unreliable (kept 50–100%
of the time depending on batch size and signal clarity, against 0% for the old prompt), so a rise in
yield here should be read as *more willingness to act*, not as *better discrimination*.
