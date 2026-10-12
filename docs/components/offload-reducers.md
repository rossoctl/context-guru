# Offload: deterministic reducers

Two Offload components that reduce tool-output tokens with **no LLM call**: **dedup** and
**extract**. Each removes only content that is provably redundant, and every cut is stashed behind
a `<<cg:HASH>>` marker and recoverable via `context_guru_expand` / `GET /expand`.

---

## dedup

!!! info "Offload — lossy, reversible"
    Replaces a tool output byte-identical to an earlier one in the same request with a short pointer + marker.

### How it works

`dedup` replaces a tool output byte-identical to an earlier one in the same request with a short
pointer + `<<cg:HASH>>` marker. Exact match only (near-duplicate is deferred).

### Before → After

```
before:  <big config dump>  … (later, identical) <same big config dump>
after:   <big config dump>  … [repeat of earlier `cat /etc/app/config.yaml`; expand <<cg:1c8e…>>]
```

The pointer is one bracket that closes around the marker, which is deliberate: the expand cue
costs 3 tokens there instead of the 10 a separate `[full output: call context_guru_expand]`
trailer costs, and the tool's full name is not repeated because `expand.Inject` puts its
definition in `tools` on every turn that carries a marker. Against the pre-#281 note the whole
pointer costs **+1 token** with no paired call, **+2** for a short command, and is capped at
**+16** for a pathological one (`min_tokens` defaults to 100, so it still pays comfortably).

The note names the **producing command**, recovered via `schema.ToolCalls`, rather than the earlier
message's position. Two reasons, and both are load-bearing:

- A *position* ("identical to the output at step 12") is not something the model can resolve — it
  sees no message indices — and it would make the replacement bytes position-dependent. `dedup` is
  one of the offloaders `CacheAware` exempts from the cache-tail gate precisely because it is
  byte-stable on the unchanged prefix; a marker carrying an index would rewrite itself as the
  transcript grows and force a full-suffix cache write. The command is derived from content that
  already sits ahead of this message, so it is stable across turns.
- An equivalence claim the model can *locate* is one it can also **disbelieve**. That matters most
  right after an edit, when "this output is unchanged" is being relied on for signal rather than for
  token savings — see [#281](https://github.com/rossoctl/context-guru/issues/281). Only the model can
  initiate recovery, so the note states what was removed and how to get it back, never that the
  equivalence was safe to assume.

When a tool result has no paired `tool_use` block, the note degrades to the unqualified
`[repeat of an earlier tool output; expand <<cg:…>>]` — still announcing itself as a pointer and
still recoverable, without inventing a source.

### Lossiness

Lossy but reversible — the duplicated output is stashed and recovered via `context_guru_expand` /
`GET /expand`.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 100 | Skip outputs smaller than this token count. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

### When it shines

Agents that re-read the same file/command output repeatedly.

### Measured on real traffic, and why the hash stays exact

Across **4,149 real requests** (31,241 in-request candidates above `min_tokens`) there was exactly
**one** byte-identical duplicate and **zero** duplicates that differed only in whitespace.

Switching the hash to `extract.ContentKey` — the whitespace/marker-insensitive key `freeze` uses —
was considered and rejected on both halves of that. It would gain nothing measured, and it is not
lossless in the way that matters: `ContentKey` collapses *every* whitespace run, so two outputs that
differ only in indentation (the same Python file read before and after a re-indent, two diffs with
different leading space) would hash equal, and the later one would be replaced by a note claiming it
is *identical to* the earlier output. The bytes are still recoverable through the marker, but the
sentence the model reads would be false until it expands — and naming the producing command makes a
false claim more misleading, not less, because it reads as checkable. Exact bytes is the right key
for a claim of identity.

### When it's inert

No exact repeats, small outputs.

---

## extract

!!! info "Offload — lossy, reversible (deterministic, no LLM)"
    Collapses only *obvious, provably redundant* noise in a tool output — consecutively
    repeated lines/blocks, runs of blank lines, and progress-bar/spinner churn — keeping every
    unique informative line verbatim.

### How it works

`extract` is the **deterministic, no-LLM** tool-output reducer. It runs on every request (cheap)
and is deliberately **conservative**: it removes only what is unambiguously redundant, so it can
never hide content the agent needs and force it to redo work. Specifically it:

- strips terminal noise (ANSI escapes, carriage-return progress churn);
- collapses a run of blank lines to a single blank;
- drops progress-bar / spinner lines (`… 45%`, `[####....]`, spinner glyphs);
- collapses a consecutively **repeated** line or block (up to 12 lines) to one copy.

It stashes the full original behind a `<<cg:HASH>>` marker (reversible), and — like every offloader
— only commits if the result (marker included) is actually smaller. Because it is a pure function of
the message content, its output is **byte-stable across turns**, so it never busts the KV cache.

!!! note "Relevance-aware trimming is a different component"
    Query-relevance projection, regex rewriting, and summarization are the job of the separate
    **[`extract_llm`](extract_llm.md)** component. Configure them together (`[extract, extract_llm]`,
    as in the `agent`/`general` presets) so the cheap deterministic pass runs every step and the LLM
    pass only fires on large outputs every few steps.

### Before → After

Captured live through the proxy (`pipeline: [extract]`) — 15 identical warning lines collapse to one,
blank runs collapse, and the original is stashed:

```
before:  Cloning into 'repo'...
         ...download progress...
         resolved 200 packages
         warning: peer dependency unmet          ← repeated 15×
         warning: peer dependency unmet
         … (13 more identical lines)


         build complete in 4.2s

after:   Cloning into 'repo'...
         ...download progress...
         resolved 200 packages
         warning: peer dependency unmet          ← one copy kept
         build complete in 4.2s
         <<cg:40b571fdebccdcd4>> [full output: call context_guru_expand]
```

### Lossiness

Lossy but reversible — the original is stashed and recovered via `context_guru_expand` /
`GET /expand`. In practice the only bytes dropped are exact duplicates and progress churn, so a
recovery is rarely needed.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 300 | Output floor before extraction runs (folds into `trigger.min_output_tokens`). |
| `trigger` | — | Optional gate: `min_output_tokens`, `min_request_tokens`, `min_messages`. |
| `marker_mode` | `full` | How the recovery marker is emitted: `full` \| `summary` \| `off`. |

### When it shines

Build/install logs, package-manager output, and any stream with repeated warnings, progress bars, or
duplicated blocks. Cheap enough to leave on for every request.

### When it's inert

Output below the floor, nothing obviously redundant to collapse, or a collapse that wouldn't shrink
the message once the marker is added.
