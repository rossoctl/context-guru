#!/usr/bin/env python3
"""Add or remove settings keys for the context-guru plugin: env.ANTHROPIC_BASE_URL (routing) and,
optionally, the top-level statusLine key.

This is the deterministic half of the install. The skill decides WHICH file and what to do
about a conflict; this script does the edit and refuses to guess.

Why a script rather than `jq` in the skill's prompt: the target file is the user's real
`settings.json`, holding their theme, model, permission rules, statusline and possibly their
own base URL. Every operation here is therefore conservative to the point of being boring:

* the file is read, parsed, and written back whole — never patched textually;
* a timestamped backup is written BEFORE the file is touched, and its path is reported;
* an existing ANTHROPIC_BASE_URL that is not ours is a CONFLICT and exits non-zero, because
  overwriting somebody's gateway or benchmark endpoint is not a thing to do quietly;
* anything unparseable is refused rather than replaced with a fresh file, which would throw
  away settings the user cannot get back.

Output is one `key=value` line per fact on stdout, so the skill can act on the result without
re-reading the file or parsing prose.

Usage:
  settings.py add    --file PATH --url URL [--force] [--upstream URL] [--bin PATH] [--statusline CMD]
  settings.py add    --file PATH --statusline CMD [--force]   # statusline only, no routing change
  settings.py remove --file PATH [--url URL]
  settings.py show   --file PATH
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import shutil
import sys
import tempfile

KEY = "ANTHROPIC_BASE_URL"
# The second key, written only when the proxy has to chain behind an existing gateway. It lives in
# the same env block because that block is what the SessionStart hook inherits — see cmd_add.
UPSTREAM_KEY = "ANTHROPIC_UPSTREAM"
# The absolute path to the proxy, for machines where its directory is not on PATH — the hook
# resolves the binary by name, so on those machines this is what makes auto-start work at all.
BIN_KEY = "CONTEXT_GURU_BIN"

# The keys this script owns. `other_env_keys` counts what the USER has in the env block, so it must
# exclude all three of ours — it excluded only the base URL, so a chained install reported 3 "other"
# keys where the user owned 1, and /context-guru:status repeated the wrong number back to them.
OURS = (KEY, UPSTREAM_KEY, BIN_KEY)

# statusLine is a TOP-LEVEL settings key — a sibling of `env`, not a member of it — so it needs its
# own record rather than fitting into OURS/env above. --statusline is optional on `add`: most
# installs never pass it, and every add/remove call that omits it must behave exactly as it did
# before this key existed.
STATUSLINE_KEY = "statusLine"
STATUSLINE_META = "installed_statusline"      # the command string we wrote, so a later run
                                               # recognises its own work even if it is about to
                                               # write a DIFFERENT command (the plugin moved).
STATUSLINE_PREV_META = "previous_statusline"  # what we replaced, so uninstall can hand it back.

# Where this script records what it did, so a later run can tell its own work from the user's.
META = "$context-guru"


def is_ours(data: dict, url: str) -> bool:
    """Did WE write this base URL? Answered from a record, never from the URL's shape.

    The tempting version of this is a regex over loopback `/anthropic` URLs, and it is wrong in a
    way a test caught: litellm's default is `http://127.0.0.1:4000/anthropic`, so "any local
    /anthropic URL is ours" would make uninstall delete somebody else's routing. Two local proxies
    are indistinguishable by URL — so instead `add` records the URL it wrote, and this reads it.

    A file with no record predates that (or was hand-edited), and then only an exact match against
    the URL the caller passed counts. Fail toward leaving the user's configuration alone.
    """
    meta = data.get(META)
    if isinstance(meta, dict) and meta.get("installed_base_url"):
        return url == meta["installed_base_url"]
    return False


def is_ours_statusline(data: dict, current: object) -> bool:
    """Did WE write the CURRENT statusLine value? Same shape as is_ours() above and for the same
    reason: answered from a record of what we wrote, never from guessing at the value's shape —
    a hand-written statusLine that happens to run a script named similarly to ours is still the
    user's, and uninstall must not take it.
    """
    meta = data.get(META)
    if isinstance(meta, dict) and meta.get(STATUSLINE_META):
        return current == {"type": "command", "command": meta[STATUSLINE_META]}
    return False


def apply_statusline(data: dict, command: str) -> None:
    """Write our statusLine into `data`, in place. A no-op if `command` is empty (the flag was not
    passed on this call) or the file already holds exactly what we would write. Records what it
    replaced, but only when that value was not already ours (a re-run that only changes the
    command path must not overwrite the ORIGINAL previous_statusline with our own prior value).
    """
    if not command:
        return
    desired = {"type": "command", "command": command}
    current = data.get(STATUSLINE_KEY)
    if current == desired:
        return
    meta = data.setdefault(META, {})
    if current is not None and not is_ours_statusline(data, current):
        meta[STATUSLINE_PREV_META] = current
    data[STATUSLINE_KEY] = desired
    meta[STATUSLINE_META] = command


def emit(**facts: object) -> None:
    for k, v in facts.items():
        print(f"{k}={v}")


def load(path: str) -> tuple[dict, bool]:
    """Return (settings, existed). Refuses to proceed on anything it cannot parse."""
    if not os.path.exists(path):
        return {}, False
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
    if not text.strip():
        return {}, True
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        emit(result="error", reason="unparseable_json", detail=f"{exc}")
        # Deliberately fatal. The alternative — treating a broken file as empty — would
        # silently discard every setting in it.
        sys.exit(3)
    if not isinstance(data, dict):
        emit(result="error", reason="not_an_object")
        sys.exit(3)
    return data, True


def backup(path: str) -> str:
    """Copy `path` aside and return the copy's name. Never overwrites an existing backup.

    The stamp used to be second-granularity with a plain `copy2`, which meant an
    install-then-uninstall round trip — well inside one second — wrote both backups to the SAME
    filename, and the survivor held the POST-install state. The user was then told to keep that
    path as their undo, and it was a copy of the change, not of what preceded it.

    Microseconds plus O_EXCL: the exclusive create is what actually guarantees it, since two
    writes in the same microsecond are merely unlikely rather than impossible.
    """
    stamp = _dt.datetime.now().strftime("%Y%m%d-%H%M%S-%f")
    for attempt in range(100):
        dest = f"{path}.context-guru-backup-{stamp}" + (f".{attempt}" if attempt else "")
        try:
            fd = os.open(dest, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            continue
        with os.fdopen(fd, "wb") as out, open(path, "rb") as src:
            shutil.copyfileobj(src, out)
        shutil.copystat(path, dest)
        prune_backups(path)
        return dest
    raise RuntimeError(f"could not create a backup for {path}")


# How many backups of one settings file to keep. Each add and each remove writes one, so a user
# who installs and uninstalls a few times accumulated them forever in ~/.claude — 40 files after
# 20 cycles, in a directory they read by hand.
KEEP_BACKUPS = 10


def prune_backups(path: str) -> None:
    """Delete all but the newest KEEP_BACKUPS backups of `path`. Best effort."""
    import glob

    try:
        # glob.escape on the PATH: `[`, `?` and `*` in a directory name are pattern syntax, so
        # for a settings file under e.g. `~/projects/foo[1]/.claude/` this matched nothing and
        # pruning silently did nothing forever — invisible, because pruning is best-effort by
        # design, and the backups it exists to bound then accumulate without limit.
        found = sorted(glob.glob(glob.escape(path) + ".context-guru-backup-*"),
                       key=os.path.getmtime)
    except OSError:
        return
    for old in found[:-KEEP_BACKUPS]:
        try:
            os.remove(old)
        except OSError:
            pass


def save(path: str, data: dict) -> None:
    """Write `data` to `path` atomically, preserving the file's identity and permissions.

    Three things here are each a defect that was found rather than anticipated:

    * **Follow symlinks.** A dotfile-managed `settings.json` is commonly a symlink into a
      repository. `os.replace` onto the link path REPLACES THE LINK with a regular file, so the
      edit silently never reaches the file the user actually manages and their dotfiles still
      hold the old content. Resolve first, then write to the real path.
    * **Preserve the mode.** The temp file is created fresh, so the replaced file's mode was
      taken from the umask: a `600` settings file holding `ANTHROPIC_AUTH_TOKEN` came back
      world-readable `644` under the common default umask.
    * **Atomic.** An interrupted write must not leave half a settings file, which would break
      every session in that scope rather than only ours.
    """
    real = os.path.realpath(path)
    os.makedirs(os.path.dirname(os.path.abspath(real)) or ".", exist_ok=True)
    # 0600 from the instant the file exists, not from copymode() below.
    #
    # `open(tmp, "w")` creates with 0666 & ~umask — typically 0644 — and the mode was corrected
    # only after the entire file had been written and flushed. For a settings.json holding
    # ANTHROPIC_AUTH_TOKEN (the reason the mode is preserved at all, per the docstring above) that
    # is a window in which the replacement sits world-readable on disk. Start private, then widen
    # with copymode; never the other way round.
    #
    # mkstemp rather than O_EXCL on the fixed name `.context-guru-tmp`: O_EXCL would close the
    # mode window too, but a leftover temp file from a crash or a full disk would then make every
    # later save fail until somebody deleted it by hand — trading a mode window for a permanent
    # lockout of the file this script exists to edit. A unique name has neither problem.
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(os.path.abspath(real)) or ".",
                               prefix=os.path.basename(real) + ".context-guru-tmp-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=2, ensure_ascii=False)
            fh.write("\n")
        if os.path.exists(real):
            shutil.copymode(real, tmp)
        else:
            os.chmod(tmp, 0o600)
        os.replace(tmp, real)
    except BaseException:
        # Leave no litter on the failure paths — this directory is the user's ~/.claude.
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def cmd_show(args: argparse.Namespace) -> int:
    data, existed = load(args.file)
    current = (data.get("env") or {}).get(KEY)
    sl = data.get(STATUSLINE_KEY)
    emit(
        result="ok",
        file=args.file,
        exists=str(existed).lower(),
        base_url=current if current else "(unset)",
        other_env_keys=len([k for k in (data.get("env") or {}) if k not in OURS]),
        top_level_keys=len(data),
        statusline=json.dumps(sl, sort_keys=True) if sl else "(unset)",
    )
    return 0


def cmd_add(args: argparse.Namespace) -> int:
    data, existed = load(args.file)
    env = data.get("env")
    if env is None:
        env = {}
    if not isinstance(env, dict):
        emit(result="error", reason="env_not_an_object")
        return 3

    # What a COMPLETE install looks like in this file. Every exit below is judged against this whole
    # set, not against the base URL alone.
    #
    # That distinction is the fix for a defect worth spelling out, because it defeated the remedy for
    # every other failure in this flow. `unchanged` used to mean "the base URL matches", and the two
    # early returns below wrote nothing else — so:
    #
    #   add --url <same> --upstream http://gw:4000 --bin /opt/cg/proxy
    #     -> result=unchanged, exit 0, and NEITHER new key written
    #
    # Re-running the install is the obvious thing to do after an attempt dies partway, which is how
    # every hosted-agent attempt ended — and it was a no-op that reported success. There was no way to
    # add the upstream to an already-routed project at all. The repointed path had the same hole, so
    # changing the configured port silently un-chained the proxy.
    desired = {KEY: args.url}
    if getattr(args, "upstream", ""):
        desired[UPSTREAM_KEY] = args.upstream
    if getattr(args, "bin", ""):
        desired[BIN_KEY] = args.bin

    # --statusline is checked here, ahead of every base_url branch below, for the same reason the
    # `desired` set above exists at all: a conflict on it must stop the WHOLE call, including the
    # base_url side, rather than writing half an install and reporting success. It is a TOP-LEVEL
    # key (see apply_statusline), so it cannot join `desired`/`env` above; this is its own gate.
    sl_command = getattr(args, "statusline", "")
    sl_desired = {"type": "command", "command": sl_command} if sl_command else None
    if sl_command:
        existing_sl = data.get(STATUSLINE_KEY)
        if existing_sl is not None and not is_ours_statusline(data, existing_sl) and not args.force:
            emit(result="conflict", file=args.file, existing=json.dumps(existing_sl, sort_keys=True),
                 proposed=sl_command, conflict_on="statusline",
                 note="a statusLine is already configured in this file; ask before replacing it, "
                      "then re-run with --force")
            return 2
    sl_unchanged = sl_desired is None or data.get(STATUSLINE_KEY) == sl_desired

    # --url is optional when --statusline is present WITHOUT it: this is a statusline-only call
    # (the /context-guru:statusline skill uses it) and must touch nothing about routing — a
    # missing --url must never fall through to the branches below, which all assume a real URL
    # and would otherwise write env.ANTHROPIC_BASE_URL="" or treat an existing one as a conflict
    # with an empty string neither the user nor this call asked to set.
    if not args.url:
        if sl_unchanged:
            emit(result="unchanged", file=args.file,
                 note="statusline already installed, nothing to add")
            return 0
        saved = backup(args.file) if existed else ""
        apply_statusline(data, sl_command)
        save(args.file, data)
        emit(result="added", file=args.file, backup=saved or "(new file)", statusline=sl_command)
        return 0

    current = env.get(KEY)
    if current == args.url and all(env.get(k) == v for k, v in desired.items()) and sl_unchanged:
        emit(result="unchanged", file=args.file, base_url=current,
             upstream=env.get(UPSTREAM_KEY, ""), bin=env.get(BIN_KEY, ""),
             note="already routed to this proxy, with nothing left to add")
        return 0
    if current == args.url:
        # Routed already, but missing one of the other keys — the repair case. Fill in only what is
        # absent or different, and say which, since "added" would misdescribe it.
        saved = backup(args.file) if existed else ""
        changed = [k for k, v in desired.items() if env.get(k) != v]
        if not sl_unchanged:
            changed.append("statusLine")
        env.update(desired)
        data["env"] = env
        meta = data.setdefault(META, {})
        meta["installed_base_url"] = args.url
        if getattr(args, "upstream", ""):
            meta["installed_upstream"] = args.upstream
        if getattr(args, "bin", ""):
            meta["installed_bin"] = args.bin
        apply_statusline(data, sl_command)
        save(args.file, data)
        emit(result="completed", file=args.file, base_url=args.url, added_keys=",".join(changed),
             upstream=env.get(UPSTREAM_KEY, ""), bin=env.get(BIN_KEY, ""), backup=saved,
             note="already routed; filled in the keys that were missing")
        return 0
    if current and is_ours(data, current) and not args.force:
        # Our own URL on a different port — the user changed the configured port and re-ran.
        # Reporting a conflict here told them somebody else owned their routing, which was
        # wrong and alarming. Move it, and keep the note so the change is visible.
        saved = backup(args.file)
        env.update(desired)   # the whole set: a port change must not drop the chaining keys
        data["env"] = env
        meta = data.setdefault(META, {})
        meta["installed_base_url"] = args.url
        if getattr(args, "upstream", ""):
            meta["installed_upstream"] = args.upstream
        if getattr(args, "bin", ""):
            meta["installed_bin"] = args.bin
        apply_statusline(data, sl_command)
        save(args.file, data)
        emit(result="repointed", file=args.file, base_url=args.url, previous=current,
             upstream=env.get(UPSTREAM_KEY, ""), bin=env.get(BIN_KEY, ""),
             backup=saved, note="this was our own URL on another port; moved")
        return 0
    if current and not args.force:
        # The one conflict this has to reason about. `env` blocks merge per key across
        # scopes, so a user-scope install is not clobbered by a project that ships its own
        # env block — what is left is a base URL the USER set, which may be their company
        # gateway or a benchmark endpoint, and taking it over would break their setup while
        # looking like it worked.
        emit(result="conflict", file=args.file, existing=current, proposed=args.url,
             note="ANTHROPIC_BASE_URL is already set here; ask before replacing it, "
                  "then re-run with --force")
        return 2

    saved = backup(args.file) if existed else ""
    env[KEY] = args.url
    # --upstream writes the SECOND key that chaining needs, in the same atomic save.
    #
    # Not scope creep: without it, chaining survives only as long as the proxy that is already
    # running. A later session's SessionStart hook reads its configuration from this env block, so a
    # missing ANTHROPIC_UPSTREAM means that hook starts a proxy aimed at api.anthropic.com — and on a
    # platform whose gateway holds the credential and rewrites model names, every request in that
    # session fails. The first real install on a hosted agent ended exactly here, with the skill
    # telling the user to paste the key in by hand.
    #
    # Recorded in our own metadata as well, so uninstall removes only an upstream WE wrote.
    if getattr(args, "upstream", ""):
        env[UPSTREAM_KEY] = args.upstream
    # --bin persists an ABSOLUTE path to the proxy, for machines where $DEST is not on PATH.
    #
    # `~/.local/bin` frequently is not, and on a hosted agent it is worse than an inconvenience: the
    # writable directories there reset on pod restart, so "add it to your shell profile" is advice
    # that does not survive. The SessionStart hook resolves the binary BY NAME, so without this the
    # install succeeds, routing works for the session that set it up, and the auto-restart safety net
    # silently never fires afterwards — and the failure mode it exists to catch is a hang with no
    # error. start-proxy.sh already honours CONTEXT_GURU_BIN and accepts an absolute path, so the fix
    # is to write it where the hook will inherit it rather than to ask for a PATH change.
    if getattr(args, "bin", ""):
        env[BIN_KEY] = args.bin
    data["env"] = env
    # Remember what we took over, so uninstall can hand it back.
    #
    # `replaced` used to be reported and then forgotten. After a --force install over somebody's
    # own gateway, uninstall deleted the key and left them with NO base URL at all — and because
    # the backup filename collided with the install's own, the copy that held it was gone too.
    # Their setup was unrecoverable from anything the tool produced.
    # Record what we wrote, so a re-run can recognise its own work, and what we took over, so
    # uninstall can hand it back.
    meta = data.setdefault(META, {})
    meta["installed_base_url"] = args.url
    if getattr(args, "upstream", ""):
        meta["installed_upstream"] = args.upstream
    if getattr(args, "bin", ""):
        meta["installed_bin"] = args.bin
    if current:
        meta["previous_base_url"] = current
    apply_statusline(data, sl_command)
    save(args.file, data)
    emit(result="added", file=args.file, base_url=args.url,
         replaced=current if current else "", backup=saved or "(new file)",
         other_env_keys=len([k for k in env if k not in OURS]))
    return 0


def cmd_remove(args: argparse.Namespace) -> int:
    data, existed = load(args.file)
    if not existed:
        emit(result="unchanged", file=args.file, note="no such file")
        return 0
    env = data.get("env")
    if not isinstance(env, dict) or KEY not in env:
        # No routing to remove here — but a STATUSLINE-ONLY install (the /context-guru:statusline
        # skill's `add --statusline` with no --url) never touches env at all, so it must not be
        # missed just because there is no base_url in this file to key off.
        meta0 = data.get(META)
        recorded_sl0 = meta0.get(STATUSLINE_META) if isinstance(meta0, dict) else None
        if recorded_sl0 and data.get(STATUSLINE_KEY) == {"type": "command", "command": recorded_sl0}:
            saved = backup(args.file)
            restored_sl = ""
            del data[STATUSLINE_KEY]
            if meta0.get(STATUSLINE_PREV_META):
                data[STATUSLINE_KEY] = meta0[STATUSLINE_PREV_META]
                restored_sl = json.dumps(meta0[STATUSLINE_PREV_META], sort_keys=True)
            meta0.pop(STATUSLINE_META, None)
            meta0.pop(STATUSLINE_PREV_META, None)
            if not meta0:
                data.pop(META, None)
            save(args.file, data)
            emit(result="removed", file=args.file, backup=saved, statusline_restored=restored_sl,
                 note="statusline-only removal; no routing was present to touch")
            return 0
        emit(result="unchanged", file=args.file, note=f"no env.{KEY} here")
        return 0
    current = env[KEY]
    # Ours is the URL passed in, or the one we recorded at install time — which covers the case
    # where the configured port changed since. It is NOT "any loopback /anthropic URL": litellm's
    # default is one of those, and uninstall must not delete somebody else's routing.
    #
    # This check is UNCONDITIONAL, and that is the fix for the worst defect this script has had.
    # It used to read `if args.url and ...`, so omitting --url — an invocation this module's own
    # docstring advertises as supported — skipped the check entirely and deleted whatever was
    # there. Measured against a corporate gateway with no context-guru record: `result=removed`,
    # `restored=` empty, exit 0. The user's gateway was gone and the exit code said success.
    #
    # What made that severe was not the branch, it was WHERE the safety lived: uninstall/SKILL.md
    # passes --url and its prose said that flag "is what keeps this safe". So the property
    # protecting the user's configuration depended on a model remembering a flag in a prompt.
    # This script is the deterministic half precisely so that it does not have to.
    #
    # With no --url, `current != args.url` is trivially true and is_ours() decides alone — i.e.
    # remove only what we recorded installing, which is what is_ours() is documented to be for.
    # The escape hatch for a record we never wrote (hand-edited settings, or an install predating
    # the record) is to pass --url naming the URL to delete, which the skill already does.
    if current != args.url and not is_ours(data, current):
        # Refuse to remove a base URL that is not ours: the user may have pointed this at
        # something else since, and uninstall must not take that with it.
        emit(result="conflict", file=args.file, existing=current, expected=args.url,
             note="this base URL is not the one context-guru installed; left untouched")
        return 2
    saved = backup(args.file)
    del env[KEY]
    # Take our upstream key with it, but ONLY the value we recorded writing. An ANTHROPIC_UPSTREAM
    # the user set themselves is theirs, and uninstall removing it would be the same class of
    # overreach as deleting a base URL we never installed.
    recorded_upstream = ""
    _meta = data.get(META)
    if isinstance(_meta, dict):
        recorded_upstream = _meta.get("installed_upstream") or ""
    if recorded_upstream and env.get(UPSTREAM_KEY) == recorded_upstream:
        del env[UPSTREAM_KEY]
    recorded_bin = ""
    if isinstance(_meta, dict):
        recorded_bin = _meta.get("installed_bin") or ""
    if recorded_bin and env.get(BIN_KEY) == recorded_bin:
        del env[BIN_KEY]
    # statusLine is a TOP-LEVEL key (not part of `env`), tracked the same way upstream/bin are
    # above: taken back only if it is exactly what we recorded writing, restored to whatever it
    # replaced. NOTE: like upstream/bin, this only runs when env.KEY was present above — the
    # normal case, since `add` always writes them together in one call — so a statusLine left
    # behind by a base_url that was already removed some other way will not be found here.
    recorded_sl = ""
    if isinstance(_meta, dict):
        recorded_sl = _meta.get(STATUSLINE_META) or ""
    restored_sl = ""
    if recorded_sl and data.get(STATUSLINE_KEY) == {"type": "command", "command": recorded_sl}:
        del data[STATUSLINE_KEY]
        if isinstance(_meta, dict) and _meta.get(STATUSLINE_PREV_META):
            data[STATUSLINE_KEY] = _meta[STATUSLINE_PREV_META]
            restored_sl = json.dumps(_meta[STATUSLINE_PREV_META], sort_keys=True)
    # Put back whatever we took over at install time. Deleting the key was leaving a user who had
    # a gateway configured with nothing at all — a worse state than before they installed.
    restored = ""
    meta = data.get(META)
    if isinstance(meta, dict):
        if meta.get("previous_base_url"):
            restored = meta["previous_base_url"]
            env[KEY] = restored
        # Our bookkeeping goes with our key: leaving it behind would make a later install think it
        # had written a URL it did not.
        meta.pop("previous_base_url", None)
        meta.pop("installed_base_url", None)
        meta.pop("installed_upstream", None)
        meta.pop("installed_bin", None)
        meta.pop(STATUSLINE_META, None)
        meta.pop(STATUSLINE_PREV_META, None)
        if not meta:
            data.pop(META, None)
    # Leave no litter: an `env: {}` we created is removed with the key. An env block that
    # still holds the user's own variables stays exactly as it is.
    if not env:
        del data["env"]
    else:
        data["env"] = env
    save(args.file, data)
    emit(result="removed", file=args.file, was=current, backup=saved,
         restored=restored, env_block_left=str(bool(env)).lower(),
         statusline_restored=restored_sl)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("add", "remove", "show"):
        p = sub.add_parser(name)
        p.add_argument("--file", required=True)
        p.add_argument("--url", default="")
        p.add_argument("--force", action="store_true")
        p.add_argument("--bin", default="",
                       help="also write env.CONTEXT_GURU_BIN (absolute path to the proxy), for "
                            "machines where its directory is not on PATH")
        p.add_argument("--upstream", default="",
                       help="also write env.ANTHROPIC_UPSTREAM, so the proxy chains behind an "
                            "existing gateway in LATER sessions too (the hook reads this block)")
        p.add_argument("--statusline", default="",
                       help="on add: also write the TOP-LEVEL statusLine key ({\"type\": "
                            "\"command\", \"command\": <this value>}), refusing to replace one "
                            "that is not ours unless --force. on remove: taken back only if it "
                            "is exactly what a previous --statusline install recorded writing.")
    args = ap.parse_args()
    if args.cmd == "add" and not args.url and not args.statusline:
        ap.error("add needs --url, or --statusline on its own for a statusline-only call")
    return {"add": cmd_add, "remove": cmd_remove, "show": cmd_show}[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())
