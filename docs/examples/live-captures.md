# Examples

Every example on this page is **captured live** — a real tool-output message sent through a running
`context-guru-proxy`, showing the exact bytes the proxy forwarded upstream (marker hashes and all).
Nothing here is hand-written illustration.

!!! info "How these were produced"
    A proxy was pointed at a local echo upstream that records each forwarded request body, and driven
    with `POST /anthropic/v1/messages`. Each component ran in an isolated one-line pipeline with its
    thresholds lowered so a small, readable payload fires. The LLM component (`extract_llm`) used a
    real cheap model (`aws/claude-haiku-4-5`) via `model.source: config` (`CHEAP_MODEL_*`). Anthropic
    tool outputs ride in `tool_result` blocks; `apply` normalizes them so components see them uniformly.

    ```sh
    # each capture: isolated pipeline, thresholds lowered, real payload
    ./bin/context-guru-proxy --config one-component.yaml   # ANTHROPIC_UPSTREAM=<echo or gateway>
    curl -s localhost:4000/anthropic/v1/messages -H 'content-type: application/json' \
      -H "Authorization: Bearer $KEY" -d @payload.json
    ```

## Reformat (lossless)

### `format` — pretty JSON → compact JSON
```text
before:  {                                after:  {"id":1,"name":"ada","nested":{"a":1,"b":2},"tags":["x","y"]}
             "id": 1,
             "name": "ada",
             "tags": [ "x", "y" ],
             "nested": { "a": 1, "b": 2 }
         }
```

### `toon` — uniform JSON array → TOON
```text
before:  [{"id":0,"name":"pod-0","status":"Running"}, … 6 objects … ]
after:   [6]{id,name,status}:
         0,pod-0,Running
         1,pod-1,Running
         … 4 more rows …
```

### `cacheinject` — adds a cache breakpoint (opt-in, in no preset)
A `cache_control` directive is attached to the last content block of a chosen message (no
model-visible content changes). The policy marks the newest message plus the last one still
matching the previous turn:

```json
{"role":"assistant","content":[{"type":"text",
  "text":"Here is a fairly long answer … worth caching across turns.",
  "cache_control":{"type":"ephemeral"}}]}
```

## Offload (lossy, reversible)

