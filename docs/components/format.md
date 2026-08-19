# format

!!! info "Reformat — lossless"
    Re-encodes a pretty-printed JSON tool output as compact JSON — same value, fewer whitespace tokens.

## How it works

`format` only acts on tool messages whose trimmed text starts with `{`/`[`, is valid JSON, is
≥ `min_tokens`, and gets smaller. It repacks the value as compact JSON, stripping the indentation
and newlines that dominate a pretty-printed payload's token cost. v1 is json-compact only (a TOON
encoder is planned — see [`toon`](toon.md)).

### Envelope descent

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

## Before → After

```
before:  { "id": 1,           after:  {"id":1,"name":"ada","tags":["x","y"]}
           "name": "ada",
           "tags": [ "x", "y" ] }
```

## Lossiness

None — nothing stashed. The value is identical; only whitespace tokens are removed.

One case is refused for exactly that reason: a `json.Decoder` reads **one** value and ignores
whatever follows, so an output that is a JSON document plus anything else (a `jq` document with a
stderr line after it, or an NDJSON stream) would be "compacted" to its first document with the rest
silently deleted. `format` declines such an output. [`toon`](toon.md) declines it too.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `min_tokens` | 50 | Skip tool outputs smaller than this token count. |

## When it shines

Verbose pretty-printed JSON/MCP payloads, including the pretty-printed payload inside a Claude Code
tool-runner envelope.

## When it's inert

Already-compact JSON (at both levels), non-JSON text, more than one document in one output, small
outputs. **Measured:** of 1,748 distinct tool outputs across every capture available here, 1,724 are
not JSON at all and `format` fires on none of them — its value is in JSON/MCP-shaped traffic, not in
a coding agent's own output. [`textclean`](textclean.md) is the reformatter for the other 1,724.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
