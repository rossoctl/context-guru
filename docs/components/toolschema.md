# toolschema

!!! info "Reformat — semantically lossless, and a marker component"
    Strips JSON-Schema **annotation keywords** from every tool's `input_schema`. The set of
    inputs the schema accepts is unchanged, and so is everything the model is told about the
    tool. **Opt-in, in no preset** — because of where it sits in the prompt cache.

`toolschema` is the envelope lever that applies to *every* request, including the first: a
coding agent re-sends its whole tool catalogue on every call. On captured Claude Code traffic
that is **31 tools, 168 descriptions, 131 KB of JSON** — 40% of the median request body.

Like [`cachesplit`](cachesplit.md) it is a **marker**: `tools` is a top-level body field the
pipeline never sees (components operate on `messages`), so the rewrite is body-level and runs
in `apply`, gated on this name being present. The transform itself is
`reformat.CompactToolSchemas`.

## What it removes

The JSON Schema annotation vocabulary, recursively, from every schema node:

`$id` · `$schema` · `$comment` · `deprecated` · `examples` · `example` ·
`markdownDescription` · `readOnly` · `title` · `writeOnly`

### Why that is lossless — verified, not assumed

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

### The property-name trap

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

## Cache safety, and the break-even

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

### Why it is *not* gated on `Ctx.ColdCache`

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

### Determinism is load-bearing

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

### Verify-then-adopt

Because the rebuild normalizes key order, a tools array with *nothing* to strip still comes
back with different bytes — and adopting that would re-anchor the prefix for a saving of
zero. So the rewrite is taken only if it is **strictly smaller**, headroom's
`compact_lossless` discipline applied to the envelope. Real traffic hits this: schema-less
Anthropic server tools (`bash_20250124`, `web_search`) and any agent whose schemas carry no
annotations. Any parse problem also returns the body untouched.

## What is deliberately not implemented

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

## Configuration

None. It is a name in the pipeline:

```yaml
pipeline: [format, dedup, failed_run, cmdfilter, cachesplit, toolschema]
```

It reports `Skipped` on any request where the strip did not run or found nothing, via
`Ctx.ToolSchema` — set in `apply` *before* the pipeline runs, so `/stats`, the Prometheus
component counters and the dashboard all agree (the mistake
[`cachesplit`](cachesplit.md) records).
