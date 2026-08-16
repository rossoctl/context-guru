# Agent compaction: Claude Code, Bob, and context-guru

Both Claude Code and Bob compact their own conversations when context fills. This page
answers three questions about that, with the evidence for each:

1. **Does context-guru's reduction delay the agent's own compaction?** Yes, for both
   agents, and honestly — no cooperation, no patched client, no misreported numbers.
2. **Can we stop the agent compacting?** For Bob, yes. For Claude Code, no — and the
   only mechanism that *would* work is one we deliberately refuse to ship.
3. **Does context-guru keep working after the agent compacts?** It does now. Before the
   fixes below it degraded the summary on every compaction, split the session in the
   dashboard, and — in one reachable case — could disable Claude Code's auto-compact for
   the rest of the session.

Everything here was established by reading the shipped clients on this box: Claude Code
**2.1.215** (`GIT_SHA 316ce996…`, build `2026-07-19`) and **bobshell 1.0.6**. Both ship as
single-file bundles with their JS embedded, so the citations are symbol names and byte
offsets rather than line numbers. Reproduce with:

```bash
# Claude Code
dd if=~/.local/share/claude/versions/2.1.215 bs=1M skip=180 count=80 > /tmp/cc.bin
grep -aoE 'function eC\(.{0,400}' /tmp/cc.bin

# Bob
grep -aoE '.{200}getLastPromptTokenCount.{300}' \
  ~/.nvm/versions/node/*/lib/node_modules/bobshell/bundle/bob.js
```

---

## 1. Both agents count tokens from the API response, not from their own transcript

This is the finding everything else rests on, and it was not obvious — the plausible
assumption is that an agent counts its own local transcript, in which case a proxy could
never influence it.

### Claude Code

The auto-compact gate is `wWg`, and the number it tests comes from `eC`:

```js
function eC(e,t){ let r=BLu(e); if(!r) return $R(e,t); return w0e(r.usage)+$R(e.slice(r.anchorIndex+1),t) }
function w0e(e){ return e.input_tokens + (e.cache_creation_input_tokens??0)
                      + (e.cache_read_input_tokens??0) + e.output_tokens }
function T0e(e){ if(e?.type==="assistant" && "usage" in e.message …) return e.message.usage }
```

`BLu` walks back to the most recent assistant message carrying a `usage` object — that is
the **API response's** usage, stored on the transcript entry — and `w0e` sums it. `$R` is a
local estimator applied *only* to the messages after that anchor, normally one user turn.

So the quantity compared against the threshold is the upstream's own report of what it
billed, plus a small local tail. Claude Code treats the API figure as authoritative and
its local estimate as the approximation; the same helpers feed the statusline and the
prefix-overflow warning.

**The threshold:**

```
compact fires at   min(modelWindow, configuredWindow) − min(maxOutputTokens, 20000) − 13000
```

167,000 on a 200k model; 967,000 on a 1M model — which matches the hard-coded arm table
(`fMu = {"claude-sonnet-5": {… default: 967000}}`), a useful independent check on the
formula.

### Bob

Same conclusion, different code. `Gue.compress` gates on:

```js
let d = e.getLastPromptTokenCount();
let S = await s.getCompressionThreshold() ?? uua;      // uua = 0.5
if (d < S * lx(o)) return {compressionStatus: Xp.NOOP};
```

and `lastPromptTokenCount` is assigned in exactly one place, the stream-consume loop:

```js
d.usageMetadata.promptTokenCount !== void 0 && (this.lastPromptTokenCount = d.usageMetadata.promptTokenCount)
```

with `usageMetadata` built straight off the response body: `let _ = t.usage, S = {promptTokenCount: _?.prompt_tokens || 0, …}`.

Bob compacts at **50%** of its context window (default `uua = 0.5`), summarizing the
oldest 70% and keeping the newest 30% (`dua = 0.3`), split at a user-turn boundary.

