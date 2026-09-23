# Offload: deterministic reducers

Seven Offload components that reduce tool-output tokens with **no LLM call**: **dedup**,
**failed_run**, **extract**, **smartcrush**, **mask**, **collapse**, and **readlifecycle**. Each
is lossy but reversible — every cut is stashed behind a `<<cg:HASH>>` marker and recoverable via
`context_guru_expand` / `GET /expand`.

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
after:   <big config dump>  … [identical to an earlier tool output] <<cg:1c8e…>>
```

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
different leading space) would hash equal, and the later one would be replaced by the words
*"identical to an earlier tool output"*. The bytes are still recoverable through the marker, but the
sentence the model reads would be false until it expands. Exact bytes is the right key for a claim
of identity.

### When it's inert

No exact repeats, small outputs.

---

## failed_run

!!! info "Offload — lossy, reversible"
    Keeps the most recent test/build run in full, collapses every earlier superseded run to a pointer + marker.

### How it works

`failed_run` recognizes test/build run output (regex: `N passed/failed`, `BUILD SUCCESS/FAIL`,
`Traceback`, `FAILED`, `panic:`, `npm ERR!`, pytest session banners). It keeps the **most recent**
run in full and collapses every earlier run **that failed** to a pointer + `<<cg:HASH>>` marker —
a superseded failure is safely recoverable, while an earlier run that *passed* is a distinct result
the agent may still reference and is kept verbatim. Needs ≥2 run-like outputs.

#### Markers are anchored to line starts

Every structurally line-initial marker — `BUILD SUCCESS/FAIL`, `=+ FAILURES`, `=+ test session`,
`Traceback (most recent`, `FAILED`, `panic:`, `npm ERR!` — must appear at the **start of a line**
(leading indentation allowed). Only `N passed/failed/error` stays unanchored, because pytest pads
its summary line (`==== 1 failed, 40 passed in 12.31s ====`) and that marker is mid-line by
construction.

Anchoring matters: a replay over 1,795 real SWE-bench requests found 9 of 81 collapses were
misclassifications rather than superseded runs — a line-numbered source read of astropy's
`qdp.py`, a sympy source read, an xarray test file and a `git show` diff, each collapsed and
labelled "superseded by a later failed→re-run" because `Traceback (most recent` occurred inside
the source *text*. A line-numbered read begins every line with its number, so nothing structural
can start the line and all four shapes are excluded for free.

### Before → After

```
before:  [run 1] 3 failed, 5 passed …   [run 2 after fix] 8 passed
after:   [superseded by a later run] <<cg:7d1c…>> [full output: …]   [run 2] 8 passed
```

### Lossiness

Lossy but reversible — superseded runs are stashed and recovered via `context_guru_expand` /
`GET /expand`.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 100 | Skip runs smaller than this token count. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `cold_cache` | `false` | On a turn whose prompt cache has **provably expired** (idle past the provider TTL), act at any depth instead of only in the uncached tail. Free when the cache really is gone — the whole transcript is re-billed anyway — and the decision is frozen so later warm turns replay it. Off by default because a *wrong* cold reading costs a cache-write of the whole suffix. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache). |

### When it shines

Iterative fix→re-run loops.

### When it's inert

<2 runs detected, small outputs, and earlier runs that **passed** (gate
`earlier_run_passed`) — those are kept verbatim by design.

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

---

## linecap

!!! info "Offload — lossy, reversible (deterministic, command-agnostic)"
    The two output rules that do not need a command signature: a per-line character cap, and a
    collapse of **non-adjacent** repeated lines.

### How it works

This exists because per-command filters do not pay. `cmdfilter` ships 939 lines of them and
production has matched exactly two (`ssh`, `uv-sync`; `no_filter_match` 164,865); sixteen further
rtk command signatures — pytest, apt, npm, pip, go test, cargo, tsc, eslint, mypy, ruff, docker,
kubectl, make, gcc, git log, ps — were replayed against 9,763 real messages and **every one
matched zero**. These two rules fire on the same corpus for 1.75M tokens, **20.3% of everything
shipped**.

**Cap:** 500 chars, from the measured sweep (tokens removed / messages touched): @200
2,031,381/2,742 · @300 1,606,241/2,520 · **@500 1,105,387/1,337** · @1000 745,350/862. 500 takes
54% of @200's tokens while touching *half* as many messages, and an untouched message is one whose
bytes stay stable for the provider's cache.

**Never-truncate allow-list:** a line carrying a file path, a source location (`path:line[:col]`),
a stack frame, an error/exception/traceback, a test verdict, an exit status, a diff marker, a hunk
header or a URL survives **any** cap intact. Those lines are long for the same reason the noise is,
and they are the ones the agent has to act on. This is what makes a generic cap deployable.

**Duplicate collapse** is the non-adjacent case (649,330 tokens); `extract`'s
`collapseObviousNoise` already handles adjacent repeats and was measured at **63 tokens** of
remaining value. Guards: skip diff-shaped blobs entirely (two identical `+ return nil` lines are
two distinct edits), never collapse lines that differ only in a source location (two findings, not
one repeated), never under 8 trimmed chars, never the first or last 3 lines (banner + summary).
The `(xN)` keeps the elision visible and is rendered *after* the cap, so the cap cannot eat it.
Duplicates collapse **before** the cap, so a dropped duplicate is not also charged as a capped
line.

**It runs last among the offloaders**, and that position is measured. Every offload leaves a
marker and every offload skips marker-bearing content, so a *modest* reducer ahead of a *drastic*
one steals its candidates. On `general` over 1,795 real captured requests: 7th in the pipeline it
saved 5,524,476 tokens — **worse than the 5,556,801 with no `linecap` at all** — because it took
39,335 tokens off messages `collapse` would have taken 76,554 off, and its marker then made
`mask`/`extract`/`collapse` decline those messages outright. Last, it saves **5,811,621 (+1.33 pp
over the baseline)**.

### Before → After

```
before:  resolving dependency graph for module   after:  resolving dependency graph for module  (x30)
         step 1 done                                     step 1 done
         resolving dependency graph for module            step 2 done
         step 2 done                                      …
         <a 2,000-char minified blob>                     <the first 497 chars>... <<cg:…>>
```

### Configuration

`max_line_chars` (500; 0 disables), `collapse_duplicate_lines` (true), `min_size` (400),
`marker_mode`.

### When it shines

Any tool output over `min_size` — command-agnostic, so it fires where per-command filters do not.
Measured on 1,795 real captured requests: acts on 735, **171,473 tokens**, 1.41 ms/request.

### When it's inert

Output under `min_size`, output with no over-long or repeated lines, marker-bearing content.

---

## smartcrush

!!! info "Offload — lossy, reversible"
    Statistical JSON-array compressor: keep first/last items plus any error item, drop the rest, stash the full original.

### How it works

`smartcrush` is a statistical JSON-**array** compressor: it parses the array, keeps `keep_first` +
`keep_last` items plus any item whose raw JSON carries an error signal, drops the rest, and stashes
the full original behind a `<<cg:HASH>>` marker. Kept items are verbatim (schema-preserving). v1
uses fixed anchors (headroom's Kneedle adaptive-K is a documented refinement).

### Before → After

```
before:  [ {…}, {…}, … 200 items … ]
after:   [ item0, item1, item2, item198, item199 ] [5 of 200 items shown; full array: call …] <<cg:…>>
```

### Lossiness

Lossy but reversible — the full original array is stashed and recovered via `context_guru_expand` /
`GET /expand`. Kept items are byte-verbatim.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_items` | 5 | Minimum array length before crushing. |
| `min_tokens` | 200 | Output floor before crushing. |
| `keep_first` | 3 | Leading items kept verbatim. |
| `keep_last` | 2 | Trailing items kept verbatim. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

### When it shines

Long homogeneous JSON arrays (list endpoints, search hits) — the `mcp` preset.

### Measured on real traffic

`smartcrush` needs a top-level JSON array of at least `min_items`. Across **1,748 distinct tool
outputs** from every capture available here (SWE-bench, Terminal-Bench, Claude Code), 15 outputs
*start* with `[` and **none of them parse as JSON** (they are log lines and Python list reprs), and
no envelope string field holds a JSON array either. So the component has zero eligible input on
that corpus.

That is also why it does **not** descend the tool-runner envelope the way `format` and `toon` do.
The descent was proposed on the strength of a 673/673 envelope finding on those two components, but
adding it here would have changed nothing measurable: there are no arrays inside those envelopes
either. Its value, if any, is in MCP-style traffic that returns record arrays — measure that traffic
before adding the descent, and note that the note+marker would have to be placed *inside* the
string field for the envelope to keep parsing.

### When it's inert

Non-array output, fewer than `min_items`, nothing to drop.

---

## mask

!!! info "Offload — lossy, reversible"
    Age-based garbage collection: keep the newest few tool outputs verbatim, replace older ones with a short marker + stash.

### How it works

`mask` is age-based garbage collection: it keeps the newest `keep_recent` tool outputs verbatim and
replaces older ones (≥ `min_tokens`) with a short `<<cg:HASH>>` marker + stash. It is complementary
to the content-based offloaders.

### Before → After

```
after (older):  [older tool output masked; starts: 700 701 def __rmul__(self, m): 702 …] <<cg:…>> [full output: call context_guru_expand]
```

The head-peek comes from `keep_head_chars` (default 96); with `keep_head_chars: 0` the marker
is the bare `[older tool output masked]`.

### Lossiness

Lossy but reversible — masked outputs are stashed and recovered via `context_guru_expand` /
`GET /expand`.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `keep_recent` | 3 | Newest tool outputs kept verbatim. |
| `min_tokens` | 100 | Only mask older outputs at least this large. |
| `keep_head_chars` | 96 | Characters of the hidden output left inside the marker as a one-line head-peek, so the model knows *what* was masked without a blind `expand` round trip — evidence showed a bare marker on a masked source-file read forces needless expands. Set `0` for the opaque marker (≈2pp more savings). |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `cold_cache` | `false` | On a turn whose prompt cache has **provably expired** (idle past the provider TTL), act at any depth instead of only in the uncached tail. Free when the cache really is gone — the whole transcript is re-billed anyway — and the decision is frozen so later warm turns replay it. Off by default because a *wrong* cold reading costs a cache-write of the whole suffix. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache). |

### When it's inert

`mask` used to report `acted: 0` with no reason at all, which is indistinguishable from a broken
component. It now names one of these on every message it passes over, visible per component in
`/stats` and on the dashboard:

| gate | meaning |
|---|---|
| `within_keep_recent` | the request holds no more tool outputs than `keep_recent`, so nothing is "older" |
| `below_min_tokens` | the output is smaller than `min_tokens` |
| `already_marked` | already offloaded by an earlier component — benign, that content is compacted already |
| `kept_verbatim_after_expand` | the agent expanded this content, so it must not be re-compacted. The first turn this appears is the turn the message reverts to its full form inside the cached prefix — counted as `expand_prefix_flips`. |
| `cached_prefix` | the output is inside the provider's cached prefix, where a new mask would flip already-cached content and force a cache-write of the suffix. This is the gate `cold_cache` lifts on a provably-expired cache |
| `non_text_blocks` | a text rewrite would drop the message's non-text blocks |
| `marker_no_win` | marker + head-peek would not be smaller than the output itself |

On a prompt-cached backend `cached_prefix` is the dominant one by construction: an agent adds one
tool result per turn, so by the turn an output is older than `keep_recent` it is already in the
prefix. That is why `mask` looks inert on cache-aware traffic even though it is the biggest lever
on non-caching traffic, and why the `cold_cache` option matters most here.

### When it shines

Long agent trajectories where old tool results are unlikely to matter. In the `agent` preset it is
the biggest lever (~27% content-token savings, no reward loss — see
[RESULTS.md](../RESULTS.md)).

≤ `keep_recent` tool outputs, small outputs are the only case it is inert on beyond the gates above.

---

## collapse

!!! info "Offload — lossy, reversible"
    Content-agnostic fallback for an oversized tool output: keep a head + tail window, stash the full original.

### How it works

`collapse` is the content-agnostic fallback for an oversized tool output nothing more specific
handled: it keeps a `head_lines` + `tail_lines` window and stashes the full original behind a
`<<cg:HASH>>` marker. It runs late (after `cmdfilter`/`format`) and skips content already marked.

The window is cut by **lines** when that actually shrinks the output, and by **characters** when it
does not. The character path matters because `collapse` is the last resort: if it declines, nothing
caps the output. The line-only version declined every output of `head_lines + tail_lines` lines or
fewer (40 by default) — which is exactly the shape of the largest tool results in practice, a
database or HTTP API response serialised as **one line** of JSON. Measured on a live 128k-band arm: 16 upstream
400 `prompt is too long` responses in 3,347 requests, on outgoing bodies of **2.6 MB to 14.8 MB**,
and 17 of 75 runs errored. Nothing else caught them — `extract` acted 0 times (`no_obvious_noise`
16,891; it only strips known noise patterns), `cmdfilter` 0 (`no_filter_match`), `toon` 21
(`not_uniform_object_array` 233,501), `dedup` 774 (exact duplicates only), `extract_llm` declines by
design once the output exceeds the compaction model's own window (`over_model_context`), and
`summarize` protects `keep_last`, which is where a fresh oversized output sits. `linecap`'s per-line
cap does not rescue it either: its `neverTruncate` allow-list exempts any line matching
`^\S+:\d+`, and `{"items":[{"id":1,...` matches that by accident ([#151](https://github.com/rossoctl/context-guru/issues/151)).

The choice between the two windows is made on **bytes, not on the line count**. Having more lines
than the window says nothing about their size: an output with a multi-megabyte line inside the kept
head or tail survived a line-count gate untouched, and *worse than being declined* — dropping the
small filler lines in the middle counts as acting, which stamps a marker, which then makes `linecap`
decline `already_marked`. Measured at 4,195,111 B in and 4,194,922 B out, 189 bytes saved
on a body forwarding 1.57 M tokens. The line window is therefore taken only when it at least halves
the output; anything else falls through to the character window. The test is in bytes rather than
tokens deliberately: `max_tokens` is resolved through `max_frac` × the model's context window, which
can change mid-session, so a token test could classify the same content differently on a later turn
and emit different bytes — trading a savings bug for a cache bug. Bytes depend only on the content
and `head_lines`/`tail_lines`, so the classification is replay-safe.

The character budget is the component's own token threshold expressed in characters — no new knob.
It starts at `max_tokens × 4` (the same ratio `internal/tokens` falls back to), is split between
head and tail in the `head_lines`:`tail_lines` ratio, and is then **measured** and tightened if the
assembled window overshoots `max_tokens`, because dense JSON tokenizes closer to 2.5 chars/token.
The cut is on rune boundaries, so a multibyte character is never split.

Two bounds on that loop are worth knowing, because they mean a very small `max_tokens` does not
deliver what it says. The tightening is bounded at **3 passes**, and the window has a **200-character
floor** — so `max_tokens: 50` or lower buys a 200-character window regardless, and `max_tokens: 100`
measures ~118 tokens out. The floor exists so a `max_tokens` of 0 cannot reduce an output to a bare
marker with no cue about what to expand; `head_lines: 0, tail_lines: 0` routes to the character
window for the same reason. The seed is also never allowed above the content itself: `max_tokens
× 4` overestimates on dense JSON, and testing the bail-out against the un-measured seed used to
decline content the fit loop would have cut (at `max_tokens: 3000`, what `codesafe` sets, one-line
JSON from 3,006 to 4,491 tokens was oversized and left entirely uncut). The never-worse guard
measures the real marker and declines rather than growing a message, which is what makes the
residual slack harmless. Negative `head_lines`/`tail_lines` are clamped to 0.

### Before → After

```
before:  <2,000-line log>
after:   <first 20 lines>
         ... (1960 lines omitted) <<cg:44ab…>> [full output: call context_guru_expand]
         <last 20 lines>
```

### Lossiness

Lossy but reversible — the full original is stashed and recovered via `context_guru_expand` /
`GET /expand`.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `max_tokens` | 2000 | Threshold above which an output is collapsed. |
| `head_lines` | 20 | Lines kept from the start. |
| `tail_lines` | 20 | Lines kept from the end. |
| `max_frac` | 0 (off) | Threshold as a **fraction of the model's context window**. When the window is known this wins over `max_tokens`. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `cold_cache` | `false` | On a turn whose prompt cache has **provably expired** (idle past the provider TTL), act at any depth instead of only in the uncached tail. Free when the cache really is gone — the whole transcript is re-billed anyway — and the decision is frozen so later warm turns replay it. Off by default because a *wrong* cold reading costs a cache-write of the whole suffix. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache). |

### When it shines

A catch-all last stage for huge outputs.

### When it's inert

It now names its reason instead of passing silently: `below_max_tokens`,
`too_few_lines_and_chars` (no window that shrinks it: no usable line window *and* fewer characters
than the character window's floor — the only genuine decline left), `already_marked`, `kept_verbatim_after_expand`, `non_text_blocks`, `marker_no_win`,
or `cached_prefix`.

`too_few_lines` is **gone** and was not renamed: its population is now either handled (the
`char_window` *event*) or a real decline (`too_few_lines_and_chars`). Exporting a decline and a
success under one metric name gives a series that falls as the component works better.

`cached_prefix` is new. `collapse` used to carry **no depth restriction at all** — it re-derived the
whole transcript on every turn, which contradicted the cache-safety contract every other
supersession offloader obeys. It got away with it because the rewrite is deterministic, but not
quite: `max_tokens` is resolved through `max_frac` × the model's context window, so a `max_frac`
config plus a mid-session window change (a model swap, or `modelinfo` resolving differently after a
refresh) would silently re-threshold messages **inside** the cached prefix. It never ran in
production, so this cost nothing; it is fixed before it can. `collapse` now decides each output once,
on the turn it arrives in the uncached tail, and freezes that decision so later turns replay the same
bytes at any depth — measured byte-for-byte identical removal on real captures.

---

## readlifecycle

!!! warning "Offload — reversible. Ships OFF, in no preset."
    Offloads a file `Read` whose body no longer describes the file: the file was **edited** later
    (STALE) or **read again** later (SUPERSEDED). A **fresh** Read is never touched. Measured on
    this box's traffic it removes **0 tokens on every warm arm**, for the structural reason in
    [Why it is off](#why-it-is-off-by-default). Enable it only for the cold-sweep case below.

### What it detects

`schema.ToolCalls` pairs every `tool_result` with the `tool_use` that produced it, so the component
reads the tool **name** and its **arguments** — for a `Read`, the `file_path` and the `offset`/`limit`
window. It walks the transcript in index order and classifies each Read against **strictly later**
events only:

| class | proof in the transcript | why it can go |
|---|---|---|
| **STALE** | a later `Edit`/`Write`/`MultiEdit`/`NotebookEdit` on the same path | the body in context is **factually wrong** — content and line numbers that no longer exist. This is a correctness argument before it is a token argument. |
| **SUPERSEDED** | the same path read again later with the **same** `offset`/`limit` | the later Read is authoritative; the earlier body is redundant. |
| **FRESH** | neither | **never touched.** |

The marker states only what is true — *"was modified later"*, *"was read again later"* — never
"identical to", which a re-indent would already falsify. In `marker_mode: full` (the default) the
original is stashed and `context_guru_expand` restores it byte-for-byte.

#### Deliberately conservative, in four places

- **Supersession keys on (path, offset, limit)**, not on path alone. Claude Code reads *ranges*; a
  later read of lines 500–600 does not replace an earlier read of the whole file. Containment (a
  later full read covering an earlier partial one) is **not** modelled — worth 867 tokens on the
  entire corpus here, which does not pay for the reasoning.
- **An Edit anywhere in the file staleizes every prior Read of it**, including a Read of a different
  range: an edit shifts the line numbers the Read printed.
- **Bash commands are not edits** unless `bash_edits` is on (default **off**), and then only for
  `> file`, `>> file`, `tee`, `sed -i`, `patch`, `truncate` — forms whose operands *are* the files
  written. `git apply`, `python script.py`, a Makefile and a generator are **not** counted: the file
  they write is not in the command text, and guessing there deletes **correct** context.
- **An image Read is never rewritten.** All 23 Reads in `capture-tb.jsonl` are PNGs; a text rewrite
  would destroy the image block. `schema.Rewritable` is the guard.

### The measured split — ours vs headroom's

headroom's `read_lifecycle.py` records **67% stale / 12% superseded / 20% fresh of Read bytes**.
Measured over `testdata/read_lifecycle.json` (every Read/Edit sequence in the SWE-bench,
Terminal-Bench and Claude Code captures on this box — 6 transcripts, 34 text Reads, 32,650 tokens):

| class | Reads | share of Reads | tokens | share of Read tokens | headroom |
|---|--:|--:|--:|--:|--:|
| stale | 15 | 44.1% | 6,889 | **21.1%** | 67% |
| superseded | 0 | 0.0% | 0 | **0.0%** | 12% |
| fresh | 19 | 55.9% | 25,761 | **78.9%** | 20% |

**Our numbers are the truth for our traffic, and they invert headroom's.** Two reasons, both
visible in the corpus:

- **Superseded is zero.** The only path-level repeat Reads here are Terminal-Bench's image Reads
  (excluded) and one partial re-read of a *different* range. Honouring the range key — which
  correctness requires — leaves nothing.
- **Fresh dominates by tokens.** Interactive Claude Code sessions on this box are read-heavy
  *investigation*: they read big files and edit nothing, so all 17 of their Reads are fresh and they
  carry 79% of all Read tokens. Stale Reads live almost entirely in SWE-bench, where the agent edits
  — and there each Read is small.

Calibration this sits inside: `Read` output is **6.9%** of interactive input tokens (`Bash` is
56.3%), so even a perfect result in this class is bounded at roughly 5% of input.

### Why it is off by default

Deterministic replay through `cg-research/bench/ab.sh` (same capture, same clock, config the only
difference), arm `pipeline: [readlifecycle]` against `pipeline: []`:

| capture | requests | removed | gates |
|---|--:|--:|---|
| `short.jsonl` | 5 | **0** | `fresh_read` |
| `long.jsonl` | 35 | **0** | `fresh_read:131`, `no_file_reads:2` |
| `mixed.jsonl` | 21 | **0** | `fresh_read` |
| `cold.jsonl` (`CG_IDLE=430`) | 9 | **0** | `fresh_read` |
| SWE-bench session (122 reqs, the most file-active in `capture-swebench.jsonl`) | 122 | **0** | **`cached_prefix:1125`**, `fresh_read:720`, `below_min_tokens:136` |

The four interactive arms remove nothing because **every Read in them is fresh** — the safety
property working as designed, not a broken component. The SWE-bench row is the interesting one:
**1,125 stale-Read candidates were declined by the cache-tail gate**, and that is *structural*. A
Read only becomes stale once a later `Edit` appears, and by that turn the Read is already inside the
provider's cached prefix, so the gate can essentially never let this component act on a warm turn.

#### `stale_at_depth` — measured, and it does not pay

The obvious response is to lift the gate for the stale class. That costs one **re-anchor**: rewriting
the Read at index *i* forces a cache-write of everything after *i*, once, at the turn the Read goes
stale. The freeze then replays the decision forever, so the recurring saving is the removed body on
every later turn. Whether that pays depends entirely on how far the Read is from the tail when its
file is finally edited — so it was measured, not argued
(`TestReanchorCostOfStaleAtDepth`, `internal/tokens`/o200k, gateway price $2.00/MTok):

| session | turns | stale transitions | one-time re-anchor | recurring saving | distance Read→cache boundary | break-even |
|---|--:|--:|--:|--:|--:|--:|
| SWE-bench, 1 instance | 122 | 22 | 188,319 tok = **$0.471**@write | 13,541 tok/turn = **$0.0027**/turn@read | mean **30.5** msgs, max 100 | **174 turns** |
| SWE-bench, 3 instances | 355 | 45 | 475,613 tok = **$1.189**@write | 32,595 tok/turn = $0.0065/turn@read | mean **37.3** msgs, max 142 | **182 turns** |

The Read→Edit loop is **not** tight: the agent reads a file, explores for ~15 turns, then edits it.
So the re-anchor is ~8.6k already-cached tokens per transition, and at the tier this corpus is
actually billed (≈90% `cache_read`, 0.1× fresh) the saving needs **~180 turns** to repay it — on
sessions that run 122. `stale_at_depth` is therefore **net negative on real traffic** and ships off.
(The re-anchor figure is an upper bound: two transitions in one turn re-anchor the same suffix once.
Halving it still leaves break-even beyond the session length.)

#### `cold_cache` — the one place it does pay

On a turn whose prompt cache has provably expired there is no prefix to disturb, so the depth
restriction is free. Replaying a **single** SWE-bench request with `cold_cache: true`
(`CG_IDLE=430`, `frozen=0`, `attempted=100%` — so `removed < attempted`, a real saving and not
cache invalidation):

| request | before | after | removed | $@cache_write | $@cache_read |
|---|--:|--:|--:|--:|--:|
| last turn of the session | 37,329 | 25,005 | **12,324 (33.0%)** | **$0.0308** | $0.0025 |
| mid-session turn | 21,883 | 13,634 | **8,249 (37.7%)** | $0.0206 | $0.0016 |

A third of a cold SWE-bench request is stale file Reads. Cache-write is the correct tier for a cold
request, so that is a genuine **+$0.031 for one request, at zero LLM cost** — this component makes
no model calls. It applies only on turns idle past the provider TTL, which is why `cold_cache` is
off by default like every other component's.

!!! danger "Do not read the summed cold replay as a saving"
    Replaying all 122 requests with `CG_IDLE=430` reports `removed=782,038 (28.5%)`, `$1.96`@write —
    against `attempted=37,329`. **`removed ≫ attempted` is cache invalidation, not saving.** That run
    fabricates a 430-second gap before *every* turn; real SWE-bench turns are seconds apart. The
    honest figure is the per-request one above.

### Cache stability

The component rewrites message **history**, which is the cached prefix of every later turn, so
byte-stability is the whole ballgame. Three properties, each with a test:

1. **The classification is monotone.** It is a function of strictly later events, and events only
   accumulate, so a Read goes fresh → stale/superseded **exactly once** and never flaps back.
   Ordinals are absolute `req.Input` indices and matching is strictly-earlier-only, so appending a
   turn can never reclassify an earlier Read — headroom's `cross_turn_dedup` invariant 1, pinned by
   `TestFrozenPrefixIsByteStable` on the real fixture with the tail gate on.
2. **The replacement is content-addressed and deterministic.** It is a pure function of
   (content, class, path, config); the marker key is `sha256(original)`. No map is iterated on the
   path that produces output — `schema.ToolCalls`'s map is only ever looked up by an ascending index
   — so Go's randomized map order cannot move a byte. `TestDeterministicAcrossProcesses` re-runs the
   transform in three **child processes** and compares a hash of the whole rewritten transcript.
3. **A decision, once made, is replayed forever.** New decisions are confined to the uncached tail
   (`Ctx.TailOnly`); once made they are frozen by content hash and re-applied on **every** later
   turn at any depth, so a message never flips offloaded → full → offloaded as the tail boundary
   moves past it (`TestFrozenDecisionReplaysForever`, 50 turns). A freeze the store has since
   dropped is re-derived at depth (`repairLostFreeze`) because the provider is already holding the
   offloaded bytes — there, leaving the body verbatim is the cache-destructive move.

Cache safety comes from **determinism**, not from "only touch the delta" — which is also why gating
this component on a per-turn condition would be a bug: applied on alternate turns it re-anchors on
every one. For any given session it is `stale_at_depth`/`cold_cache`-shaped ALWAYS or NEVER.

### Config

```yaml
pipeline: [readlifecycle]      # in no preset; enable explicitly
components:
  readlifecycle:
    min_tokens: 100            # only offload a Read above this many tokens
    stale: true                # offload a Read whose file was later edited
    superseded: true           # offload a Read re-read later at the same range
    bash_edits: false          # count narrow shell write forms as edits
    stale_at_depth: false      # offload a stale Read inside the cached prefix (net negative — above)
    cold_cache: false          # act at any depth on a turn idle past the provider cache TTL
    marker_mode: full          # full | summary | off
```

Gate reasons on `/stats`: `no_file_reads`, `fresh_read`, `non_text_blocks`,
`already_marked`, `kept_verbatim_after_expand`, `below_min_tokens`, `cached_prefix`, `marker_no_win`.

### Recommendation

**Leave it off.** It belongs in no preset: on warm traffic it removes 0 tokens, and the only knob
that would change that (`stale_at_depth`) is measured net-negative by ~2 orders of magnitude on the
break-even. Turn it on — with `cold_cache: true` — for a deployment running long **editing** agents
with real idle gaps, where it removes a third of a cold request for no LLM spend. Do not turn it on
for read-only investigation traffic: there it is pure latency (0.2–1.9 ms/req) for zero tokens.

See also: [Components overview](../components.md) · [`extract_llm`](extract_llm.md) ·
[Choose a preset](../reference/presets.md)
