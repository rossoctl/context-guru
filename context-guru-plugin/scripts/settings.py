#!/usr/bin/env python3
"""Add or remove exactly ONE key in a Claude Code settings file: env.ANTHROPIC_BASE_URL.

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
  settings.py add    --file PATH --url URL [--force]
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

KEY = "ANTHROPIC_BASE_URL"

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
        found = sorted(glob.glob(f"{path}.context-guru-backup-*"), key=os.path.getmtime)
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
    tmp = f"{real}.context-guru-tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=2, ensure_ascii=False)
        fh.write("\n")
    if os.path.exists(real):
        shutil.copymode(real, tmp)
    else:
        os.chmod(tmp, 0o600)
    os.replace(tmp, real)


def cmd_show(args: argparse.Namespace) -> int:
    data, existed = load(args.file)
    current = (data.get("env") or {}).get(KEY)
    emit(
        result="ok",
        file=args.file,
        exists=str(existed).lower(),
        base_url=current if current else "(unset)",
        other_env_keys=len([k for k in (data.get("env") or {}) if k != KEY]),
        top_level_keys=len(data),
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

    current = env.get(KEY)
    if current == args.url:
        emit(result="unchanged", file=args.file, base_url=current,
             note="already routed to this proxy")
        return 0
    if current and is_ours(data, current) and not args.force:
        # Our own URL on a different port — the user changed the configured port and re-ran.
        # Reporting a conflict here told them somebody else owned their routing, which was
        # wrong and alarming. Move it, and keep the note so the change is visible.
        saved = backup(args.file)
        env[KEY] = args.url
        data["env"] = env
        data.setdefault(META, {})["installed_base_url"] = args.url
        save(args.file, data)
        emit(result="repointed", file=args.file, base_url=args.url, previous=current,
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
    if current:
        meta["previous_base_url"] = current
    save(args.file, data)
    emit(result="added", file=args.file, base_url=args.url,
         replaced=current if current else "", backup=saved or "(new file)",
         other_env_keys=len([k for k in env if k != KEY]))
    return 0


def cmd_remove(args: argparse.Namespace) -> int:
    data, existed = load(args.file)
    if not existed:
        emit(result="unchanged", file=args.file, note="no such file")
        return 0
    env = data.get("env")
    if not isinstance(env, dict) or KEY not in env:
        emit(result="unchanged", file=args.file, note=f"no env.{KEY} here")
        return 0
    current = env[KEY]
    # Ours is the URL passed in, or the one we recorded at install time — which covers the case
    # where the configured port changed since. It is NOT "any loopback /anthropic URL": litellm's
    # default is one of those, and uninstall must not delete somebody else's routing.
    if args.url and current != args.url and not is_ours(data, current):
        # Refuse to remove a base URL that is not ours: the user may have pointed this at
        # something else since, and uninstall must not take that with it.
        emit(result="conflict", file=args.file, existing=current, expected=args.url,
             note="this base URL is not the one context-guru installed; left untouched")
        return 2
    saved = backup(args.file)
    del env[KEY]
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
         restored=restored, env_block_left=str(bool(env)).lower())
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("add", "remove", "show"):
        p = sub.add_parser(name)
        p.add_argument("--file", required=True)
        p.add_argument("--url", default="")
        p.add_argument("--force", action="store_true")
    args = ap.parse_args()
    if args.cmd == "add" and not args.url:
        ap.error("add needs --url")
    return {"add": cmd_add, "remove": cmd_remove, "show": cmd_show}[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())
