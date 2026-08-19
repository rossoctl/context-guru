# agentdiet

!!! info "Offload (LLM) — lossy, reversible"
    A baseline reproduction of the published **AgentDiet** trajectory-reduction method: one
    cheap-model reflection per turn, on the step that has just aged past a fixed delay.

## Why it exists

`agentdiet` is a **comparable baseline**, not a recommendation. It reproduces the method from
*"Reducing Cost of LLM Agents with Trajectory Reduction"* (Xiao, Gao, Peng, Xiong — FSE 2026,
[arXiv:2509.23586](https://arxiv.org/abs/2509.23586)), which the authors call AgentDiet, so the
published approach can be A/B'd against context-guru's own reducers on the same traffic, agent and
benchmark. The paper reports **−39.9%…−59.7% input tokens** and **−21.1%…−35.9% total cost** at
unchanged task success on SWE-bench Verified and Multi-SWE-bench Flash.

## How it works

Its unit is the **step** — one assistant message plus the tool results that answered it — not one
tool output. Four things follow from that, and together they are what distinguishes it from
[`extract_llm`](extract_llm.md):

1. **A fixed age chooses the target, not size or economics.** When the agent has completed step
   `s`, only step `s − a` is eligible (`delay_steps`, a=2). The most recent `a` steps are never
   touched, so a bad reduction cannot corrupt what the agent is working on right now — the paper's
   protection against a malfunctioning reflection model.
2. **The model gets a sliding window of neighbouring steps**, serialized as XML
   (`context_steps`, b=1 ⇒ steps `[s−a−b … s]`). This is the part `extract_llm` structurally cannot
   do: seeing the steps around the target is what lets the model call content *redundant* (already
   stated nearby) or *expired* (mattered only to a finished sub-goal) rather than merely verbose.
3. **Two thresholds bound the spend.** A step below `min_step_tokens` (θ=500) never earns a call;
   and a reduction that comes back is applied only if it clears `min_saved_tokens` **or**
   `max_keep_ratio`, so a marginal rewrite does not pay a cache-write for a handful of tokens.
4. **A reduction is made once, then frozen.** Later turns replay the same bytes, so the request
   prefix stays stable and reductions accumulate over the session — which is what the paper gets
   for free by editing the agent's own trajectory in place.

The window is serialized in the shape the reflection prompt describes:

```xml
<step id="7">
<think>The fix works. Now run the existing suite to check nothing broke.</think>
<call tool="bash">{"command":"python -m pytest testing/test_collection.py -v"}</call>
<result id="0">… 74 collected, per-test PASSED lines, summary …</result>
</step>
```

## Before → After

```
before:  <result id="0">  … 74 individual "PASSED" lines … 73 passed, 1 xfailed in 4.48s
after:   ... (individual test lines omitted; mostly PASSED)
         ======= 73 passed, 1 xfailed in 4.48s  <<cg:…>> [full output: call context_guru_expand]
```

## Lossiness

Lossy but reversible — each reduced tool result is stashed under its own `<<cg:…>>` marker and
recovered via `context_guru_expand` / `GET /expand`.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `delay_steps` | 2 | *a* — steps of protection; only the step this far back is eligible. `0` is accepted for an ablation, but it targets the step the agent has just completed and so gives up the protection described above. |
| `context_steps` | 1 | *b* — steps of leading context in the window (`[s−a−b … s]`). |
| `min_step_tokens` | 500 | *θ* — a step below this is not worth a reflection call. |
| `min_saved_tokens` | 400 | Apply the reduction if it saves at least this many tokens… |
| `max_keep_ratio` | 0.8 | …or if it keeps less than this fraction of the step. |
| `model.source` | `incoming` | LLM source: `incoming` or `config` (the cheap model). The preset uses `config`. |
| `model.model` | *the source's own model* | The reflection model, on that source's endpoint and credential. The method's economics depend on it being much cheaper than the agent's. |
| `model.provider` | `anthropic` | Wire dialect for a config-pinned endpoint: `anthropic` \| `openai`. |
| `model.base_url` | *the provider's public API* | Pin a dedicated endpoint as a full URL. |
| `model.api_key` | *the process env key* | **Credential** for the pinned endpoint; empty falls back to the provider env key, which a hosted deployment refuses. Write-only on the settings page. |
| `model.auth` | `x-api-key` | Anthropic only: `x-api-key` \| `bearer`. |
| `marker_mode` | `full` | `full` (reversible) \| `summary` \| `off`. |
| `cache_tail_only` | `false` | Restrict new reductions to the uncached tail. See the warning below. |

`CONTEXT_GURU_AGENTDIET_TIMEOUT` (default `90s`) bounds one reflection call; `/stats` reports
`agentdiet_timeouts`, `agentdiet_errors` and `agentdiet_call_timeout_ms` beside it. A non-zero
timeout count means the budget is too small for the server's load, and that arm's savings are an
**undercount rather than a measurement**.

!!! warning "`cache_tail_only` defaults to `false`, unlike every other age-based offloader"
    The target step is chosen by age, so with `a ≥ 1` it is **always** inside the provider's cached
    prefix. A tail restriction would therefore make this component a silent no-op on every caching
    backend. The paper accepts one cache-write of the suffix per reduced step and counts it in its
    cost figures; because the decision is then frozen and replayed byte-identically, that write
    happens **once per step, not once per turn**. Set `true` only if you would rather keep the cache
    pristine and reduce nothing.

## Faithfulness to the paper

Defaults are the paper's tuned values (`a=2`, `b=1`, `θ=500`). `min_saved_tokens` (400) and
`max_keep_ratio` (0.8) come from the authors' artifact, whose apply-gate is
`saved >= 400 || keep < 0.8`; Algorithm 1 in the paper states this more simply as
`l_orig − l_reduced > θ`. The artifact's form is the default because it is what produced the
published numbers — set `min_saved_tokens: 500` and `max_keep_ratio: 0` to reproduce the paper's
stated gate exactly.

Three deviations, all forced by where context-guru sits:

- **The reduction is written in place**, into the step's tool-result messages. The paper's reflection
  module replaces the whole step with one assistant message; here only
  [`summarize`](summarize.md) changes the message count, and it must run alone for that reason.
  Tool results are where **63%** of trajectory tokens live by the paper's own accounting
  (30.4K of 48.4K).
- **Tool-call arguments are not reduced.** On Anthropic traffic bifrost's schema does not model
  `tool_use` blocks, so their name and input are not visible to any component here and the assistant
  message is not even rewritable. That forgoes the paper's `str_replace_editor` redundancy case
  (~25% of trajectory tokens) and is the main reason to expect a smaller reduction here than the
  published 39.9%–59.7%.
- **The prompt is written from the paper's description** of its four parts (job, format, the three
  waste categories with the examples the paper names, and the anti-loss guidelines) rather than
  copied from the authors' artifact. `components.Model` is one prompt in / text out, so the paper's
  system + user + assistant-prefill + `</step>` stop sequence is folded into a single prompt and the
  reply is parsed defensively instead (prose, code fences and a missing close tag are all tolerated;
  an unparseable reply reduces nothing). One case is declined rather than guessed: a `<result>` block
  whose `id` is missing or unparseable is used only when the step has a single result. On a step with
  parallel tool calls it is dropped, because placing it at `id 0` would put one tool's compressed
  output into another tool's message — smaller, so the never-worse check would pass it, and then
  frozen and replayed for the rest of the session.

## When it shines

Long agentic coding sessions with verbose tool output — test runs, build logs, directory listings —
where the same step is re-sent on every subsequent turn. Its `expired` category is the one signal no
other component here computes.

## When it's inert

Fewer than `b + a` completed steps; a target step below `min_step_tokens`; a reduction that fails the
apply-gate; no model available (it then replays what it already froze and reduces nothing new); or a
trajectory whose tool outputs are all small — GAIA-shaped traffic, where the median text tool output
is well under θ.

## Run it alone

The preset is `[format, agentdiet, cachesplit]` on purpose. The method's claim is what **one**
age-targeted reflection achieves; stacking context-guru's own offloaders beside it would reduce the
same tool outputs first, and there would be nothing left to attribute.

See also: [Components overview](../components.md) · [extract_llm](extract_llm.md) ·
[summarize](summarize.md) · [Choose a preset](../how-to/choose-a-preset.md)
