# When your agent compacts

Claude Code and Bob both summarize their own conversation when it grows too long for the
context window. context-guru is designed to make that happen later, and to stay correct
when it does happen.

## What context-guru does about it

**It pushes compaction further out.** Both agents decide when to compact from the token
usage the provider reports for the request that was actually sent. context-guru sends a
smaller request, so the reported usage is smaller and the threshold arrives later. Usage
figures themselves are never touched — the response is replayed to the agent byte for
byte, so `/cost`, the statusline and your provider bill all still agree.

**It never compacts the compaction request.** The agent's summarization request demands
verbatim fidelity over the whole transcript, so the pipeline steps aside for it and
forwards it untouched. Detection is a match on the opening of the agent's own
summarization prompt in the last message of the request, and it covers Claude Code and Bob
Shell (`proxy/agentcompaction.go`). Nothing else the agent sends to the same endpoint is
treated specially.

**A compaction does not split your session.** The session id comes from a stable value the
agent already sends (`x-context-guru-session`, then Claude Code's `metadata.user_id`, then
Bob's `metadata.taskId`), so one conversation stays one session in the dashboard and keeps
its dedup memory, frozen offload decisions and cache boundary across the compaction.

## Can you turn agent compaction off?

| Agent | Answer |
|---|---|
| **Bob** | Yes. Set `model.compressionThreshold` in `~/.bob/settings.json` (restart required) — `0.9` compacts at 90% instead of the default 50%, a large value disables it in practice. `/compress` still forces it on demand. |
| **Claude Code** | No. Every threshold knob is clamped to at most the model's window, so it can only compact *earlier*. `DISABLE_AUTO_COMPACT` turns it off entirely, but then a long session walks into the provider's hard context limit with no summary to fall back on. |

## Bob: prefer lossless components

Bob cannot expand a `<<cg:HASH>>` marker — it has no way to call the expand tool (see
[Integrations](../integrations.md#use-it-with-an-agent-bob-bobshell)). Any lossy component
therefore leaves content permanently unrecoverable in a Bob session. Use lossless
components (`format`, `toon`, `cachesplit`) with Bob, and add lossy ones only where losing
that content is acceptable.

<details markdown="1">
<summary>Troubleshooting</summary>

**My savings look tiny early in the session.** context-guru only reduces `messages`. The
system prompt and tool schemas are the bulk of a fresh Claude Code request (roughly 55k of
64k billed input tokens on a measured session), and they are not ours to touch. As the
transcript grows it comes to dominate the request, so the reduction applies to almost all
of it by the time the compaction threshold is in sight.

**Compaction still fires at the same point.** The delay depends on the upstream reporting
usage for the *reduced* request. Any OpenAI- or Anthropic-compatible backend does; a
gateway that pre-counts or re-expands the body before billing would not. Compare the
`input_tokens` in the response against what your agent sent to check.

**One request stopped being compacted for no reason.** The compaction-detection phrase can
appear in ordinary content — for example if the agent reads a file that quotes it. When
that happens the request is forwarded untouched and the next turn resumes normally. The
cost is that request's savings, nothing else.

**A conversation appears twice in the dashboard.** Send an explicit
`x-context-guru-session` header. Without any agent-supplied id the fallback is a hash of
the system prompt plus the first user message, and both of those change when the agent
compacts.

**Bob's numbers jump for one turn after a compaction.** Bob seeds its own token estimate
locally right after building a new chat, and that estimate is in force until the next
response arrives. It corrects itself on the following turn.

</details>

See also: [Reversibility & recovery](recover-context.md) ·
[Measuring savings](measure-savings.md)
