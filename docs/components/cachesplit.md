# cachesplit

!!! info "Reformat — lossless, and a marker component"
    Enables the **volatile-tail split**: the top-level `system` array's churning tail is
    separated from its stable head so the provider's cache boundary excludes the churn.
    The model sees byte-identical text.

`cachesplit` is in the default presets. It carries no logic of its own: the component's
`Reformat` always reports `Skipped`, and the actual rewrite is body-level — it edits the
top-level `system` array, which components never see — and lives in `apply/prefixsplit.go`,
gated on this name being present in the pipeline.

`cachesplit` and [`cacheinject`](cacheinject.md) are separate config entries because their
evidence is not comparable: the **split** is measured (−34.1% cost, and 0% → 96.7% cache hit in an
isolated A/B), while breakpoint **placement** has never been shown to help. Keeping them apart is
what stops disabling placement from silently disabling the split too. `apply` still honours
`cacheinject` as a gate for the split, so an existing pipeline naming only `cacheinject` keeps
working.

## What the split does

A cache entry hashes **everything before** its breakpoint, and no breakpoint position can
exclude part of a single content block. Claude Code appends a live environment snapshot to
the **end** of its main system block:

```
Current branch: main
...
Recent commits:
0898367954 SWE-bench
```

Measured across 50 SWE-bench tasks that block is ~7,017 tokens, of which the first
**6,921 (98.4%)** are byte-identical across sessions — but it is one cacheable unit with its
breakpoint at the end, so the hash covers the churning tail and the shared 98.4% is
re-written every session.

The tail is real content: it cannot be moved or dropped without lying to the model about the
repo state. It can be **split** — `[stable][volatile]` as two text blocks with the same
concatenated text, breakpoint on the first. Adjacent text blocks concatenate, so the model
sees a **byte-identical** prompt while the provider gains a hash boundary that excludes the
churn. Asserted by `TestSplitIsConcatenationIdentical`.

## What it is worth

The full four-way measurement (structural target, mechanical verification, isolated live
A/B, end-to-end agent run) is on the `cacheinject` page under
[The volatile-tail split](cacheinject.md#the-volatile-tail-split), because that is where the
work was done. The headline: **−34.1% mean cost on one Terminal-Bench task measured three
times**, and `$0.0205` saved per warm session on Sonnet 5 in the isolated A/B. Read the
caveats there — a second Terminal-Bench task was within noise and its numbers are not
quoted.

## Configuration

None. It is a name in the pipeline:

```yaml
pipeline: [format, dedup, failed_run, cmdfilter, cachesplit]
```

A `components.cachesplit:` block is an **error**, not a no-op. The constructor used to take
its raw block and discard it, so a document saying `cachesplit: {ttl: 1h}` looked configured
and was not; it now decodes strictly like every other component and refuses the block.

## When it shines

Anthropic-family agents whose system prompt carries a volatile tail (an environment
snapshot, a timestamp, a git status) in front of a cache breakpoint.

## When it's inert

**Explicit-breakpoint providers only** (Anthropic, Bedrock, Vertex). Under an implicit
longest-prefix cache (OpenAI, Gemini) the match already ends at the divergence, so a block
boundary buys nothing. It is also inert when the system block has no separable volatile
tail.

Because the component itself always skips, `/stats` lists it under `top_passthrough` — that
is expected, not dead weight. The split's saving is a provider-side cache effect, invisible
to content-token counts.

See also: [Components overview](../components.md) · [cacheinject](cacheinject.md) ·
[Choose a preset](../how-to/choose-a-preset.md)
