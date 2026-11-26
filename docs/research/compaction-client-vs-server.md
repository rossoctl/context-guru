# Compaction: client-side vs. server-side

Research note, 2026-11-09. It compares how Claude Code and Codex CLI compact context, and
recommends which kind to use.

*Compaction* replaces the older turns of a conversation with a summary, so a long session stays
inside the context window.

- *Client-side* compaction: the agent (the client) makes the summary call itself and rewrites its
  own history.
- *Server-side* compaction: the model provider writes the summary inside its API and returns it
  as a block or item that the client sends back on later requests.

## Summary

- **Use the provider's server-side compaction for that provider's own models.** On Anthropic,
  prefer on-demand compaction to threshold compaction.
- **Keep the client-side steps that do not need a model:** trim old tool outputs first, keep
  recent user messages word for word, reload instructions from disk, and trigger at 85–90%.
- **Keep a client-side fallback** for other providers and for when server compaction fails.
- **For Context Guru:** a proxy can read a Claude summary (plain text) but not a Codex remote
  summary (encrypted).

## How each tool compacts

### Claude Code: client-side

- Claude Code makes the summary call itself and swaps the summary in for the old history.
- It clears old tool outputs first. If it still needs room, it sends one summary request.
- After the summary, it reloads `CLAUDE.md`, recent files, the task list and recent skills from
  disk.
- Third-party sources put the trigger at about 95% of the context window.
- You can steer the summary with `/compact <focus>`.

### Codex CLI: server-side by default, client-side as fallback

- **Remote v2 (default on OpenAI and Azure):** Codex sends a normal `/responses` request with a
  `compaction_trigger` item. The server returns one **encrypted** `compaction` item.
- **Remote v1:** a separate call to `/responses/compact`. Used only when v2 is off.
- **Local path (all other providers):** Codex makes its own summary call. Custom compaction
  prompts work only on this path.
- Codex keeps real user messages word for word: up to 64k tokens on remote v2, 20k on the local
  path.
- It reloads developer instructions, `AGENTS.md` and environment context fresh, instead of
  summarizing them.
- Auto-compaction triggers at 90% of the window (`model_auto_compact_token_limit`). A higher
  value is ignored.
- Measured: one session went from 174,871 to 3,368 tokens. That compaction took 34 s.

### Anthropic API: server-side options

| | On demand | At a token threshold | Your own summarizer |
|---|---|---|---|
| Who decides when | You | The API, at the trigger | You |
| Code you write | A compaction loop | One parameter | The call, the prompt, the rewrite |
| Keeps recent turns word for word | Yes | Yes, with `pause_after_compaction` | Yes |
| Runs in the background | Yes | No | Yes |
| Kept turns keep their thinking | Yes, with conditions | No | No |

- Both server modes are beta. Threshold: `compact-2026-01-12`, trigger default 150k, minimum
  50k. On demand: `compact-2026-09-04`.
- The summary comes back as **plain text**. The server keeps nothing.
- Anthropic's docs say: "Use on-demand compaction wherever it is available."

## Benefits of server-side compaction

1. **The model is trained for it.** The provider can tune the model to write and resume from its
   own summaries. A client cannot copy this.
2. **Less code.** One parameter. No summary prompt, no history rewrite.
3. **It works mid-turn.** A long tool loop keeps going. Codex v2 and Anthropic threshold
   compaction both do this.
4. **The provider improves it** with no client change.

## Benefits of client-side compaction

1. **It works with any provider.** Codex uses its local path for every non-OpenAI model.
2. **You can read and audit the summary.** The Codex blob is opaque. A researcher also showed it
   acts as a long-lived token for the conversation: other accounts accepted it after 50 days.
3. **You control the prompt and the timing.** For example, compact while the user is idle, or
   before the cache expires.
4. **Cheap work first.** Drop old tool outputs by rule before you pay for an LLM summary.

## What is the same either way

- **One extra model call.** Anthropic bills it and counts it against rate limits. Its usage is in
  `usage.iterations`, not in the top-level token counts.
- **One cache rewrite.** The new summary sits at the start of the history, so the cache prefix
  after it is written again.
- **Information loss grows with each compaction.** Codex warns users about this.

## Recommendation

1. Use the provider's server-side compaction for that provider's models. On Anthropic, choose
   on-demand over threshold.
2. Keep the client-side steps that do not depend on the model:
   - Trim old tool outputs before you summarize.
   - Keep recent user messages word for word.
   - Reload instructions and memory from disk instead of summarizing them.
   - Trigger at 85–90%, not 95%. Several sources say 95% is often too late.
3. Keep a client-side fallback for other providers and for failed server compactions
   (see openai/codex#22107).

### What this means for Context Guru

- The proxy can see and change a Claude Code summary, because it is plain text.
- It cannot read a Codex remote-v2 summary, because it is encrypted.
- Proxy-side features, such as compaction timing or offload, therefore have more to work with on
  the Claude side.

## Open questions

- No first-party source says whether Claude Code now uses the server-side compaction API. The
  client-side description comes from Claude Code's docs and from community analysis.
- A blog claims about 80% remote-compaction failures in Codex with GPT-5.5 (May 2026). This is
  not verified.

## Sources

- [Anthropic: Compaction overview](https://platform.claude.com/docs/en/build-with-claude/compaction)
- [Anthropic: Compaction at a token threshold](https://platform.claude.com/docs/en/build-with-claude/compaction-threshold)
- [Anthropic: Context editing](https://platform.claude.com/docs/en/build-with-claude/context-editing)
- [How Claude Code works](https://code.claude.com/docs/en/how-claude-code-works)
- [osolmaz: How Codex CLI compaction works (1,087 compactions)](https://gist.github.com/osolmaz/a38acf6e522df67530e3ed47c80fdcd5)
- [OpenAI: Unrolling the Codex agent loop](https://openai.com/index/unrolling-the-codex-agent-loop/)
- [OpenAI: Codex Prompting Guide](https://developers.openai.com/cookbook/examples/gpt-5/codex_prompting_guide)
- [openai/codex#10365: remote_compaction flag](https://github.com/openai/codex/issues/10365)
- [openai/codex#22107: remote compact failures](https://github.com/openai/codex/issues/22107)
- [badlogic: Compaction research, Claude Code / Codex / OpenCode / Amp](https://gist.github.com/badlogic/cd2ef65b0697c4dbe2d13fbecb0a0a5f)
- [SmartScope: Claude Code vs Codex compaction](https://smartscope.blog/en/blog/claude-code-codex-compact-comparison/)
- [Daniel Vaughan: Codex CLI compaction architecture](https://codex.danielvaughan.com/2026/03/31/codex-cli-context-compaction-architecture/)
- [Decode Claude: Inside Claude Code's compaction](https://decodeclaude.com/compaction-deep-dive/)