Every offloaded original is stashed and recoverable — see [reversibility](#reversibility-verified).

### `dedup` — byte-identical repeat → pointer
```text
after (2nd copy):  [identical to an earlier tool output] <<cg:0abfad591bbe1c9a>>
```

!!! note "Captured before #281"
    The note now names the producing command and carries an expand cue — e.g.
    ``[repeat of earlier `git diff`; expand <<cg:0abf…>>]``. The capture above is left as it
    was recorded.

### `extract` — deterministic noise collapse
```text
before:  resolved 200 packages / warning: peer dependency unmet ×15 / blank runs / build complete
after:   resolved 200 packages
         warning: peer dependency unmet          ← 15 → 1
         build complete in 4.2s
         <<cg:40b571fdebccdcd4>> [full output: call context_guru_expand]
```

### `extract_llm` — LLM-written filter (live `aws/claude-haiku-4-5`)
Query: *"find the auth timeout error and nearby context"* — 120 near-identical request lines around
one error line, projected to the error plus surrounding context:
```text
after:   2024 GET /users/58 200 12ms
         2024 GET /users/59 200 12ms
         ERROR auth timeout on token refresh
         2024 GET /items/0 200 8ms
         2024 GET /items/1 200 8ms
         [auth timeout error + context; repetitive successful requests elided]
         <<cg:923fff04ab267215>> [full output: call context_guru_expand]
```

## Savings, verified

Driving a realistic agent transcript (repeated large tool outputs, several old file reads) through
the `general` preset against a real gateway, `GET /stats` reported:

```json
{ "requests": 2, "tokens_before": 24021, "tokens_after": 259, "savings_pct": 98.9,
  "components": {
    "dedup":       { "acted": 1, "saved_tokens": 11958 },
    "extract_llm": { "acted": 1, "saved_tokens": 2264 } },
  "llm_calls": 1, "llm_input_tokens": 5463, "llm_output_tokens": 451 }
```

`dedup` is deterministic; `extract_llm` made one real cheap-model call. Components that
found nothing to do (`format`, `toon`, `cmdfilter`, `extract`) sit in
`top_passthrough` at zero cost — the pipeline is never worse. `cachesplit` is there too, but
permanently and by design: its `Reformat` always skips because the split it enables is a body-level
rewrite in `apply`.

## Reversibility, verified

Every marker is recoverable. Taking any marker above and calling the HTTP recovery route:

```sh
curl -s 'localhost:4000/expand?id=c6a32e9e9c9fcf1f'
# → the full 200-line original, byte-for-byte (6,579 chars, 200 lines)
```

In normal operation the model recovers it itself via the injected `context_guru_expand` tool. See
[Recover offloaded context](../how-to/recover-context.md).

See also: [Components overview](../components.md) · [Benchmark component internals](../results/components.md)

---

## Before → After showcase

One input context run through each of the three [llm-d compaction service](llm-d-service.md)
configs, showing what each did to the same `messages` array. Captured with the store disabled and
`marker_mode: off`, so outputs carry no `<<cg:…>>` markers and are drop-in inference-request bodies.

!!! note "About this capture"
    The configs here are the committed ones with triggers lowered so this small readable example
    actually fires. The two LLM configs used **`claude-haiku-4-5`** via `model.source: config`
    (`CHEAP_MODEL_*` env).

### The numbers

| Config | messages | body bytes |
|---|---|---|
| **before** (shared input) | 11 | 2667 |
| **TOON** (`format` + `toon`, no LLM) | 11 | 2376 |
| **Extract** (`strategy: code`, LLM) | 11 | 2145 |
| **Summarize** (LLM) | 4 | 1298 |

TOON and Extract keep all 11 messages and shrink content in place. Summarize is the outlier — it
collapses 11 messages to 4 by restructuring the transcript.

### The shared input

The task: *"List the admins, then fix the failing test_col_insert."* The 11-message transcript
carries a 6-user JSON array (tool `c1`), 14 lines of `col_insert` grep hits (`c2`), the buggy
implementation (`c3`), and a failing pytest run (`c4`), then a final user question:
*"Which users are admins, and what's the fix?"*

### What each config did

=== "TOON"

    `11 → 11 messages, 2667 → 2376 bytes` — deterministic, no LLM.

    `toon` rewrote the user-list JSON array in tool `c1` to **TOON**: the field names are declared
    once as a header, then one compact row per element. Everything else is untouched.

    ```text
    before (tool c1):
        [{"id": 1, "name": "Alice", "role": "admin", "active": true}, {"id": 2, ...}, ...]

    after (tool c1):
        [6]{active,id,name,role}:
        true,1,Alice,admin
        false,2,Bob,member
        true,3,Carol,admin
        true,4,Dave,member
        false,5,Eve,admin
        true,6,Frank,member
    ```

    Lossless repack: same values, fewer tokens. The grep hits, implementation, and pytest run
    pass through unchanged.

=== "Extract"

    `11 → 11 messages, 2667 → 2145 bytes` — LLM (`claude-haiku-4-5`), `strategy: code`.

    `extract` had a cheap LLM write a sandboxed, deletion-only filter tuned to *this* content and
    the current query. It made two content-specific cuts:

    - **tool `c1`** — filtered the 6-user array down to just the **three admins** (Alice, Carol,
      Eve), verbatim, dropping the non-admin members.
    - **tool `c3`** — trimmed the implementation to the buggy lines, dropping the 12 lines of
      `# helper line N` noise below the bug.
    - **tool `c4`** — dropped the trailing `1 failed, 180 passed` summary line, keeping the
      failure and the `IndexError` traceback.

    ```text
    after (tool c1):
        [{"active":true,"id":1,"name":"Alice","role":"admin"},
         {"active":true,"id":3,"name":"Carol","role":"admin"},
         {"active":false,"id":5,"name":"Eve","role":"admin"}]
    ```

    Deletion-only: the model can trim but provably cannot fabricate or reword.

=== "Summarize"

    `11 → 4 messages, 2667 → 1298 bytes` — LLM (`claude-haiku-4-5`).

    `summarize` collapsed the middle of the transcript into a single
    **`=== History Summary ===`** system message, keeping the head and the last couple of turns.
    The result is `[system, <History Summary>, tool c4, final user question]`.

    ```text
    [1] system:
        === History Summary ===
        The earlier trajectory is summarized below.

        <summary>
        • The user requested two tasks: (1) list the admins, (2) fix the failing test_col_insert
        • The assistant listed users, searched col_insert refs, read the impl, ran the failing test
        • All tool outputs were masked, so specific results are unavailable
        </summary>

        Use this summary as the older context ... Continue the task accordingly.
    ```

    The summarizer is grounded in the current task (the first user turn + recent turns are passed
    as "summarize toward this"), not a blind digest. It changes the message count — the one
    component that does — so it runs alone.

See how these configs are wired in the [llm-d compaction service](llm-d-service.md) guide.
