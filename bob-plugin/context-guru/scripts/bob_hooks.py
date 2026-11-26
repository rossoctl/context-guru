#!/usr/bin/env python3
"""Merge context-guru's hooks into Bob's global settings file, and take out exactly them.

A Bob plugin cannot carry hooks. Bob reads them only from the `hooks` key of
~/.bob/settings/settings.json (or a workspace .bob/settings.json). So we edit that file:

- We add one matcher group per event. Each handler command ends with TAG, so we can find our
  groups again even after Bob rewrites the file.
- We keep every other key, every other hook and the key order.
- We refuse a file that is not plain JSON (comments, trailing commas). We do not rewrite what we
  cannot read back exactly.
- One symlink hop is followed (a dotfiles repo). The link stays a link.
- Our state folder keeps the bytes before and after our write. If the file still has our bytes
  at removal, and nothing else wrote it since the first setup, we put back the original bytes
  exactly. If Bob changed it since, we remove only
  the groups that carry TAG.
"""

import json
import os
from pathlib import Path
import re
import shlex
import tempfile

import bob_route

TAG = "# context-guru-bob-hook"
HOOK_TIMEOUT_S = 5
NEW_FILE_MODE = 0o600


class HooksRefused(Exception):
    """The settings file is one we must not rewrite. The message says why."""


def _command(python, script, subcommand, takes_stdin):
    # Bob runs a command hook with Node's child_process.exec, so through `/bin/sh -c`. The
    # `|| true` makes the hook exit 0 even when python3 or the script is gone (127 or 2). Exit 2
    # would block the user's prompt. `</dev/null` keeps the prompt away from a handler that does
    # not need stdin. Only check may print: its one line is meant for the model.
    stdin = "" if takes_stdin else " </dev/null"
    output = " 2>/dev/null" if subcommand == "check" else " >/dev/null 2>&1"
    return (f"{shlex.quote(python)} {shlex.quote(script)} {subcommand}{stdin}{output}"
            f" || true {TAG}")


def our_groups(python, script):
    """Our matcher group for each event. Absolute paths: Bob has no plugin-root variable."""

    def handler(subcommand, takes_stdin=False):
        return {"type": "command", "command": _command(python, script, subcommand, takes_stdin),
                "timeout": HOOK_TIMEOUT_S}

    return {
        # No matcher: startup, resume and compact all need the proxy, and compact also needs
        # the signal (the proxy retires the old cache entries).
        "SessionStart": {"hooks": [handler("ensure"), handler("signal", takes_stdin=True)]},
        # Bob's prompt is on stdin here. check does not need it, so it never reads it.
        "UserPromptSubmit": {"hooks": [handler("check")]},
    }


def _is_ours(group):
    hooks = group.get("hooks") if isinstance(group, dict) else None
    return isinstance(hooks, list) and bool(hooks) and all(
        isinstance(h, dict) and isinstance(h.get("command"), str) and h["command"].endswith(TAG)
        for h in hooks)


def _reject_duplicates(pairs):
    seen = {}
    for key, value in pairs:
        if key in seen:
            raise ValueError(f"duplicate key {key!r}")
        seen[key] = value
    return seen


def _load(path):
    """Return (bytes or None, dict). Raise HooksRefused for anything but a plain JSON object."""
    try:
        raw = Path(path).read_bytes()
    except FileNotFoundError:
        return None, {}
    if not raw.strip():
        return raw, {}
    try:
        data = json.loads(raw.decode("utf-8"), object_pairs_hook=_reject_duplicates)
    except (UnicodeDecodeError, ValueError) as error:
        raise HooksRefused(f"{path} is not plain JSON ({error}). It may have comments or "
                           "trailing commas. I do not rewrite it. Add the hooks by hand, or "
                           "remove the comments and run setup again.") from None
    if not isinstance(data, dict):
        raise HooksRefused(f"{path} is not a JSON object. I do not rewrite it.")
    if "hooks" in data and not isinstance(data["hooks"], dict):
        raise HooksRefused(f"{path} has a `hooks` value that is not an object. I do not "
                           "rewrite it.")
    for event, groups in (data.get("hooks") or {}).items():
        if not isinstance(groups, list):
            raise HooksRefused(f"{path} has hooks.{event} that is not a list. I do not "
                               "rewrite it.")
    return raw, data


def _indent(raw):
    """The indent the file already uses, so our write looks like the user's file."""
    match = re.search(rb"\n([ \t]+)\S", raw or b"")
    if not match:
        return 2
    unit = match.group(1).decode()
    return "\t" if unit.startswith("\t") else len(unit)


def _render(data, raw):
    text = json.dumps(data, indent=_indent(raw), ensure_ascii=False)
    return (text + "\n").encode("utf-8")


