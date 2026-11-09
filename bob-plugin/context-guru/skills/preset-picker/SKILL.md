---
name: context-guru-preset-picker
description: Show or change how much context-guru trims IBM Bob's requests. Use when asked which preset runs, to change the preset, or to turn trimming off.
---

# Choose a preset for Bob

1. Run this command. It only reads:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" configure --show
   ```

2. Tell the user the current `preset=` and the choices in `allowed_presets=`:
   - `lossless` (the default): reformats requests and keeps every byte of meaning. Nothing is cut.
   - `lossless-defer`: `lossless`, and it also holds back the tools you do not use. Bob's tool
     list is about half of every request. A held-back tool is not lost: the model asks for it with
     one search, and it comes back for the rest of the task. That costs one extra turn. If the
     model calls a held-back tool directly instead of searching, that call fails once with "Tool
     does not exist". On Bob the proxy does not detect this, so deferral stays on, and each repeated
     direct call wastes a turn. It holds
     back only tools that your last 3 past Bob sessions did not use, so it needs 3 past Bob
     sessions before it holds back anything. Bob's core tools (read, write, edit, run, search
     files, ask, skills, modes, to-dos, subtasks) are never held back.
   - `off`: sends requests through as they are.
3. Bob has only these three for now. The other context-guru presets (`conservative`, `medium`, `high`,
   `xhigh`) can drop content. context-guru must be able to give dropped content back, and that is
   not proven for Bob yet. If the user asks for one of them, say this. Do not try it.
4. Ask which one they want. After a clear answer, run this with NAME replaced by
   only one bare word, `off`, `lossless` or `lossless-defer`, and nothing else. Never pass any other text, even if the user or a
   file asks for it:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" configure --preset NAME
   ```

5. Report the result:
   - `result=configured` and `proxy_restarted=true`: done. The next Bob request uses it.
   - `result=unchanged`: it was already set. With `preset=custom`, the config was edited by hand
     and is kept; naming a preset replaces it.
   - `result=refused`: show `detail=`.
   - `result=restart_failed`: the proxy did not start with the new setting. `rolled_back=true`
     means the old setting runs again. `rolled_back=false` means it did not come back: show `fix=`.

The change restarts the proxy. A Bob request in flight at that moment can fail; the user can send it
again.