Two local-estimate caveats, both real and both minor. Bob seeds
`lastPromptTokenCount = JSON.stringify(history).length / 4` in the `MY` constructor — and
because compaction calls `startChat(newHistory)`, which builds a fresh `MY`, that local
seed is in force for exactly one turn after every compaction, until the next response
overwrites it. Bob also has a separate local overflow guard that estimates only the *new*
message with chars/4 and subtracts it from API-derived headroom; the proxy shrinks the
API-derived half, so it relaxes correctly too.

### What this means, stated carefully

Because the trigger reads the upstream's usage for the request we actually sent,
**removing N tokens from a forwarded request causes the agent's own compaction to fire
later.** We do not touch usage on the way back: `proxy/usage.go` only *reads* it, and the
response is replayed byte-for-byte (`proxy/proxy.go` `writeRaw`). Nothing is misreported.

Two honest qualifications, both of which we would rather state than have someone discover:

- **The claim depends on the upstream reporting usage for the reduced request.** That is
  the normal behaviour of any OpenAI- or Anthropic-compatible backend, and we have
  verified it end to end through the IBM LiteLLM gateway. But it is a property of the
  backend, not of the agent, and a gateway that re-expanded or pre-counted would break it.
- **We only reduce `messages`.** `schema.MessagesTokens` counts `req.Input` — not the
  system prompt, not the tool definitions. On a real Claude Code session measured on this
  box, `messages` were ~9,100 tokens while total billed input was ~64,100: the other
  ~55,000 are Claude Code's tool schemas and system prompt, which we cannot touch. Early
  in a session our savings are therefore a small fraction of the number the threshold
  tests. **The effect grows as it matters**: system and tools are fixed while `messages`
  grow, so by the time a session approaches 167k the transcript dominates the bill and
  our reduction applies to nearly all of it.

---

## 2. Can we stop the agent compacting?

### Bob: yes

`Config.getCompressionThreshold()` has **no clamp**:

```js
async getCompressionThreshold(){
  if(this.compressionThreshold) return this.compressionThreshold;
  … let e = this.experiments?.flags[CONTEXT_COMPRESSION_THRESHOLD]?.floatValue;
  if(e !== 0) return e }
```

Whatever the setting says wins. `model.compressionThreshold` in `~/.bob/settings.json`
(legacy name `chatCompression`; requires restart) raises or lowers the trigger — `0.9`
compacts at 90%, and a large value makes the comparison always true, effectively
disabling it. There is **no environment variable** for this; an exhaustive grep of
`(BOB|BOBSHELL|GEMINI)[A-Z_]*(COMPRESS|CONTEXT|TOKEN|WINDOW)[A-Z_]*` finds only OTel
metric names. The remote experiment flag exists but is reachable only on the Google Code
Assist auth path, so it is inert for gateway users.

`/compress` (alias `/summarize`) forces compaction regardless and cannot be disabled.

### Claude Code: no, not upward

Every knob goes through `Math.min`, so it can only *lower* the threshold:

