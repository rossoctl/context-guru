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

**If they are here because Claude could not talk at all, they got here by luck** — the routing this
skill removes is what breaks the session that would run it. There is a plain-sh escape hatch for
that, installed outside the plugin at
`${XDG_STATE_HOME:-$HOME/.local/state}/context-guru/context-guru-reset` (also on `PATH` as
`context-guru-reset` when `~/.local/bin` is there). It restores every settings file the install
edited from a copy taken before the first edit, and needs no Claude, no network and no proxy. Point
at it when a user reports being stuck, and prefer it to this skill for anyone who is currently
locked out — it is `cp`, where this skill is a conversation.

## 1. Remove the routing key

Check all three scopes: the install may have written any of them, and a `--global` install
plus a per-project one can both exist.

Get the configured port first — `CLAUDE_PLUGIN_OPTION_*` reaches hook environments only, so a shell
default would build the wrong URL and the removal would find nothing to remove:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" config
```

Use its `option_port=`. **Read the fallback per option, not from `source=`:** that command prints an
`option_<name>=` line only for keys the user actually configured, and reports `source=(none)` only when
nothing at all is set — so somebody with a partial config gets a real `source=` and no `option_port=`
line. Any option the output does not list is unconfigured; use the `plugin.json` default for that one,
whatever `source=` says.

**The port is the exception, and 8787 is the wrong guess for it.** Ports are allocated per project —
one project per port, because two projects sharing one proxy made every session start kill and
restart it, wiping the in-memory store each time — so the port this project runs on is usually
neither 8787 nor anything the user ever typed. It is recorded, so ask for it instead of defaulting
it:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port show
```

`result=ok port=<n>` is this project's port, and it wins over `option_port=` when the two disagree —
that is the same order the allocator uses, and it is what the proxy was actually started on.
`port=(none)` means there is no record: only then fall back to `option_port=`, and to 8787 after
that. Then:

```bash
PORT="<port>"
for f in .claude/settings.local.json .claude/settings.json ~/.claude/settings.json; do
  [ -f "$f" ] || continue
  out=$("${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" remove \
      --file "$f" --url "http://127.0.0.1:${PORT}/anthropic")
  printf '%s\n' "$out"
  # The port OPTION goes with the routing key, in the same file, or the next session in this project
  # is worse off than before the uninstall. Claude Code resolves options project-local > project >
  # user, so a leftover project `port` outranks a machine-wide install's: `ANTHROPIC_BASE_URL`
  # resolves to the machine-wide port while the port option the hooks are handed resolves to the dead
  # one this uninstall just stopped. Both hooks self-gate on "does that URL name MY port", neither
  # matches, and both exit silently — so nothing starts the proxy and nothing reports why.
  # install.sh does the same `port unset` when it folds a project into a machine-wide install.
  #
  # `removed` ONLY, which is the single value that means the key really went. `remove` also answers
  # `conflict` (the file still routes somewhere — the user's own gateway — and taking the port
  # from under it would break that) and `unchanged` (this file was not routing to that port, or is not
  # there at all). Matching anything but `removed` strips the port out of files this loop never
  # unrouted: a pin the user typed in `.claude/settings.json` while routing lives in
  # `settings.local.json`, or — the third file here — the machine-wide install's own option, which
  # a single project uninstall has no business touching. `remove` refuses that file on its own now
  # (`reason=another_installs_routing`, below); this gate is the half that must not undo the refusal.
  case "$out" in *result=removed*)
    "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port unset --file "$f" ;;
  esac
done
```

The script removes the key only if it holds **our** base URL — the one passed as `--url`, or the
one it recorded at install time — and reports `result=conflict` instead of deleting a value the
user has since pointed somewhere else. If you see a conflict, leave it alone and tell them what
is there.

`conflict reason=another_installs_routing` on the third file means something else, and it wants a
question rather than a fix: this project routes through a file of its own
(`this_project_routes_in=`), so `~/.claude/settings.json` is the **machine-wide** install's, and
every other project on the machine is using it. Leaving it is what "reset this project" means —
this project falls back to it, which is the documented upgrade path in reverse. Only if the user
says they want the machine-wide install gone too, run that one file again with `--user-scope`, and
then do steps 2 and 3 a second time for **its** port (`port show --key "$KEY"`, with `$KEY` from
`project-key --user-scope` as in step 2).

