# Proposal: a deployment gate

**Status:** proposal. Nothing here is built yet.

**Problem:** commits and reviews are increasingly machine-authored, humans steer rather than
read every diff, and there is no mechanism that says "this build is not worse than the last
one" before it reaches `/usr/local/bin/context-guru-proxy`. CI proves the code compiles, the
tests pass, and the binary starts. It proves nothing about the two properties the product is
*for*.

## 1. What has to be true before a deploy

Three independent axes. They fail differently, they cost different amounts to check, and only
one of them is expensive.

| axis | the regression | what covers it today |
|---|---|---|
| **Configuration** | a default, preset, env var, route or auth class changes — or the *deployed* config stops matching what the binary reads | `pipeline_drift_test.go` (2 preset arms), `config/docdrift_test.go` (preset doc tables). Nothing checks the deployed host. |
| **Expensiveness** | savings shrink, or cost *rises* while content falls (cache-write growth, our own LLM spend) | nothing automated |
| **Capability** | the agent solves fewer tasks — corrupted `tool_result`, a broken expand round trip, a cache-busting splice | nothing automated |

The configuration axis is the one that has already bitten this deployment, twice, in ways no
test could see:

- `key_env` on an upstream entry decides who pays, and it **wins over the code**. A
  deployment whose `upstreams.yaml` still names it keeps billing the operator no matter what
  the binary does. `REDEPLOY-caller-pays.md` §"Why this is a runbook and not a script" exists
  because of that.
- `TENANT_MONTHLY_CAP_USD` stayed in the systemd unit after the binary stopped reading it — a
  live-looking spend control silently doing nothing.

Both are "the deployed configuration and the binary's actual config surface disagree". That
class is free to check, and is where the first work should go.

A crude inventory of the current surface, taken while writing this: the tree reads **58**
environment variables, **38** of them operator-facing (excluding `CG_TEST_*`, `CG_DIAG*`, the
capture/child plumbing and `CONTEXT_GURU_GOLDEN`). Of those 38, **10 appear nowhere under
`docs/`** — `CG_BASE`, `CG_FILTER_REMOVE`, `CG_LIVE`, `CG_LIVE_ARCHIVE_REMOTE`,
`CG_LIVE_PRICE`, `CG_LIVE_RCLONE`, `CG_SMTP_LIVE_TO`, `CG_TRIGGER_CF_MAX`,
`CHEAP_MODEL_AUTH`, `KVCACHE_PYTHON`. That is a grep, not the AST inventory §3.2 proposes, so
treat the exact list as indicative — but a config surface a quarter of which is undocumented
cannot be diffed against a deployed host, which is the point.

## 2. Why the gate cannot be "run SWE-bench"

The measured cost of the study in `docs/results/`, SWE-bench Verified, 50 tasks,
`claude-code` on `aws/claude-sonnet-5`:

| arm | billed cost | agent wall (sum) |
|---|--:|--:|
| baseline `off` | $31.98 | 317 min |
| `codesmart` | $27.77 | 293 min |

Two arms, one trial each, is **~$60 and ~10 h of agent wall**. The published study ran four
arms at two trials. Per deploy, that is not a gate, it is a research budget.

**But cost is the weaker of the two objections.** The stronger one is that reward is the
*worst* signal per dollar on offer. The headline separation between arms was **43 vs 44 of
50** — one task. On a 12-task subset at p≈0.86 the binomial sd is ≈1.2 tasks, so such a subset
cannot distinguish an 88% pipeline from a 78% one. Spending $10 to sample the noisiest metric,
then reading "12/12 solved" as "no capability regression", would be a gate that manufactures
false confidence. That failure mode is worse than no gate, because a deploy proceeds on it.

So the design principle is: **pay only for what cannot be measured for free, and gate hardest
on the lowest-variance quantity available.**

## 3. Tier 0 — free, every PR, in CI (seconds)

Contract and configuration drift. No model, no Docker, no network.

