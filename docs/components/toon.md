# toon

!!! info "Reformat — lossless"
    Re-encodes a JSON array of uniform, flat objects as TOON (Token-Oriented Object Notation) — one header, one row per element, no repeated keys or quotes.

## How it works

`toon` re-encodes a JSON array of uniform, flat objects as **TOON** (Token-Oriented Object
Notation): one header listing the field names once, then one comma-separated row per element. It
drops the braces, repeated keys, and quotes that dominate a JSON array's token cost. It's a
Reformat (repack in place, nothing stashed), so every scalar value is preserved **and stays
distinguishable from every other**. Only arrays whose elements share one scalar key-set are
encoded; anything nested, ragged, or non-array is left untouched, and the pipeline's never-worse
guard reverts any case that fails to shrink.

### The cell grammar

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

### Verify then adopt

Every candidate table is **decoded again** before it is used, and adopted only if the result
reproduces the input exactly (numbers included). Losslessness is therefore a property `toon`
checks per call, not one this page claims. A shape the encoder cannot represent — a cell with a
literal newline, say, which quoting cannot rescue because rows are newline-separated — costs a
declined table, never a corrupted one.

### Envelope descent

The record array is almost never at the top level of a tool result. Claude Code's tool runner wraps
results, so the array sits inside a JSON document **escaped in a string field**:

```
{"ok": true, "exit_code": 0, "stdout": "{\n  \"total\": 50, ... \"tasks\": [ {…} x50 ]}"}
```

`toon` used to see the outer object, report `not_uniform_object_array` and never descend — which is
why it looked inert. Measured on real traffic: of the large low-reduction JSON blobs, **673/673
carry their payload in a `stdout` string field, and 537 of those (2,098,762 tokens, 89% of that
mass) contain a repeated-record array** — the 72.8% `not_uniform_object_array` rate.

`toon` now descends into the payload, bounded at **two levels** and no deeper:

1. any string field that cheaply looks like JSON (leading `{`/`[`, ≥ 64 bytes) is parsed — `stdout`
   is the measured case, but the field name is not special-cased;
2. if that payload *is* the record array it is encoded directly; if it is a wrapper object
   (`{"total":50,"tasks":[…]}`) the array-valued field is encoded and the TOON text replaces it as
   a JSON string value, so the payload still parses as JSON.

Everything it does not encode is re-emitted byte-exact, and if nothing shrinks the whole blob comes
back untouched with a gate reason (`envelope_no_embedded_json`, `envelope_inner_not_smaller`,
`not_uniform_object_array`).

## Before → After

```
before:  [{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]
after:   [2]{id,name}:
         1,Alice
         2,Bob
```

## Lossiness

None — nothing stashed, and proven per call rather than asserted: a table is adopted only if
decoding it reproduces the input value exactly (see *Verify then adopt* above). Every scalar,
including `null` and the empty string, comes back as itself.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

## When it shines

Long homogeneous JSON arrays (the llm-d TOON config), including arrays buried in a tool-runner
envelope's `stdout` — the dominant real-traffic shape.

## When it's inert

Nested/ragged/non-array output, arrays more than two levels down, cells holding a literal newline
or carriage return, or output that is not smaller after re-encoding. **Measured on real traffic:**
of 1,748 distinct tool outputs across every capture available here (SWE-bench, Terminal-Bench and
Claude Code sessions), 1,724 are not JSON at all and **not one is a JSON object array** at any
level, so `toon` fires zero times on that corpus. Its measured mass comes from tool-runner
envelopes in MCP-style workloads, not from a coding agent's own output.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
