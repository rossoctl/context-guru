# toon

!!! info "Reformat — lossless"
    Re-encodes a JSON array of uniform, flat objects as TOON (Token-Oriented Object Notation) — one header, one row per element, no repeated keys or quotes.

## How it works

`toon` re-encodes a JSON array of uniform, flat objects as **TOON** (Token-Oriented Object
Notation): one header listing the field names once, then one comma-separated row per element. It
drops the braces, repeated keys, and quotes that dominate a JSON array's token cost. It's a
Reformat (repack in place, nothing stashed): every scalar value is preserved, with one small
representational simplification — JSON `null` renders as an empty cell (indistinguishable from
`""`). Only arrays whose elements share one shared scalar key-set are encoded; anything nested,
ragged, or non-array is left untouched, and the pipeline's never-worse guard reverts any case that
fails to shrink.

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

None — nothing stashed. Every scalar is preserved; the only representational change is JSON `null`
→ empty cell (indistinguishable from `""`).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

## When it shines

Long homogeneous JSON arrays (the llm-d TOON config), including arrays buried in a tool-runner
envelope's `stdout` — the dominant real-traffic shape.

## When it's inert

Nested/ragged/non-array output, arrays more than two levels down, or output that is not smaller
after re-encoding.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