Every check below produces a **golden artifact under version control**. That is deliberate,
and it is how the requirement "unless they need change, and that needs to be spotted as well"
is met: an intended change shows up as a diff in the golden, in the PR, and a human or
reviewing agent has to accept it in the commit. Silent drift becomes impossible; intended
change becomes visible and cheap.

### 3.1 Preset and pipeline drift — extend what exists

`deploy/harbor/pipeline_drift_test.go` already asserts the harness arms run the shipped
pipelines, and its own comment records why it covers `codesafe` as well as `codesmart`: the
first version covered one preset, and the same bug was found one arm down. Generalise it to
its natural closure — every preset named anywhere in the tree (harness arms, docs tables,
`presetnames.go`, examples) resolves to the shipped pipeline, checked as a set in both
directions, the way `config/docdrift_test.go` already does for the doc tables.

### 3.2 Env-var contract — the `TENANT_MONTHLY_CAP_USD` gate

Generate the inventory from the AST (`os.Getenv` / `os.LookupEnv` call sites), not a grep, and
emit it as a committed artifact (`deploy/gate/golden/envvars.json`) carrying for each var:
name, package, whether it is operator-facing, and where it is documented. Then assert:

1. every operator-facing var the binary reads is documented under `docs/`;
2. every var a shipped unit file or drop-in *sets* is one the binary reads.

Check 2 is the dead-control gate, and it is the one that needs the deployed host (§6).

### 3.3 Effective-config golden

`cmd/context-guru-proxy/main.go:983` already assembles the **resolved** configuration for the
dashboard's config page (`effectiveConfig`). Snapshot it for a fixed matrix of inputs (each
preset × the default flag set × a representative config file) into goldens. Any change to any
default — a trigger threshold, a cheap-model choice, a component's `max_tokens` — appears as a
diff. This is the highest-value check in the proposal per hour spent, because it covers the
whole config surface at once rather than one field at a time.

### 3.4 Route and auth-class golden

One table: route → authenticated? metered? loopback/CIDR-only? `proxy/security_test.go` and
`proxy/tenancy_test.go` already assert pieces of this per route. A golden makes *losing* a
protection visible, which per-route assertions do not — `/compact` is the example the code
itself flags: "the most expensive route on the box was the only one with no rate limit".

### 3.5 Cost-model agreement

`deploy/harbor/kv_ttl_cost_drift_test.go` already holds the Python cost model to
`kvcache.Simulate`. It belongs in the gate's tier-0 set rather than living only as a harness
test, since every dollar figure the gate reports downstream depends on it.

## 4. Tier 1 — cents, every PR touching `components/`, `apply/`, `expand/` (minutes)

**This is where an expensiveness regression is actually caught**, and the reason it is cheap is
already in the tree: `/compact` runs the pipeline and returns the rewritten body **without
forwarding upstream**, and `deploy/harbor/replay.py` already drives it over a captured request
stream, with a format-integrity check, explicitly "deterministically, with NO repeated
agent/LLM spend".

What is missing is not the mechanism. It is a **frozen corpus** and a **committed baseline**.

### 4.1 The corpus

A `CONTEXT_GURU_CAPTURE` JSONL recorded from a real baseline agent run, pinned by sha256 and
fetched by the gate rather than committed (size, and it is real traffic — see §11). It must
cover the shapes that carry the money: large `tool_result` blobs, repeated file reads, bash
output, MCP tool schemas, and a realistic cached-prefix layout. A corpus on which the pipeline
never fires measures nothing, so corpus adequacy is itself asserted: each gated component must
act at least once, or the run fails as *unmeasured* rather than passing as *unchanged*.

### 4.2 What it measures

Per component and per preset: `tokens_before/after`, `saved_tokens`, `saved_tokens_unique`,
act counts, and — the part that matters most — the **cache-tier split**. `measured-2026-08.md`
records that 90.74% of input tokens bill as cache reads at 12.6× less than fresh input, and
that compaction can therefore *raise* the bill while cutting content. So the gated number is
**net dollars at the live price list, including our own LLM spend**, not a token percentage,
plus a separate hard gate on cache-write growth (the published arms were a four-way wash within
1.1%; a regression here is the expensive one).