`--url` is worth passing (it also covers a port that changed since install), but it is **not**
what makes this safe, and the earlier version of this line said it was. That put the property
protecting the user's `ANTHROPIC_BASE_URL` in a prompt — i.e. dependent on this file being read
and followed. `settings.py` now refuses unconditionally: invoked with no `--url` at all it
removes only what it has a record of installing, and exits 2 over anything else. Deleting a
stranger's gateway is not a mistake a skill instruction should be the last line of defence
against.

A clean removal deletes every rolling backup it made for that file along the way — `backup=` then
reports why rather than a path, since there is nothing left at the old one to point at. What
survives is `.pre-install.json` in the recovery folder (see step 3), which is the one that matters.

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
PORT="<port>"
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

Once it is confirmed stopped, release the port — and only then:

```bash
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/context-guru"
rm -f "${STATE}/proxy-${PORT}.owner" "${STATE}/proxy-${PORT}.fingerprint"
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port release

# A MACHINE-WIDE install is not filed under any project — it has its own record, so that a project
# install and a machine-wide one made from inside it can hold two ports. `port release` with no
# --key releases THIS PROJECT's record and would leave that one behind. Only when the routing you
# just removed was in `~/.claude/settings.json`:
KEY=$("${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" project-key --user-scope | sed -n 's/^key=//p')
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" port release --key "$KEY"
```

Ask `project-key --user-scope` for that key rather than deriving it — one encoding of where that
record lives, so a release cannot name a row nothing looks up.

Both halves matter, for different reasons. `port release` drops this project's record, and that
record is what keeps the port out of every other project's allocation pool — skip it and the pool
loses a port per uninstall, silently, until installs start landing further and further from 8787 for
no visible reason. The `.owner` file is what makes a *future* install on that port refuse with
`port_owned_by_another_project`: it names the project that owned the proxy you just stopped, so
leaving it behind gets the next install here refused over a proxy that no longer exists. The
`.fingerprint` describes the same dead proxy and goes with it.

**Only do this if the proxy is actually stopped.** If the kill reported **NOT OURS**, or the health
check still answers, leave all three alone — they describe something that is still running, and a
released record plus a live proxy is the one state nothing else in the plugin expects. Neither
command is allowed to be load-bearing either: `port release` fails open by design, so report a
`result=skipped` and finish the uninstall rather than stopping on it.

## 3. Offer, do not assume, the rest

Ask before either of these; neither is implied by "stop routing my sessions":

- **Delete the binary** — `rm ~/.local/bin/context-guru-proxy` (or wherever
  `command -v context-guru-proxy` reports).
- **Remove the plugin itself** — `/plugin uninstall context-guru@context-guru`. Until they do,
  both hooks (`SessionStart` and `UserPromptSubmit`) stay installed, and that is harmless: each
  self-gates on `ANTHROPIC_BASE_URL` and exits immediately in a project that is not routed — which,
  after step 1, is every project. Worth saying, so a leftover hook is not mistaken for a leftover
  proxy.
- **Delete the state directory** — `~/.local/state/context-guru` holds the pidfile,
  `install-scope.json` (which port belongs to which project, shared by every project on the machine
  — so deleting it affects the others, not just this one), the dashboard
  database (session metadata and token counts, no prompt content unless they enabled content
  capture), and `context-guru-reset` itself. Deleting it removes the hatch and its `~/.local/bin`
  copy — fine once they are working again and have confirmed it, but say so before they agree.
- **Delete the recovery folder(s)** — `<dir>/context-guru-settings-json/`, beside each settings
  file the install touched (so `.claude/context-guru-settings-json/` for a project install, or
  `~/.claude/context-guru-settings-json/` for `--global`). Step 1's removal already deleted the
  rolling `.context-guru-backup-*.json` files in there on its own — nothing left to offer there.
  **Say what's still in it before they agree to delete the rest:** a `README.md` explaining the
  folder, and `<name>.pre-install.json` (the copy taken before this plugin's first edit — what
  step 1 restores from if needed), plus `<name>.pre-reset-*.json` if the escape hatch has run here.
  (If context-guru created the file from nothing, step 1 already removed the whole folder along
  with it — there is nothing left to offer here at all.) Deleting the rest is what makes the
  routing removal irreversible; it also can contain a credential, same as the settings file it
  copies, which is why `/context-guru:install` adds a `.gitignore` entry for it automatically. Full
  explanation:
  [docs/how-to/plugin-recovery-files.md](../../../docs/how-to/plugin-recovery-files.md).

## 4. Confirm the end state

Report, in one short list: which files changed and their backups, that the proxy is stopped,
whether the binary and plugin are still present, and that the change lands in the next session.

If there are leftovers the user declined to remove, name them — an uninstall that quietly
leaves things behind is the reason people distrust installers.
