---
name: context-guru-cache-strategy-picker
description: Show the cache keep-alive strategy for IBM Bob, and explain why only none is offered. Use when asked about cache keep-alive, idle pings or keeping the cache warm for Bob.
---

# Cache strategy for Bob

Run this command. It only reads:

```sh
python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" configure --show
```

Report `cache_strategy=` (it is `none`) and `allowed_cache_strategies=` (only `none`). The command
reads that list from the proxy's strategy table. If it prints
`cache_strategy_error=strategy_table_unreadable` in place of `allowed_cache_strategies=`, the proxy
is too old: tell the user what `cache_strategy_detail=` says. Explain `cache_note=` in plain words:

- A keep-alive ping sends the last request again while the user is idle, so the provider keeps its
  prompt cache. context-guru does this only for Anthropic and OpenAI Responses caches.
- Bob's model call is OpenAI chat/completions. The proxy does not ping that, so `5-min-ping` and
  `1-hour-head` would do nothing for Bob.
- Bob has no SessionEnd event. A ping could not stop when a Bob task ends, and would spend the user's
  quota on a closed task.

So `none` is the only choice for Bob, and it is the default. If the user asks for another strategy,
do not run a command; say why. If a command refuses a name with
`reason=cache_strategy_not_supported_by_plugin`, the proxy has the strategy for Bob's route, but this
plugin cannot write it yet. To set `none` explicitly, run:

```sh
python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" configure --cache-strategy none
```
