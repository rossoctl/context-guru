# Reformat components

Six lossless, config-free-or-simple components that repack a tool output or the request
envelope without dropping anything: **format**, **toon**, **textclean**, **searchfold**,
**toolschema**, **toolfilter**. The first four verify-then-adopt on tool-output content; the
last two are **marker** components that rewrite the top-level `tools` array in `apply`, ahead
of `system` and `messages` in the provider's prompt-cache hash.

---

## format

!!! info "Reformat — lossless"
    Re-encodes a pretty-printed JSON tool output as compact JSON — same value, fewer whitespace tokens.

### How it works

`format` only acts on tool messages whose trimmed text starts with `{`/`[`, is valid JSON, is
≥ `min_tokens`, and gets smaller. It repacks the value as compact JSON, stripping the indentation
and newlines that dominate a pretty-printed payload's token cost. v1 is json-compact only (a TOON
encoder is [`toon`](#toon), below).

#### Envelope descent

Claude Code tool results do not arrive as bare JSON: the tool runner wraps them, and the payload
the agent reads is a JSON document **escaped inside a string field**.

```
{"ok": true, "exit_code": 0, "stdout": "{\n  \"total\": 50, ... \"tasks\": [ {…} x50 ]}"}
```

Re-encoding only that outer object is worth nothing: measured on real traffic it saved **9 tokens
of 6,459 (0.1%)**, and `not_json_shaped`/`already_compact` accounted for 98.4% of candidates. Of the
large low-reduction JSON blobs, **673/673 carried their payload in a `stdout` string field**.

So `format` descends **one level**: after parsing the object, any string field that cheaply looks
like JSON (leading `{`/`[` after trimming, ≥ 64 bytes) is parsed, compacted, and written back into
the field, correctly re-escaped. `stdout` is the measured case but the field name is not
special-cased. Fields it does not transform are re-emitted byte-exact (they are held as raw JSON,
and HTML escaping is off), so the envelope still parses for the agent and its siblings (`ok`,
`exit_code`, …) are unchanged. If the descent finds nothing, or the payload does not shrink, the
whole blob is left untouched and a gate reason is reported (`envelope_no_embedded_json`,
`envelope_inner_not_smaller`).

### Before → After

```
before:  { "id": 1,           after:  {"id":1,"name":"ada","tags":["x","y"]}
           "name": "ada",
           "tags": [ "x", "y" ] }
```

### Lossiness

None — nothing stashed. The value is identical; only whitespace tokens are removed.

One case is refused for exactly that reason: a `json.Decoder` reads **one** value and ignores
whatever follows, so an output that is a JSON document plus anything else (a `jq` document with a
stderr line after it, or an NDJSON stream) would be "compacted" to its first document with the rest
silently deleted. `format` declines such an output. `toon` declines it too.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

### When it shines

Verbose pretty-printed JSON/MCP payloads, including the pretty-printed payload inside a Claude Code
tool-runner envelope.

### When it's inert

Already-compact JSON (at both levels), non-JSON text, more than one document in one output, small
outputs. **Measured:** of 1,748 distinct tool outputs across every capture available here, 1,724 are
not JSON at all and `format` fires on none of them — its value is in JSON/MCP-shaped traffic, not in
a coding agent's own output. [`textclean`](#textclean) is the reformatter for the other 1,724.

---

## toon

!!! info "Reformat — lossless"
    Re-encodes a JSON array of uniform, flat objects as TOON (Token-Oriented Object Notation) — one header, one row per element, no repeated keys or quotes.

### How it works

`toon` re-encodes a JSON array of uniform, flat objects as **TOON** (Token-Oriented Object
Notation): one header listing the field names once, then one comma-separated row per element. It
drops the braces, repeated keys, and quotes that dominate a JSON array's token cost. It's a
Reformat (repack in place, nothing stashed), so every scalar value is preserved **and stays
distinguishable from every other**. Only arrays whose elements share one scalar key-set are
encoded; anything nested, ragged, or non-array is left untouched, and the pipeline's never-worse
guard reverts any case that fails to shrink.

#### The cell grammar

Ambiguity is resolved by **quoting**, not by refusing the table:

| cell | means |
|---|---|
| *(bare, empty)* | `null` |
| `""` | the empty string |
| `1`, `-3e10` | a number, byte-exact (no float round-trip) |
| `true` / `false` | a boolean |
| `"1"`, `"true"`, `"1.50"` | a **string** that would otherwise read back as a number or a bool |
| `"a,b"`, `"he said ""hi"""` | a string carrying a delimiter or a quote (CSV-style) |

An earlier version refused a whole array over one such cell, which was expensive — record arrays
are 89% of the measured envelope mass — and it still did not make the encoding safe: a cell holding
a literal newline was quoted and emitted anyway, breaking the row split.

#### Verify then adopt

Every candidate table is **decoded again** before it is used, and adopted only if the result
reproduces the input exactly (numbers included). Losslessness is therefore a property `toon`
checks per call, not one this page claims. A shape the encoder cannot represent — a cell with a
literal newline, say, which quoting cannot rescue because rows are newline-separated — costs a
declined table, never a corrupted one.

#### Envelope descent

The record array is almost never at the top level of a tool result — it sits inside a JSON document
**escaped in a string field**, the same shape `format`'s envelope descent handles. `toon` used to see
only the outer object, report `not_uniform_object_array` and never descend — which is why it looked
inert. Measured on real traffic: of the large low-reduction JSON blobs, **673/673 carry their payload
in a `stdout` string field, and 537 of those (2,098,762 tokens, 89% of that mass) contain a
repeated-record array** — the 72.8% `not_uniform_object_array` rate.

`toon` now descends into the payload, bounded at **two levels** and no deeper:

1. any string field that cheaply looks like JSON (leading `{`/`[`, ≥ 64 bytes) is parsed — `stdout`
   is the measured case, but the field name is not special-cased;
2. if that payload *is* the record array it is encoded directly; if it is a wrapper object
   (`{"total":50,"tasks":[…]}`) the array-valued field is encoded and the TOON text replaces it as
   a JSON string value, so the payload still parses as JSON.

Everything it does not encode is re-emitted byte-exact, and if nothing shrinks the whole blob comes
back untouched with a gate reason (`envelope_no_embedded_json`, `envelope_inner_not_smaller`,
`not_uniform_object_array`).

### Before → After

```
before:  [{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]
after:   [2]{id,name}:
         1,Alice
         2,Bob
```

### Lossiness

None — nothing stashed, and proven per call rather than asserted: a table is adopted only if
decoding it reproduces the input value exactly (see *Verify then adopt* above). Every scalar,
including `null` and the empty string, comes back as itself.

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

### When it shines

Long homogeneous JSON arrays (the llm-d TOON config), including arrays buried in a tool-runner
envelope's `stdout` — the dominant real-traffic shape.

### When it's inert

Nested/ragged/non-array output, arrays more than two levels down, cells holding a literal newline
or carriage return, or output that is not smaller after re-encoding. **Retired from every preset
2026-08:** of 1,748 distinct tool outputs across every capture available here (SWE-bench,
Terminal-Bench and Claude Code sessions), 1,724 are not JSON at all and **not one is a JSON object
array** at any level — 0 acts in 5,752 production requests, 0 convertible candidates in 11.67M
measured tokens, at 1.53 ms + a `TextTokens` call per tool message. The component and its tests
stay, so tabular MCP-style traffic can enable it by hand.

---

## textclean

!!! info "Reformat — lossless"
    Strips terminal display control out of plain-text tool output: ANSI escape sequences and `\r` progress redraws. No marker, no stash, no floor.

### How it works

Most tool output is not JSON. Measured over **1,748 distinct tool outputs** from every capture
available here (SWE-bench, Terminal-Bench and Claude Code sessions), **1,724 are plain text** — so
[`format`](#format) and [`toon`](#toon), which both require a JSON document, never see them.

`textclean` is the lossless Reformat for that mass. On a tool message that is ≥ `min_tokens` it
removes two kinds of pure terminal display control, and nothing else:

- **ANSI/VT100 escape sequences** — colour, cursor moves, OSC strings. Display control, never
  content. `grep --color=always`, coloured `pytest`, `npm` and `cargo` output are full of them.
- **Carriage-return redraws** — a progress line rewritten in place. The bytes before the last `\r`
  on a line were overwritten before anything displayed them, so keeping only the final rendered
  segment loses nothing the agent could ever have read. A **trailing** `\r` is the CRLF separator
  surfacing after a split on `\n`, not a redraw: CRLF text comes back byte-identical.

Both transforms already existed inside [`extract`](offload-reducers.md#extract), an **Offload** — so
they cost a `<<cg:HASH>>` marker, a store round-trip and, above all, they were refused below
extract's 300–400 token floor. As a Reformat there is no marker, no stash and no floor.

#### Verify then adopt

A candidate is adopted only if **every informative line survives it byte-identical** (each line
that is non-blank once display control is resolved, in the same order) *and* the result is strictly
smaller in tokens. Anything else leaves the message exactly as it arrived, with a gate reason.

### Before → After

```
before:  \x1b[35m/src/app.go\x1b[m\x1b[36m:\x1b[m\x1b[32m41\x1b[m\x1b[36m:\x1b[m  return \x1b[1;31mnil\x1b[m
         downloading:  10%\rdownloading:  60%\rdownloading: 100%

after:   /src/app.go:41:  return nil
         downloading: 100%
```

### Lossiness

None, and nothing is stashed. Stripping an escape sequence is one-way but non-semantic (there is
nothing to restore — it was never content), and a redraw's overwritten bytes were never displayed.
The runtime check above is what enforces that; the tests prove it over **real** terminal output
(a real `grep --color=always` run and a real `\r` progress writer), not hand-typed fixtures.

Three neighbouring transforms are deliberately **out of scope**:

- dropping progress-bar lines and collapsing repeated lines/blocks remove whole lines, so they need
  `extract`'s stash to stay recoverable — that is `extract`'s job, not this one's;
- trimming trailing whitespace changes meaning in a unified diff (a context line whose only content
  is its leading space) and in markdown (the two-space line break);
- collapsing runs of blank lines is safe but **measured worthless**: 21 tokens over the whole
  1,748-output corpus, against 16,721 for the ANSI strip and 498 for the redraws.

### Measured

`TestCorpusMeasureReformatters` / `TestCorpusMeasureTextCleanBreakdown` in
`components/reformat`, run against a capture with `CG_CORPUS` (captures are never committed):

| | outputs | tokens |
|---|--:|--:|
| corpus | 1,748 | 1,077,024 |
| `textclean` fires | 12 (0.7%) | 18,694 → 1,475 (**−17,219**, 1.60% of the corpus) |
| of that: ANSI strip | 9 | −16,721 |
| of that: `\r` redraws | 3 | −498 |
| below extract's 400-token floor (saving nobody takes today) | 10 | −1,353 |
| above it (same saving, now with no marker or stash) | 2 | −15,866 |

It fires rarely and pays hugely when it does: on the outputs it touches it removes **92%** of the
tokens, because escape-sequence bytes tokenize badly.

### Cache posture

Deterministic — a pure function of the content, so the same output is rewritten to the same bytes
on every turn and after a process restart. No `TailOnly` gate and no freeze are needed, exactly
like [`format`](#format).

### Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

### When it shines

Coloured search/build/test output, and any progress-bar-writing command.

### When it's inert

Output with no escape sequences and no interior carriage return — which is most output, and costs a
single regex probe (`no_terminal_noise`).

### Presets

Shipped in **`general`**, right after the JSON reformatters. It is deliberately **not** added to
`codesmart` / `codesafe`: those name-lists are the SWE-bench study's published arms, and
`config/config.go` states that a change to them is a reason to re-measure rather than a
documentation edit.

---

## searchfold

!!! info "Reformat — lossless"
    Folds the repeated path prefix out of search output — `rg`/`grep -rn` hit lists and
    `find`/`ls -1`/`rg -l` path lists — by emitting each path (or its parent directory) once as a
    heading with the rows beneath it.

### How it works

Routing is by **content**: the fold is attempted on every tool output and kept only when it
round-trips, because the fold self-verifies so a misroute costs CPU and never correctness. It is
lossless *by construction*, not by argument: every fold has an exact inverse, and a fold is
adopted only when applying that inverse reproduces the input **byte for byte** and the result is
strictly smaller. Anything else — output already grouped by file, a content line that reads as a
path, a basename ending in `/` — is declined and passes through untouched. No path, line number
or line is ever dropped, so there is nothing to stash and no marker.

It used to pre-gate on the producing command, and that gate was measured a strict loss on 1,795
real captured requests: 234,722 tokens folded with it against **333,764 without**, at 1.174
ms/request against 0.509. It declined 29,737 candidate messages, 99,042 tokens of which had
exactly the repeated path prefix this folds — the pairing says which command *ran*, not what its
output looks like, and resolving one `json.Unmarshal`s a whole argument object per tool message.
The general rule for a self-verifying fold: attempt it, keep what round-trips. A cheap *shape*
pre-check still earns its keep (`format`'s `not_json_shaped` is a one-byte test guarding a full
parse); one that has to reconstruct request *structure* does not.

### Before → After

```
before:  pkg/a.go:12:foo      after:   pkg/a.go
         pkg/a.go:31:foo               12:foo
         pkg/b.go:7:foo                31:foo
                                       pkg/b.go
                                       7:foo
```

### Lossiness

None — nothing stashed, and the fold is only adopted when its own inverse reproduces the input
byte for byte.

### Configuration

`min_tokens` (50).

### When it shines

`rg`/`grep -rn` hit lists and `find`/`ls -1`/`rg -l` path lists with a repeated path prefix.
Measured on 466 real captured search-command outputs (Terminal-Bench + SWE-bench): it fires on
81 of them and takes those from 21,088 to 14,410 tokens (**−31.7%**), which is −7.0% across all
search-command output. Shipped in every preset since 2026-08 — it's part of **the lossless
trio** that leads every working pipeline (with `format` and `textclean`), since verify-then-adopt
components carry no risk argument for omitting them.

### When it's inert

Single-file `grep -n`, prose, already-grouped output.

---

## toolschema

!!! info "Reformat — semantically lossless, and a marker component"
    Strips JSON-Schema **annotation keywords** from every tool's `input_schema`. The set of
    inputs the schema accepts is unchanged, and so is everything the model is told about the
    tool. **Opt-in, in no preset** — because of where it sits in the prompt cache.

`toolschema` is the envelope lever that applies to *every* request, including the first: a
coding agent re-sends its whole tool catalogue on every call. On captured Claude Code traffic
that is **31 tools, 168 descriptions, 131 KB of JSON** — 40% of the median request body.

Like [`cachesplit`](caching.md#cachesplit) it is a **marker**: `tools` is a top-level body field the
pipeline never sees (components operate on `messages`), so the rewrite is body-level and runs
in `apply`, gated on this name being present. The transform itself is
`reformat.CompactToolSchemas`.

### What it removes

The JSON Schema annotation vocabulary, recursively, from every schema node:

`$id` · `$schema` · `$comment` · `deprecated` · `examples` · `example` ·
`markdownDescription` · `readOnly` · `title` · `writeOnly`

#### Why that is lossless — verified, not assumed

The list comes from headroom's `proxy/tool_schema_compaction.py:44`, whose justification is
one sentence ("removing them does not change the set of valid inputs"). Checked against the
specifications rather than taken on trust:

| Keyword | Where it is defined | Effect on validation |
|---|---|---|
| `title`, `description`, `default`, `deprecated`, `readOnly`, `writeOnly`, `examples` | json-schema-validation §9, "Basic Meta-Data Annotations" | None. The section states these keywords "do not affect validation"; `examples` in particular is explicitly *not* `enum` — §9.5 says it places no constraint and an instance need not match any listed value. |
| `$comment` | json-schema-core §8.3 | None, and implementations "MUST NOT" derive behaviour from it. |
| `example`, `markdownDescription` | Not JSON Schema at all — OpenAPI 3.0 and VS Code respectively | Ignored by every validator; unknown keywords are annotations by §6.5 of core. |
| `$id`, `$schema` | json-schema-core §8.1–8.2 | **Not annotations.** `$id` sets the base URI references resolve against; `$schema` declares the dialect. |

Two consequences of that last row:

* **`$id`/`$schema` are only dropped when the tool's schema contains no reference.** If
  `$ref`, `$dynamicRef`, `$anchor` or `$dynamicAnchor` appears anywhere in it, both are kept
  — dropping the base URI could change what a relative reference resolves to. On real traffic
  no Claude Code tool uses `$ref`, so all 31 lose their `$schema`.
* Dropping `$schema` is safe *because the declared dialect is the one the consumer assumes
  anyway* (draft 2020-12), not because the keyword is inert. That is a narrower claim than
  headroom's and it is the honest one.

`description` and `default` are **kept**, even though the spec calls them annotations too:
they are the annotations the *model* reads. Removing them would change what it knows about a
tool, which is a different kind of loss from the one this component claims.

#### The property-name trap

`title` is a keyword in a schema node and a perfectly legal *property name* inside a
`properties` map. A recursion that deletes the word wherever it appears silently deletes a
real parameter of the tool. headroom special-cases this at `tool_schema_compaction.py:230`
("skip the drop under `properties`").

`toolschema` handles it **positionally** instead: recursion descends only into the three
shapes a JSON Schema keyword can hold a subschema in —

* *name → schema* maps: `properties`, `patternProperties`, `$defs`, `definitions`,
  `dependentSchemas` — values are pruned, **keys are never inspected**;
* single subschemas: `items` (and its draft-07 tuple form), `additionalProperties`,
  `propertyNames`, `contains`, `not`, `if`/`then`/`else`, `unevaluated*`, `contentSchema`;
* subschema lists: `allOf`, `anyOf`, `oneOf`, `prefixItems`.

Anything else is left byte-for-byte alone, which buys a second correctness property free:
`const`, `default` and `enum` hold arbitrary **instance data**, and an object sitting in a
`default` may legitimately have a `title` field. A "skip under `properties`" rule still
corrupts those; a positional rule cannot reach them.

Both cases are pinned by tests — `properties named title/examples/readOnly survive` (which
also declares a property called `$schema`) and `instance data under const/default/enum is
never touched` — plus a per-tool property-name check across every captured request.

### Cache safety, and the break-even

`tools` renders **first**, ahead of `system` and `messages`, so changing a byte here
invalidates the entire cached prefix — once, and then never again. With the provider's
multipliers (cache read `R = 0.1x`, 5m cache write `W = 1.25x`, uncached `1.0x`), a prefix of
`P` tokens and a saving of `s` tokens:

**Cold start** (nothing cached). The prefix is written either way, so the transform costs
nothing and saves `W·s = 1.25s` on the spot, plus `R·s = 0.1s` on every later request of the
session. Net-positive at request 1.

**Warm** (an entry for the untransformed prefix exists). The re-anchor replaces a read with a
write:

```
one-time cost  = W·(P − s) − R·P = (W − R)·P − W·s = 1.15·P − 1.25·s
recurring gain = R·s = 0.1·s per later request
break-even n   = (1.15·P − 1.25·s) / (0.1·s)  ≈  11.5 · P/s
```

Measured (`TestCompactToolSchemasRealCaptures`, tokens counted with `internal/tokens`, not
bytes/4):

| Capture | Tools/req | Tokens saved/req | % of request input | Break-even (warm) |
|---|--:|--:|--:|--:|
| `bench/long.jsonl` | 31 | 473 | 0.55% of 85,348 | 2,062 requests |
| `bench/mixed.jsonl` | 31 | 473 | 0.62% of 75,967 | 1,834 requests |
| `bench/short.jsonl` | 31 | 473 | 0.83% of 57,233 | 1,378 requests |
| `bench/cold.jsonl` | 31 | 473 | 0.83% of 56,926 | 1,371 requests |
| `cg-runs/capture-swebench.jsonl` | 24 | 389 | 1.29% of 30,210 | 880 requests |

Real sessions are tens of requests, so **a warm prefix never pays this back.** That is the
whole shape of the component: it is worth having on a prefix nobody has cached yet, and it is
a loss on one that is warm.

#### Why it is *not* gated on `Ctx.ColdCache`

The obvious reading of the table above — "apply it only on cold turns" — is a bug. A prefix
transform must be all-or-nothing over a session. Applied on cold turns only, request 1 sends
the compacted `tools` and request 2 sends the original, so request 2 re-anchors, request 3
re-anchors back, and the `1.15·P` penalty is paid **every other turn, forever**. The only
safe shapes are *always* and *never*.

So the component is unconditional while enabled, and **off in every default preset**. The
one-time re-anchor is then paid once per *deployment* — by whichever sessions happen to be in
flight when an operator enables it — rather than once per session, and from then on every
session's first request is strictly cheaper. Flipping it on mid-sweep is the one way to lose
money with it, which is why it is an operator decision rather than a default.

#### Determinism is load-bearing

If the output were not byte-identical for identical input, the prefix would re-anchor on
*every* request and the component would be a pure loss. Two things make it stable, and both
are enforced rather than assumed:

* the result is memoized by SHA-256 of the incoming `tools` array (31 schemas per call, and
  the array is identical on every turn of a session, so the walk runs once per catalogue);
* the rebuild goes through `encoding/json`, whose object encoder **sorts keys**. Go map
  ranging is randomized, so a ranged rebuild would emit a different byte order in every
  process. `TestCompactToolSchemasByteStable` asserts equality across 400 in-process calls
  *and* across five child processes, since same-process repetition cannot detect a
  per-process seed.

Numeric literals are decoded with `json.Number`, so a bound never gets silently reformatted
(`1.0` stays `1.0`).

#### Verify-then-adopt

Because the rebuild normalizes key order, a tools array with *nothing* to strip still comes
back with different bytes — and adopting that would re-anchor the prefix for a saving of
zero. So the rewrite is taken only if it is **strictly smaller**, headroom's
`compact_lossless` discipline applied to the envelope. Real traffic hits this: schema-less
Anthropic server tools (`bash_20250124`, `web_search`) and any agent whose schemas carry no
annotations. Any parse problem also returns the body untouched.

### What is deliberately not implemented

**headroom's L2/L3 — truncating tool descriptions, and dropping "self-explanatory" ones.**
These are lossy: they change what the model is told a tool does, and the failure mode is a
wrong tool call rather than a visible error. They default to disabled even in headroom, and
no shipped preset there enables them. They are also the tempting lever — 168 descriptions are
110 KB of the 131 KB of `tools`, i.e. ~25x what L1 recovers — which is exactly why they need
a measured accuracy story before a byte of them ships, not a size argument.

**`description` whitespace normalization.** Measured on the same corpus: collapsing runs of
whitespace saves 844 bytes (0.6% of the tools array) and does it by flattening the markdown
lists, tables and fenced blocks agent tool descriptions are written in — so it is not
semantically inert, despite looking like it. The genuinely inert form (strip trailing spaces
per line, collapse 3+ blank lines, trim) saves **19 bytes out of 110,401**. Not worth a line
of code, and it is not in the component.

### Configuration

None. It is a name in the pipeline:

```yaml
pipeline: [format, dedup, failed_run, cmdfilter, cachesplit, toolschema]
```

It reports `Skipped` on any request where the strip did not run or found nothing, via
`Ctx.ToolSchema` — set in `apply` *before* the pipeline runs, so `/stats`, the Prometheus
component counters and the dashboard all agree (the mistake [`cachesplit`](caching.md#cachesplit)
records).

---

## toolfilter

!!! info "Reformat — a marker component"
    Drops the tool and MCP-server declarations an account has explicitly opted to stop sending.

| | |
|---|---|
| Kind | Reformat (marker: the rewrite lives in `apply`, on the top-level `tools` array) |
| Lossy? | Yes, deliberately and by name — a removed declaration is not sent |
| Reversible? | Yes: delete the name from `remove`. Re-anchors the prefix once (~$0.70) |
| In any preset? | **No.** It has no effect without an explicit list, and the list is a decision only the account can make |
| Config | `remove: [<declaration name> ...]`, plus `mcp__<server>` for a whole MCP server |

### When it helps

Your agent re-sends its whole tool catalogue on every turn. Measured over 1,844 real request
bodies: **21,889 declared tokens per session, of which 18,107 (82.7%) belong to declarations that
no session ever invoked.** 27 items were declared by every session and called by none. That's the
largest single lever in this project — one to two orders of magnitude above anything in
[the 2026-08 results](../results/measured-2026-08.md) — and also the one that most easily turns
into a correctness bug, so it is **opt-in, per account, per name, and never inferred**.

### How to configure it

```yaml
pipeline: [format, dedup, extract, cachesplit, toolfilter]
components:
  toolfilter:
    remove: [CronCreate, CronDelete, CronList, Workflow, mcp__playwright]
```

Names are exactly as the inventory reports them. `mcp__<server>` (no tool half) removes a whole MCP
server, which is the unit you actually added and the unit you'd remove.

On the hosted service you don't hand-write that: the dashboard's **Inventory** page has a switch per
declaration, which posts to `POST /api/toolfilter` (`{kind, name, server, action:"exclude"|"include"}`).
`GET /api/toolfilter` is the read half:

```json
{
  "enabled": true,
  "excluded": [{"kind": "tool", "name": "Workflow", "since": 1755000000000}],
  "realized": {"reads": 446390, "usd": 0.1186, "priced": true, "requests": 35, "since": 1755000000000},
  "suggestions": [{"name": "CronCreate", "tokens": 812, "sessions": 6, "days": 10,
                   "projected_usd": 0.004, "basis": "declared but never invoked across 6 of your sessions since 2026-06-01 (10 days)"}],
  "min_sessions": 5, "min_days": 7, "withheld": 4, "coverage": {"sessions": 21, "captured": 20, "not_captured": 1}
}
```

A candidate is only suggested once it was declared in at least 5 distinct captured sessions spanning
7+ days, and invoked in none of them — the count guards against a fluke, the span against "one
afternoon's task didn't need it."

Any signed-in account can switch off declarations it added itself (MCP tools/servers, other
non-Claude-Code-native tools) — those are on your own bill. Switching off one of Claude Code's
**own** tools is different: it takes away equipment the model is expected to have, so that write
answers 403 for a plain account and needs a manager.

### Before / after

Replayed over real captured Claude Code traffic, with the removal list an account would actually
arrive at from its own inventory:

| capture | requests | removed per request | tokens avoided | avoided |
|---|---|---|---|---|
| `long.jsonl` (one session) | 35 | 12,754 | 446,390 | **$0.1186** |
| `capture-tb.jsonl` | 73 | 12,854 | 938,342 | **$0.2764** |

`tools` is hashed at position 0 of the cached prefix, so any edit here re-anchors it. From a
session's first request the filter is free (that request is a cold start anyway); turning it on or
off mid-session costs one re-anchor (measured ~$0.70 for a 147k-token prefix). Prefer flipping it
between sessions rather than mid-task. A transcript that already called a tool you've since removed
keeps working — that history isn't touched, only future declarations are.

### What it will refuse to remove

**Any name still mentioned in the system prompt's prose.** Claude Code's system prompt describes
some tools in English (e.g. "Prefer dedicated tools over Bash (Read, Edit, Write)..."). Stripping a
tool's declaration while that sentence survives can make the model try to narrate the call instead
of emitting it — nothing errors, you just get a worse agent. Since we can't safely edit hand-written
prose, the rule is conservative: **a declaration mentioned in the prose is kept, whatever the
configuration says.** On Claude Code's own catalogue this keeps `Agent`, `Bash`, `Edit`, `Read`,
`Skill`, `Write` no matter what you list.

Also always kept: the tool `tool_choice` forces on that request (removing it would turn a forced
call into a 400), provider-side tools declared by `type` rather than schema, and the last remaining
tool.

**Skills are removable too**, by a different mechanism (a skill is prose inside a system message,
not a `tools[]` entry) — list it as `skill__<name>`. Unlike a tool, an over-removed skill fails
*open*: a model that names an unlisted skill anyway will simply run it, since the `Skill` tool takes
a free-form string. So this switch stops you paying for a skill's declaration; it doesn't forbid
using it. To actually disable one, use Claude Code's own `skillOverrides`.

### Not done, and why

- **Truncating or dropping tool *descriptions*** rather than whole declarations — the one lever
  whose failure mode is a wrong tool call.
- **Per-name attribution of the realized saving** — `realized` is one total per request/account, not
  broken out per removed name.
- **Letting a non-manager set their own list** for Claude Code's own tools — that's the compaction
  configuration, and it's a manager's field on purpose.

See also: [Components overview](../components.md) · [Choose a preset](../reference/presets.md)
