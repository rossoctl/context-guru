---
name: insights-components
description: Which context-guru components actually earned their place on this machine's traffic, which ran and never acted, which cost more than they saved, and which switched-off ones there is measured evidence for enabling - including how much of the cost is tool output and conversation compaction. Use when the user asks which preset to use, whether a component is worth it, what the pipeline is doing, why tool output costs so much, about compaction or summarization cost, or what they should turn on. Accepts --json.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/insights.py)
---

# What earned its place, and what is switched off

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/insights.py" components
```

**With `--json`**, add `--json` and print the output verbatim.

`preset` is the configured preset and `pipeline` the components it builds, in order.
`components_ran` is what actually has rows. **Where they disagree, the running proxy predates the
configuration** — check for the `config-pending` finding before telling anybody to change a setting
they already changed.

## The verdict on a component that ran

For each `component.<name>.*`, the verdict is **`net_usd`** and nothing else. It is
`net_usd_with_estimate` under the hood, which matters twice:

- A component that spends on its own (`extract_llm`, `summarize`) judged on its saving while its own
  LLM bill is ignored is not a verdict.
- The same component judged on its full spend against only the rows written since the saving column
  existed is not one either.

`saved_usd` beside it is the gross. Quote the net; mention the gross only if asked.

`replay_multiple` is the factor that explains a sign flip: a removal is priced once at the
cache-write rate it would have entered the prompt at, then again for every later turn the content
stayed absent. **That is why a short window understates a component and a long one does not**, and
why the proxy's own `/stats` verdict (first-removal only) can read negative while this one reads
positive. If a user has seen both, that is the reconciliation — not a contradiction.

`acted_tokens` vs `acted_structural`: the second is a run that changed the request **without**
removing anything, which is the entire point of `cachesplit` (it moves a cache breakpoint) and
`cacheinject` (it places one). A component whose work is structural has `acted_tokens=0` forever and
that is success, not a zero to apologise for.

`reverted` above zero is fail-open working: the component errored, that component alone was reverted,
and the original request went forward. Not a fault to report as one.

`unpriced_rows` above zero means rows that removed tokens and could not be valued at all. Counted,
never valued at zero, because "unknown" and "worthless" are different claims.

`component-inert-*` — ran, never acted. A real answer about their traffic rather than a fault: some
components only fire on shapes they do not send. The cost is latency, and leaving it is harmless.

## The switched-off set, and the boundary you must not cross

**You cannot measure what a component that never ran would have removed.** Simulating it means
replaying every request through it, which is a benchmark and not a report. So the off set comes back
in three tiers, and the tier is on the wire:

| Tier | What it means | What you may say |
|---|---|---|
| measured | `toolfilter`, `cachesplit` only — the store holds the quantity they would have acted on, priced at the tier the requests really paid | "would have saved $X" |
| related | the offloaders. No figure for the component; a **measured** figure for the waste it targets | "there is $X of this kind of waste" — never "this would save $X" |
| none | everything else | name it, say what it does, **give no number** |

The `offloaders-off` finding is tier *related*, and its own text says so. Carry that through: its
dollar figure is `compaction_no_episode_cold_usd` — conversations that grew past what the cache held,
hit no compaction episode at all, and paid a cold rebuild anyway. That is **the measured size of the
problem**, and it is deliberately not the compaction credits beside it, which measure compaction that
did happen. Presenting it as a projected saving is the one way to make this section untrustworthy.

`off.<name>` facts name each unmeasured component and what it does, with no figure. **Do not go and
find one.** A study figure from another corpus quoted as this user's saving is exactly what
`off_unmeasured_note` is warning against, and this repo's own presets carry a worked example of why:
`linecap`'s "20.3%" is gross reach — the share of shipped tokens its rules touch — and measured
incrementally on the same corpus it was worth +0.797pp, roughly a fourteenth of what the headline
reads as.

## Tool output and compaction

`compaction.<provenance>.*` is split by provenance and **not summed across it**, because a compaction
the client did and one the proxy did are different events. `compaction_episodes`,
`compaction_net_usd` and `compaction_summarizer_usd` are the rollup; `compaction_conversations` vs
`compaction_no_episode` is the coverage — how many conversations were managed at all.

Where `bash_max_output_length` is set, it is worth one sentence of context: it decides how much tool
output enters the transcript in the first place, and every offloading component exists to deal with
what it lets through. **Do not recommend lowering it as a cost fix.** It changes what the agent can
see, which is a correctness decision, not a cost one.

## Recommending a preset

Only where a finding argues for it, and name what the change actually adds:

```bash
/plugin configure context-guru
```

| Preset | Adds | Spends on its own |
|---|---|---|
| `off` | nothing runs; requests forwarded untouched | no |
| `cache` | `cachesplit` only | no |
| `house` | deterministic trimming, `extract`, and `toolfilter` | no |
| `codesmart` | the study's winning pipeline; a cheap-model relevance pass. **No `toolfilter`** | yes, a little |
| `housellm` | `house` plus a compaction-model pass over the conversation | yes, the most |

`/context-guru:preset-picker` explains the trade in each and is the better thing to hand somebody who
is undecided. **Say it out loud when a recommended preset spends**, and if a `toolfilter-off` finding
is what motivated the change, note that `codesmart` does not carry it — moving there to fix that
finding does not fix it.

## Never

- Never quote a `related`-tier figure as a saving.
- Never add a component's `net_usd` figures into one total across components. Their savings overlap:
  two offloaders reducing the same tool output would be credited twice.
- Never report `acted_tokens=0` on a structural component as a failure.
- Never recommend `off` to fix a negative net without saying it turns off everything that is working.