def _atomic_write(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        mode = os.stat(path).st_mode & 0o7777
    except FileNotFoundError:
        mode = NEW_FILE_MODE
    fd, temporary = tempfile.mkstemp(prefix=".context-guru-bob.", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary:
            os.unlink(temporary)


def _strip_ours(data):
    hooks = data.get("hooks")
    if not isinstance(hooks, dict):
        return data
    for event in list(hooks):
        kept = [g for g in hooks[event] if not _is_ours(g)]
        if kept:
            hooks[event] = kept
        else:
            del hooks[event]
    return data


def target(path):
    """The file to read and write: one symlink hop is followed (bob_route's rule)."""
    end, why = bob_route.follow(path)
    if end is None:
        raise HooksRefused(why)
    return end


def check(path):
    """Raise HooksRefused when install would refuse. Writes nothing."""
    _load(target(path))


def _read_record(state_dir):
    try:
        record = json.loads((state_dir / "record.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return record if isinstance(record, dict) else None


def install(path, state_dir, groups):
    """Merge our groups into the settings file. Record what we did in state_dir.

    state_dir holds record.json, and, only while an exact restore is still safe, before.json
    (the user's bytes) and after.json (the bytes we wrote). Plain files, so reset.sh can compare
    them without Python.

    An exact restore is safe only when the file is still the bytes we wrote, and nothing else
    wrote it between the first setup and now. A second setup over a file the user changed
    since makes the restore unsafe for good, so we drop before.json and after.json. Uninstall
    then removes only our tagged groups.
    """
    path, state_dir = Path(path), Path(state_dir)
    real = target(path)
    raw, data = _load(real)
    before, after = state_dir / "before.json", state_dir / "after.json"
    previous = _read_record(state_dir)
    if previous and previous.get("settings") == str(path):
        record = previous
        # Still exact only when the file is the bytes our last run wrote.
        exact = (record.get("exact") is True and after.exists() and raw is not None
                 and raw == after.read_bytes()
                 and (before.exists() or not record.get("existed")))
    else:
        record = {"settings": str(path), "existed": raw is not None,
                  "had_hooks_key": "hooks" in data}
        # Our groups already in the file (the state folder was lost): those bytes are not the
        # user's, so an exact restore would put our hooks back.
        exact = not any(_is_ours(g) for groups in (data.get("hooks") or {}).values()
                        for g in groups)
    _strip_ours(data)
    hooks = data.setdefault("hooks", {})
    for event, group in groups.items():
        hooks.setdefault(event, []).append(group)
    rendered = _render(data, raw)
    state_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(state_dir, 0o700)
    record.update(events=sorted(groups), exact=exact)
    if not exact:
        before.unlink(missing_ok=True)
        after.unlink(missing_ok=True)
    elif not previous and raw is not None:
        _atomic_write(before, raw)
    _atomic_write(state_dir / "record.json",
                  (json.dumps(record, indent=2, ensure_ascii=False) + "\n").encode("utf-8"))
    if rendered != raw:
        _atomic_write(real, rendered)
    if exact:
        _atomic_write(after, rendered)


def _clear(state_dir):
    for name in ("record.json", "before.json", "after.json"):
        (state_dir / name).unlink(missing_ok=True)
    try:
        state_dir.rmdir()
    except OSError:
        pass


def remove(path, state_dir):
    """Take out our groups. Return restored, removed, absent or no_record."""
    path, state_dir = Path(path), Path(state_dir)
    record = _read_record(state_dir)
    real = target(path)
    try:
        raw = real.read_bytes()
    except FileNotFoundError:
        raw = None
    before, after = state_dir / "before.json", state_dir / "after.json"
    exact = (record is not None and record.get("exact", True) is True and raw is not None
             and after.exists() and raw == after.read_bytes()
             and (before.exists() or not record.get("existed")))
    if exact:
        # Nobody changed the file since our write: give back the user's exact bytes.
        if record.get("existed"):
            _atomic_write(real, before.read_bytes())
        else:
            real.unlink()
        _clear(state_dir)
        return "restored"
    if raw is None:
        _clear(state_dir)
        return "absent" if record else "no_record"
    raw, data = _load(real)
    if not any(_is_ours(g) for groups in (data.get("hooks") or {}).values() for g in groups):
        _clear(state_dir)
        return "absent"
    _strip_ours(data)
    if record and not record.get("had_hooks_key") and data.get("hooks") == {}:
        del data["hooks"]
    if record and not record.get("existed") and data == {}:
        real.unlink()  # we made the file, and nothing else is in it
        _clear(state_dir)
        return "removed"
    _atomic_write(real, _render(data, raw))
    _clear(state_dir)
    return "removed"


def is_installed(path):
    try:
        _, data = _load(target(path))
    except HooksRefused:
        return False
    return any(_is_ours(g) for groups in (data.get("hooks") or {}).values() for g in groups)