Two more zero-tolerance checks: every rewritten body is still structurally valid for the next
call (`replay.py` does this), and every `<<cg:HASH>>` emitted resolves through `/expand` back to
the original bytes. The "fail open, always" and "every lossy Offload must be reversible"
boundaries in `CLAUDE.md` are exactly the invariants a gate should hold, and both are testable
here for free.

### 4.3 Determinism

Deterministic components get **exact** baselines, zero tolerance — a 1% savings regression is
invisible on a 12-task live run and unmissable here. The LLM components (`extract_llm`,
`summarize`) are not deterministic; give them a recorded-response cache so this tier stays free
and exact, and measure them live only in tier 2.

## 5. Tier 2 — the live subset, per release candidate (~$10, ~1 h)

Only for what replay cannot see: real prompt caching, a real agent loop, real tool use.

### 5.1 Choosing the subset

Not a list invented here. Select from the existing 50-task per-task record by four criteria:

1. **the long tail**, where compaction matters and cache busting shows — `django__django-13128`
   (123 steps, 10.15M cache-read, $2.99), `matplotlib__matplotlib-25775` (108 steps, $2.27),
   `django__django-14034` (84 steps, $1.79);
2. **tasks where the arms diverged** — `django__django-14034` went reward 1 → 0 between
   baseline and `codesmart`, and its steps fell 84 → 28. Whatever that is, it is sensitive;
3. **tasks where the pipeline actually fires**, from the per-component attribution in
   `rows-*.json` / `--dump-configs`;
4. **two cheap canaries** both arms always solved — `django__django-12050` ($0.09),
   `django__django-15572` ($0.14). If a canary breaks, suspect the harness, not the pipeline.

Criterion 3 needs data that lives on the eval box, not in this repo, so the plan includes a
**one-off selection run** rather than a task list asserted from thin air. From the per-task cost
table, ~12 tasks chosen this way land at **$9–11 per arm, one trial**.

### 5.2 What it gates on, strongest signal first

1. **Hard failures** — exceptions, upstream 4xx/5xx, malformed bodies, unresolved markers,
   expand bounces above baseline. Near-zero variance. Any occurrence blocks.
2. **Cost per task**, paired against the baseline task by task. Continuous, so far more powerful
   than the binary at this sample size.
3. **Steps and cache-hit rate** per task.
4. **Resolved count** — blocks only on a *gross* drop (e.g. 12 → 7). A one-task move is inside
   noise, and the report must say so rather than pretending otherwise.

### 5.3 The honesty clause

The report must state, in the artifact and not only in this document: *this tier detects gross
capability breakage and cannot detect an 88% → 82% reward regression; only the full 50-task run
can.* A gate whose green is over-read is the failure mode §2 warns about, and the
countermeasure is a sentence in the output.

## 6. Post-deploy verification

`cg-gate verify-deployed --host <h>` closes the loop `REDEPLOY-caller-pays.md` walks by hand:

- effective config on the host matches the tier-0 golden for the intended preset;
- every `Environment=` in the unit and its drop-ins is a var the binary reads (§3.2);
- `--version` matches the commit that passed the gate;
- the runbook's own three assertions: `healthz` 200, a keyless request 401, `/stats` 403 through
  the front end.

## 7. The baseline problem — the part worth arguing about

Absolute baselines rot, and not because of us: the upstream model changes, gateway routing
changes, task images change, the price list changes. A July number is not a July-equivalent
measurement in September.

Two designs:

**(a) Paired arms every run.** Run `off` and `codesmart` in the same session, gate on the
*delta*. Immune to upstream drift. Costs 2×.

**(b) Single arm against an absolute baseline, plus a drift canary.** Record in the baseline the
model id, price-list hash, and task image digests. Before each gated run, replay one frozen
request live with the pipeline off and assert the token accounting and price list have not
moved. When the canary moves, the baseline is marked **stale** and the next run must be a
re-baseline, in paired mode.

**Recommendation: (b).** Steady-state cost is halved, and correctness is preserved exactly where
it matters — the world moving is detected for cents instead of assumed away.

