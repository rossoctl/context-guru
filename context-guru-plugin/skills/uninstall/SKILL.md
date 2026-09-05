---
name: uninstall
description: Stop routing Claude Code through context-guru — remove the ANTHROPIC_BASE_URL key it added, stop the local proxy, and optionally delete the binary. Use when the user asks to uninstall, remove, disable, turn off, or stop context-guru, or says routing through it is breaking their sessions.
---

# Uninstall context-guru

Undo is the promise the install made, so it has to work on the first try and in the order
below. **Remove the routing before stopping the proxy** — the other way round leaves a window
where sessions are pointed at a port with nothing behind it, and every request in that window
fails.

If the user is here because something is broken, do step 1 first and explain afterwards.

## 1. Remove the routing key

Check all three scopes: the install may have written any of them, and a `--global` install
plus a per-project one can both exist.

```bash
PORT="${CLAUDE_PLUGIN_OPTION_PORT:-8787}"
for f in .claude/settings.local.json .claude/settings.json ~/.claude/settings.json; do
  [ -f "$f" ] && python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" remove \
      --file "$f" --url "http://127.0.0.1:${PORT}/anthropic"
done
```

The script removes the key only if it holds **our** base URL — the one passed as `--url`, or the
one it recorded at install time — and reports `result=conflict` instead of deleting a value the
user has since pointed somewhere else. If you see a conflict, leave it alone and tell them what
is there.

`--url` is worth passing (it also covers a port that changed since install), but it is **not**
what makes this safe, and the earlier version of this line said it was. That put the property
protecting the user's `ANTHROPIC_BASE_URL` in a prompt — i.e. dependent on this file being read
and followed. `settings.py` now refuses unconditionally: invoked with no `--url` at all it
removes only what it has a record of installing, and exits 2 over anything else. Deleting a
stranger's gateway is not a mistake a skill instruction should be the last line of defence
against.

Every change writes a timestamped backup — report those paths.

The removal takes effect in a **new session**; this one keeps the environment it started with.
Say so, or "I removed it and it is still routing" is the next message.

## 2. Stop the proxy

**Do not pattern-match the process.** An earlier version of this skill ran
`pkill -f "context-guru-proxy.*${PORT}"`, which was wrong in two ways at once: the starter passed
the port through the environment, so it appeared nowhere in the proxy's command line and the
pattern matched no proxy at all — while it *did* match the shell running the `pkill`, i.e. the
Bash tool of the session you are in. It killed the user's own session, reported nothing removed,
and left the proxy holding the port.

Use the pidfile the starter writes, and fall back to the socket's owner:

```bash
PORT="${CLAUDE_PLUGIN_OPTION_PORT:-8787}"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
PIDFILE="${STATE}/proxy-${PORT}.pid"

pid=""
[ -f "$PIDFILE" ] && pid=$(cat "$PIDFILE")

# Fall back to whoever holds the port — covers a proxy started by hand, or a stale pidfile.
if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
  if command -v lsof >/dev/null 2>&1; then
    pid=$(lsof -ti "tcp:${PORT}" -sTCP:LISTEN 2>/dev/null | head -1)
  elif command -v ss >/dev/null 2>&1; then
    pid=$(ss -lntpH "sport = :${PORT}" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2 | head -1)
  fi
fi

# The ownership check GATES the signal — one branch, so the order cannot be got wrong.
#
# This used to be two blocks: `kill "$pid"` here, and "confirm the PID is ours" as prose with its
# own snippet BELOW. Executed the way it reads, top to bottom, the signal was already sent by the
# time the guard was reached. It matters most on the lsof/ss fallback above, which exists for a
# stale pidfile or a hand-started proxy — i.e. exactly the cases where the PID may belong to
# something else, and a recycled PID satisfies `kill -0` perfectly well.
#
# Same shape as the defect that made uninstall kill the user's own session: a destructive command
# whose safety condition lived somewhere the reader got to afterwards.
if [ -z "$pid" ]; then
  echo "(nothing listening on ${PORT})"
elif ! ps -p "$pid" -o command= 2>/dev/null | grep -q context-guru-proxy; then
  echo "NOT OURS — pid ${pid} holds port ${PORT} and is not a context-guru-proxy; leaving it alone"
  ps -p "$pid" -o command= 2>/dev/null
else
  kill "$pid" && rm -f "$PIDFILE"
fi
```

If it reports **NOT OURS**, stop here and tell the user what is on the port. Do not kill it, and do
not remove the pidfile: something else owns that port, and the routing change alone already stops
this project using it.

Then confirm it is actually gone, rather than assuming the kill worked:

```bash
sleep 1
curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/healthz" && echo "STILL RUNNING" || echo "stopped"
```

If it is still running, report the PID and let the user decide. Do not escalate to `kill -9` on a
pattern, and never broaden the match to `context-guru-proxy` alone: on a host that also runs a
production instance or a benchmark arm, that takes those down too.

## 3. Offer, do not assume, the rest

Ask before either of these; neither is implied by "stop routing my sessions":

- **Delete the binary** — `rm ~/.local/bin/context-guru-proxy` (or wherever
  `command -v context-guru-proxy` reports).
- **Remove the plugin itself** — `/plugin uninstall context-guru@context-guru`. Until they do,
  both hooks (`SessionStart` and `UserPromptSubmit`) stay installed, and that is harmless: each
  self-gates on `ANTHROPIC_BASE_URL` and exits immediately in a project that is not routed — which,
  after step 1, is every project. Worth saying, so a leftover hook is not mistaken for a leftover
  proxy.
- **Delete the state directory** — `~/.local/state/context-guru` holds the pidfile and the
  dashboard database (session metadata and token counts, no prompt content unless they enabled
  content capture). Nothing reads it once the proxy is gone.

## 4. Confirm the end state

Report, in one short list: which files changed and their backups, that the proxy is stopped,
whether the binary and plugin are still present, and that the change lands in the next session.

If there are leftovers the user declined to remove, name them — an uninstall that quietly
leaves things behind is the reason people distrust installers.