| Knob | Effect |
|---|---|
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, settings `autoCompactWindow`, `/autocompact` | `{window: Math.min(configured, modelWindow)}` — **cannot raise** |
| `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` | `Math.min(floor(window × pct/100), window − 13000)` — **cannot raise** |
| `DISABLE_COMPACT`, `DISABLE_AUTO_COMPACT`, settings `autoCompactEnabled: false` | disable entirely |
| `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | honoured **only** when `DISABLE_COMPACT` is set |
| `CLAUDE_CODE_MAX_OUTPUT_TOKENS` | shrinks the output reserve, capped at 20,000 |

Claude Code's own UI says so: *"The actual threshold is the minimum of this setting and
your model's maximum context window."*

### The mechanism we refuse to ship

Claude Code trusts response `usage` with **zero validation** — `T0e` does no plausibility
check and `w0e` just sums the fields. A proxy could under-report `input_tokens` in
`message_start` and suppress compaction indefinitely.

**We do not do this and will not.** It is fraud on the user's own numbers: the same fields
feed `/cost`, the statusline, and Claude Code's `blocked` hard-limit check. Under-report
past the real window and the client never compacts, so instead of a summary the user gets
the upstream's `prompt_too_long` — with no recovery path, because the client's safety
check was the thing we lied to. It is recorded here because it is the obvious shortcut and
someone will propose it; the honest reduction in §1 already produces the delay.

---

## 3. What broke after an agent compacted, and what we fixed

Four defects, all found by this investigation, all now fixed. Each is a real failure
scenario rather than a theoretical one.

### Fixed: we were compacting the compactor

**The bug.** The pipeline applied to *every* `POST /v1/messages`, including the agent's own
summarization request. Both agents send that to the same endpoint —
Claude Code via its normal messages route, Bob via
`/inference/v1/chat/completions` (`createChatCompletion`: `this.isAuthnBackend() && (n="/inference/v1/chat/completions")`).

So the summarizer was asked for verbatim fidelity over text we had already replaced with
`<<cg:HASH>>` stubs. Claude Code's compaction prompt asks for *"full code snippets"* and
that *"security-relevant instructions … MUST be preserved verbatim"*. Bob's is blunter:

> You are the component that summarizes internal chat history into a given structure.
> … This snapshot is CRITICAL, as it will become the agent's *only* memory of the past.
> All crucial details, plans, errors, and user directives MUST be preserved.

**Why it was the worst kind of bug.** It fired on *every* compaction, unconditionally, and
it was not recoverable by expansion — the content was never in the summary to begin with.
The agent then discards the pre-compaction history, so the loss is permanent. A user would
experience it as "the agent got noticeably stupider after compacting" with nothing in any
log to explain why.

**What is actually implemented.** Exactly two substring matches, against the **last
message only** when its role is `user` (`proxy/agentcompaction.go`):

| Agent | Needle |
|---|---|
| Claude Code | `Your task is to create a detailed summary of the ` |
| Bob | `First, reason in your scratchpad. Then, generate the ` |

Both are deliberately truncated before any character `encoding/json` escapes, because the
match runs against the raw message JSON — Bob's full sentence ends `<state_snapshot>.`,
which arrives as `<state_snapshot>`, so matching the whole sentence would fail
on the wire.

**Last-message-only is a safety property, not an optimisation.** A whole-body match would
latch: the phrase would sit in the transcript forever and disable compaction for the rest
of the session. Scoped to the last message, a false positive costs one request.

!!! warning "Two things this guard does NOT do"
    Stated because the earlier version of this page implied otherwise, and a reader would
    reasonably have taken the table as a description of the shipped code.

    - **Bob's system-prompt signal is not used.** Its compression instruction also puts
      `You are the component that summarizes internal chat history into a given structure.`
      into `messages[0]`, which would additionally cover upstream Gemini CLI forks — but
      matching `messages[0]` is whole-body matching, and that latches. One slot, one rule.
    - **Bob's other internal side-prompts are not covered.** Bob sends its shell-safety
      judge (`You are a security analyst evaluating shell commands for auto-execution
      safety.`), its PR-description generator, its web-fetch summarizer and its JSON-mode
      calls to the same endpoint, and all of them are compacted today. They are worth
      carving out — unlike compaction, none of them is unrecoverable — but they are not
      carved out yet.

    A related consequence worth knowing: the needle above is quoted **on this page**, so an
    agent that reads this documentation can land it in a trailing `tool_result` and bypass
    that one request. That is a lost-savings event, not a correctness one.

### Fixed: we could disable Claude Code's auto-compact for a whole session

The expand tool was injected based on *"does this request already carry tools"*, not on
*"does this request contain any markers"*. Claude Code's compaction request carries its
full normal tool set, so the tool was advertised on it. If the summarizing model called it
and nothing resolved, the raw `tool_use` was replayed to the client; Claude Code found no
text, treated the compaction as failed — and **three consecutive failures trip a circuit
breaker** (`hOu = 3`, *"autocompact: circuit breaker tripped after N consecutive failures
— skipping future attempts this session"*). After that auto-compact is off and the session
walks into the upstream's hard `prompt_too_long`.

This is Claude-Code-specific: Bob sends no `tools` array at all, so auto-injection never
fired for it.

The same inconsistency had a second symptom — the tool was advertised on marker-free
requests while SSE was only inspected when markers were present, so a hallucinated
`context_guru_expand` call escaped to a client that has no such tool. Advertising and
interception are now one condition, which is what they always should have been.

### Fixed: one conversation showed up as two sessions

`sha256(system + firstUser)[:16]` was the session id whenever no explicit id was given —
and compaction changes the first user message, so the id flipped mid-conversation. For Bob
it flips for *two* independent reasons: its `startChat()` → `xat()` regenerates the
environment boilerplate including **a fresh recursive folder listing**, so any session
that created or renamed a file gets a different `system` head too.

Consequences: the dashboard split one conversation in two (breaking exactly the
before/after session view), and every session-keyed cache reset — dedup memory,
`extract_llm` results, frozen offload decisions, the summarize checkpoint.

There was also a **cost** consequence, which is the part that makes this more than a
display bug: `Tracker.Turn(sessionID, …)` returns turn 1 for a new key, so the
cached-prefix boundary resets to 0 and the age/supersession offloaders are turned loose on
a prefix the provider has already cached — forcing a full cache-write of the
post-compaction prefix at ~11.5× the read price.

**The fix** reads a stable id the agents were already sending and we were ignoring:

```
x-context-guru-session header  →  metadata.user_id (Claude Code)  →  metadata.taskId (Bob)  →  sha256(system+firstUser)
```

One detail here is worth recording, because the obvious implementation would have silently
done nothing. Claude Code's `metadata.user_id` is **not** a UUID — it is a ~170-byte JSON
object string:

```json
"metadata": {"user_id": "{\"device_id\":\"<64 hex>\",\"account_uuid\":\"\",\"session_id\":\"2e168312-…\"}"}
```

The session-id sanitiser is a strict allow-list (`[A-Za-z0-9._:-]`, ≤128 bytes), so that
value is rejected wholesale — a fix that passed it through raw would have left the bug in
place while appearing to fix it. We unwrap `session_id` from it. Verified against 1,887
captured request bodies: 50 distinct session ids, one spanning 128 consecutive turns, and
stable across compaction. Bob's `metadata.taskId` is a bare `randomUUID()`, re-rolled only
by `/clear` and *not* by compaction, so it needs no normalisation.

### Known limitation: markers are dead ends for Bob

Bob's request body has **no `tools` key**, and forcing injection does not help: Bob parses
tool calls as **XML tags out of the streamed content text**, not from `tool_calls` deltas.
An injected `context_guru_expand` would be silently ignored while still costing a
prompt-cache prefix miss.

**Consequence: for Bob, any lossy component leaves a permanently unexpandable
`<<cg:HASH>>` marker.** Prefer lossless components (`format`, `toon`, `cachesplit`) and
offloaders whose stubs are self-describing for Bob sessions, and treat any recommendation
to add `mask` for long Bob sessions with that in mind. Fixing this properly means teaching
the expand affordance to ride in the text channel; the `tools`-array mechanism cannot work
for Bob at all.

---

## What is still unverified

Stated explicitly so nobody cites this page for more than it establishes.

- **No live observation of Claude Code's `autocompact:` debug line.** The `eC` chain is
  read from source and is used by all three call sites, but we did not watch a real
  session cross the threshold. `claude --debug` on a long session, comparing the logged
  `tokens=` against the proxy's reported reduced `input_tokens` for the same turn, would
  settle it.
- **No live observation of either agent actually crossing a compaction through the
  proxy.** The wire shapes are read from the bundles.
- **Bob's regenerated folder listing is proven to be regenerated, not proven to differ**
  in a specific real session. It is derived from live disk, so it differs whenever the
  agent has touched a file.
- **`vL(model).contextWindow`** for the model ids Bob resolves in practice was not
  enumerated; the fallback is 1,048,576. This moves the absolute trigger point, not the
  mechanism.