Baseline lifecycle, either way:

- lives in the repo as versioned JSON (`deploy/gate/baseline/<name>.json`) with provenance:
  commit, model id, price hash, image digests, date, and who accepted it;
- updated only by a PR carrying the run artifact — **never** written by the gate itself;
- **ratchet**: the savings baseline may move up freely, and down only with a stated reason in the
  commit. "Progress updated the baseline" and "a regression updated the baseline" must not look
  the same in the log.

## 8. The gate must be shown to fail

This repo's convention is that a test is not evidence until it has been seen to fail with its
subject reverted, and the coref-compaction branch produced three vacuous passes that motivated
it. A *gate* has the same exposure at higher stakes, so `cg-gate selftest` is part of the
deliverable, not a nice-to-have: it mutates each subject in turn — flip a preset component,
delete a documented env var, degrade one component's savings, corrupt one expand marker — and
asserts the corresponding check fails, with the failure text recorded.

Related and separate: every check asserts **its subject ran**. A corpus that fails to load, a
component that never acted, a task set that silently came back short — none of these may read as
"no regression". The gate reports `pass` / `fail` / `unmeasured` as three distinct verdicts,
because a counter meaning both "nothing broke" and "nothing was checked" is not a measurement.

## 9. Shape of the thing

`cmd/cg-gate`, in Go, in this repo — versioned with what it gates, and able to import `config`,
`components`, `kvcache` directly rather than re-deriving them. Tier 2 keeps driving the existing
Python harness; it works, and rewriting it would be a second implementation of the money
question, which §3.5 exists to prevent.

```
cg-gate tier0                      # goldens + contracts (CI, free)
cg-gate replay --corpus <sha>      # frozen-corpus savings + integrity (CI, cents)
cg-gate live --tasks <set>         # subset, on the eval box (~$10)
cg-gate verify-deployed --host <h> # post-deploy
cg-gate selftest                   # prove each check can fail
cg-gate accept-baseline <artifact> # writes the PR, not the file
```

One machine-readable verdict JSON plus a Markdown report; non-zero exit blocks. Wire tier 0 into
`ci.yaml`, tier 1 into `ci.yaml` behind a path filter, and tier 2 + `verify-deployed` into
`release.yaml`'s `workflow_dispatch` path — which already exists precisely so the release path
can be exercised before there is a tag to regret.

### Where it runs, and one trap to encode

Tier 2 runs on the eval box. Gate runs are launched **from development sessions**, which inherit
`ANTHROPIC_BASE_URL` pointing at Context Guru itself — and a gate that measures savings through
a proxy that already compacted the traffic reports nonsense. The harnesses already refuse to
start when the gateway names a context-guru route or a `cg_live_` token (`REPRODUCE.md`,
prerequisites); `cg-gate` must adopt the same refusal and set the benchmark gateway explicitly.

Separately: `deploy/eval-containers/sweep.py` hardcodes `ROOT` to one engineer's laptop path, so
it runs nowhere else as written. Any path the gate depends on has to come from the environment or
a config file.

## 10. Phasing

| phase | content | cost | value |
|---|---|---|---|
| 1 | tier 0 + `selftest` | free | covers the axis that has actually broken this deployment twice |
| 2 | corpus + replay baseline | cents/run | catches expensiveness regressions with near-zero variance |
| 3 | selection run + live baseline | one-off ~$25, then ~$10/RC | catches gross capability breakage |
| 4 | `verify-deployed`, release checklist doc, `release.yaml` wiring | free | closes the manual runbook |

Phase 1 is worth shipping alone.

## 11. Decisions needed before phase 2

- **Corpus provenance.** It is real traffic. Where is it hosted, what scrubbing does it get, and
  who approves its use? Phase 2 cannot start without an answer.
- **Budget per gated run**, which sets the task count in §5.1.
- **Who accepts a baseline update** — and whether a reviewing agent may, or only a human.
- **Does reward get a hard gate at all**, given §5.3, or is it advisory with the full run as the
  only blocking authority?
