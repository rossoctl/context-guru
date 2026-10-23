#!/usr/bin/env python3
"""Settings that cost you money: find client settings that make you pay for something you could get free or
cheaper, say what they cost, and change them after a yes. Part of /context-guru:save-more (docs/plugin/save-more.md).

    The list of checks is costly_settings.json, next to this file. ONE list: the plugin reads it here, and
    dash/costlysettings.go prices the same shapes (a Go test compares them).

Detection reads the user's own client settings on their machine and ONLY the keys a check names. It never
prints any other value and never reads a secret. Cost comes from the proxy, metadata only (GET
/api/adapt/settings). Stdlib only. Nothing here writes except `apply` and `revert`.
"""

from __future__ import annotations

import datetime as dt
import json
import os
import re
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
LIST_PATH = os.path.join(HERE, "costly_settings.json")
RECORD_NAME = "costly-settings.json"
SOURCE_ORDER = ("user", "user-local", "project", "project-local", "managed")  # low to high precedence
MANAGED_DEFAULT = {"darwin": "/Library/Application Support/ClaudeCode/managed-settings.json",
                   "win32": r"C:\Program Files\ClaudeCode\managed-settings.json"}


def load_checks(path: str = LIST_PATH) -> list[dict]:
    with open(path, encoding="utf-8") as f:
        return json.load(f)["checks"]


# ---------------------------------------------------------------------------------------------
# where the settings are

def user_dir() -> str:
    return os.environ.get("CLAUDE_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".claude")


def project_dir() -> str:
    return os.environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()


def managed_path() -> str:
    return os.environ.get("CG_MANAGED_SETTINGS") or MANAGED_DEFAULT.get(sys.platform, "/etc/claude-code/managed-settings.json")


def settings_files() -> list[tuple[str, str]]:
    """(source, path) from the lowest precedence to the highest."""
    u, p = user_dir(), os.path.join(project_dir(), ".claude")
    return [("user", os.path.join(u, "settings.json")), ("user-local", os.path.join(u, "settings.local.json")),
            ("project", os.path.join(p, "settings.json")), ("project-local", os.path.join(p, "settings.local.json")),
            ("managed", managed_path())]


def tilde(path: str) -> str:
    home = os.path.expanduser("~")
    return "~" + path[len(home):] if home != "~" and path.startswith(home + os.sep) else path


def _load(path: str) -> dict | None:
    try:
        with open(path, encoding="utf-8") as f:
            doc = json.load(f)
    except (OSError, ValueError):
        return None
    return doc if isinstance(doc, dict) else None


def read_named(env_names: list[str], keys: list[str]) -> dict:
    """What the named keys say, per source: {"files": [{source, path, env: {name: str}, keys: {key: value}}], "env": {name: str}}.
    Only the named keys are copied out of a file; nothing else is kept."""
    files = []
    for source, path in settings_files():
        doc = _load(path)
        if doc is None:
            continue
        blk = doc.get("env") if isinstance(doc.get("env"), dict) else {}
        files.append({"source": source, "path": path,
                      "env": {n: str(blk[n]) for n in env_names if n in blk},
                      "keys": {k: doc[k] for k in keys if k in doc}})
    return {"files": files, "env": {n: os.environ[n] for n in env_names if n in os.environ}}


# ---------------------------------------------------------------------------------------------
# detection

def _where(f: dict, what: str) -> dict:
    return {"source": f["source"], "path": f["path"], "what": what, "editable": f["source"] != "managed"}


def detect(check: dict, seen: dict) -> dict | None:
    """None when the check does not fire; else {"locations": [...], "note": str|None}.
    A location: {source, path (None for the process env), what, editable}."""
    d = check["detect"]
    if d["kind"] == "env_truthy":
        name, truthy = d["name"], d["truthy"]
        locs = [_where(f, f"env.{name}") for f in seen["files"] if f["env"].get(name, "").strip().lower() in truthy]
        if not locs and seen["env"].get(name, "").strip().lower() in truthy:
            # The settings env block is applied to Claude Code's own environment, so a value that
            # came from a file would show here too. It is named only when no file has it.
            locs = [{"source": "process", "path": None, "what": f"environment variable {name}", "editable": False}]
        return {"locations": locs} if locs else None
    if d["kind"] == "on_by_default":
        if seen["env"].get(d["off_env"], "").strip().lower() in d["off_values"]:
            return None
        eff = None  # the highest-precedence file that sets the key decides
        for f in seen["files"]:
            if d["key"] in f["keys"]:
                eff = f
        if eff is not None:
            if eff["keys"][d["key"]] is False:
                return None
            return {"locations": [_where(eff, d["key"])]}
        user = next(((s, p) for s, p in settings_files() if s == "user"))
        return {"locations": [{"source": "default", "path": user[1], "what": f"{d['key']} is not set (on by default)", "editable": True}]}
    if d["kind"] == "model_unsupported":
        below = tuple(d["below"])
        locs, shown = [], []
        for name in d["names"]:
            val = effective_value(seen, name)
            v = haiku_version(val)
            if v is None or v >= below:
                continue
            shown.append(val.strip())
            files = [f for f in seen["files"] if name in f["env"]]
            if not files:
                locs.append({"source": "process", "path": None, "what": f"environment variable {name}", "editable": False, "name": name, "value": val})
                continue
            for f in files:  # the winning file decides; every editable file that holds an old Haiku gets the fix too
                fv = haiku_version(f["env"][name])
                if f is files[-1] or (fv is not None and fv < below):
                    locs.append({**_where(f, f"env.{name}"), "name": name, "value": f["env"][name]})
        return {"locations": locs, "values": list(dict.fromkeys(shown))} if locs else None
    return None


_HAIKU_NEW = re.compile(r"haiku[-.](\d{1,2})(?!\d)(?:[-.](\d{1,2})(?!\d))?")  # claude-haiku-4-5, ...-4-5-20251001, ...haiku-5
_HAIKU_OLD = re.compile(r"(?<!\d)(\d{1,2})(?:[-.](\d{1,2})(?!\d))?-haiku")  # claude-3-5-haiku-20241022


def haiku_version(model) -> tuple[int, int] | None:
    """(major, minor) of a Haiku model id, or None when the id is not a Haiku or the version cannot be read.
    Never guesses: 'haiku' alone, a dated id with no version, or any other model gives None."""
    if not isinstance(model, str):
        return None
    m = model.strip().lower()
    if "haiku" not in m:
        return None
    hit = _HAIKU_NEW.search(m) or _HAIKU_OLD.search(m)
    return (int(hit.group(1)), int(hit.group(2) or 0)) if hit else None


def effective_value(seen: dict, name: str) -> str | None:
    """The value Claude Code uses for an env name: the highest-precedence file that sets it, else the process env."""
    eff = None
    for f in seen["files"]:
        if name in f["env"]:
            eff = f["env"][name]
    return eff if eff is not None else seen["env"].get(name)


def findings(checks: list[dict] | None = None, seen_for=None) -> list[dict]:
    out = []
    for c in checks if checks is not None else load_checks():
        seen = (seen_for or read_named)(c["reads"]["env"], c["reads"]["keys"])
        hit = detect(c, seen)
        if hit:
            out.append({"check": c, **hit})
    return out


def applicable(f: dict) -> bool:
    """True when at least one place the setting comes from is a file Context Guru may change."""
    return bool(f["check"].get("applicable")) and any(l["editable"] and l["path"] for l in f["locations"])


# ---------------------------------------------------------------------------------------------
# cost

def costs(api, since: int, until: int) -> dict[str, dict] | None:
    """{check id: row} from the proxy, or None when it cannot say (no proxy, an old one, a failure)."""
    if api is None or (not getattr(api, "base", "") and getattr(api, "fixture", None) is None):
        return None
    try:
        st, doc = api.send("GET", "/api/adapt/settings", {"since": since, "until": until}, timeout=60)
    except (OSError, ValueError):
        return None
    if st != 200 or not isinstance(doc, dict):
        return None
    return {r["id"]: r for r in doc.get("checks") or [] if isinstance(r, dict) and "id" in r}


def cost_words(row: dict | None, days: int, dollars, also: bool = True) -> list[str]:
    """Hindsight words. A row with no cost is 'not measured', never $0."""
    if not row or not row.get("measured") or row.get("cost_usd") is None:
        return ["not measured yet"]
    n = row.get("requests") or 0
    L = [f"about {dollars(row['cost_usd'])} over the last {days} days ({n:,} request{'s' * (n != 1)})"]
    if also:
        for p in row.get("also") or []:
            if (p.get("requests") or 0) > 0 and p.get("cost_usd") is not None:
                L.append(f"also, {p['id'].replace('-', ' ')} (probably): {dollars(p['cost_usd'])}, {p['requests']:,} requests, not added to the figure above")
    return L


# ---------------------------------------------------------------------------------------------
# the card

SOURCE_WORDS = {"user": "your user settings", "user-local": "your local user settings", "project": "this project's settings",
                "project-local": "this project's local settings", "managed": "managed settings (your administrator)",
                "process": "your environment", "default": "not set anywhere (on by default)"}


def where_words(f: dict) -> str:
    parts = []
    for l in f["locations"]:
        s = SOURCE_WORDS[l["source"]]
        parts.append(f"{s}, {tilde(l['path'])}" if l["path"] and l["source"] != "default" else s)
    return "; ".join(dict.fromkeys(parts))


def lines_for(f: dict, rows: dict | None, days: int, dollars, firing: set | None = None) -> list[str]:
    c = f["check"]
    row = (rows or {}).get(c["id"])
    title = c["title"].replace("{model}", ", ".join(f.get("values") or []))
    L = [f"{title} {c['what']}.", f"  Set in: {where_words(f)}"]
    price = c.get("price") or {}
    if price.get("kind") == "avoided_call":  # a side call the fix stops: nothing to price or attribute
        L.append(f"  Saves: {price['saves']}.")
    else:
        words = cost_words(row, days, dollars)
        L += [f"  Cost: {w}" for w in words[:1]] + [f"        {w}" for w in words[1:]]
        if c.get("cost_note") and row and row.get("measured") and row.get("cost_usd") is not None:
            L.append(f"        {c['cost_note']}")
    nw = c.get("note_when")
    if nw:
        L.append("  Note: " + (nw["fires"] if nw["check"] in (firing or set()) else nw["silent"]))
    blocked = [l for l in f["locations"] if not l["editable"]]
    if applicable(f):
        L.append(f"  Fix: {c['fix']['text']} {c['fix']['effect']}")
    else:
        L.append("  Fix: " + ("ask your administrator to change it in managed settings. Context Guru cannot change it."
                              if any(l["source"] == "managed" for l in blocked)
                              else "unset it where you set it (your shell or launcher). Context Guru cannot change it."))
    L.append(f"  Risk: {c['risk']}")
    if c.get("also_changes"):
        L.append(f"  Also changes: {c['also_changes']}")
    if applicable(f) and blocked:
        L.append("  Cannot change: " + "; ".join(SOURCE_WORDS[l["source"]] for l in blocked) + ". It stays on there.")
    return L


def apply_line(slash: str, check_id: str) -> str:
    return f"Apply it? Say yes, or run: {slash} apply settings {check_id}"


def render(fs: list[dict], rows: dict | None, days: int, dollars, card, slash: str, e) -> list[str]:
    """The card, then one never-wrapped apply line per fixable check. Nothing when no check fires."""
    if not fs:
        return []
    body: list[str] = []
    firing = {f["check"]["id"] for f in fs}
    for i, f in enumerate(fs):
        body += ([""] if i else []) + lines_for(f, rows, days, dollars, firing)
    L = card("Settings that cost you money", body, e)
    for f in fs:
        if applicable(f):
            L.append(apply_line(slash, f["check"]["id"]))
    return L


# ---------------------------------------------------------------------------------------------
# changes: apply and revert

def state_dir() -> str:
    o = os.environ.get("CONTEXT_GURU_STATE")
    if o:
        return o
    return os.path.join(os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local", "state"), "context-guru")


def record_path() -> str:
    return os.path.join(state_dir(), RECORD_NAME)


def load_record() -> dict:
    try:
        with open(record_path(), encoding="utf-8") as f:
            doc = json.load(f)
        return doc if isinstance(doc, dict) else {}
    except (OSError, ValueError):
        return {}


def _write_atomic(path: str, data: bytes, mode: int) -> None:
    """Write beside the target and rename over it. The mode is set before the rename, so a 0600 file is never wider."""
    real = os.path.realpath(path)  # a dotfile-managed settings.json is often a symlink: change the file, not the link
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(real), prefix=".cg-", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, real)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _indent(raw: str):
    for line in raw.splitlines()[1:]:
        s = line[: len(line) - len(line.lstrip())]
        if s:
            return "\t" if s[0] == "\t" else len(s)
    return 2


def _read_doc(path: str) -> tuple[dict, str, int]:
    real = os.path.realpath(path)
    with open(real, encoding="utf-8") as f:
        raw = f.read()
    doc = json.loads(raw)
    if not isinstance(doc, dict):
        raise ValueError("not a JSON object")
    return doc, raw, os.stat(real).st_mode & 0o7777


def _dump(doc: dict, raw: str) -> bytes:
    return (json.dumps(doc, indent=_indent(raw), ensure_ascii=False) + ("\n" if raw.endswith("\n") or not raw else "")).encode("utf-8")


ABSENT = {"absent": True}


def _get(doc: dict, fix: dict, name: str | None = None):
    """The current value of the fix's key (for set_env and remove_env, the env key `name`)."""
    if fix["kind"] in ("remove_env", "set_env"):
        name = name or fix["name"]
        blk = doc.get("env")
        return blk[name] if isinstance(blk, dict) and name in blk else ABSENT
    return doc[fix["key"]] if fix["key"] in doc else ABSENT


def _put(doc: dict, fix: dict, value, name: str | None = None) -> None:
    """value ABSENT removes the key; anything else sets it. Touches that one key and nothing else."""
    if fix["kind"] in ("remove_env", "set_env"):
        name = name or fix["name"]
        if value == ABSENT:
            if isinstance(doc.get("env"), dict):
                doc["env"].pop(name, None)
        else:
            if not isinstance(doc.get("env"), dict):
                doc["env"] = {}
            doc["env"][name] = value
    elif value == ABSENT:
        doc.pop(fix["key"], None)
    else:
        doc[fix["key"]] = value


def _target(fix: dict, loc: dict | None = None):
    if fix["kind"] == "set_env":
        return (loc or {}).get("name") or fix["names"][0]
    return fix["name"] if fix["kind"] == "remove_env" else fix["key"]


def _after(fix: dict):
    if fix["kind"] == "set_env":
        return fix["value"]
    return ABSENT if fix["kind"] == "remove_env" else False


def backup_name(path: str, now: dt.datetime | None = None) -> str:
    return f"{os.path.realpath(path)}.{(now or dt.datetime.now()).strftime('%Y%m%dT%H%M%S')}.bak"


def apply(check_id: str, checks: list[dict] | None = None, seen_for=None) -> tuple[list[str], int]:
    """Make the fix for one check in each editable file that needs it. Backs up each file first, edits only the
    one key, keeps the file mode, writes atomically, and records what it did so `revert` can undo it exactly."""
    checks = checks if checks is not None else load_checks()
    check = next((c for c in checks if c["id"] == check_id), None)
    if check is None:
        return [f"Not applied: there is no settings check named {check_id!r}. Known: {', '.join(c['id'] for c in checks)}."], 1
    fs = findings([check], seen_for)
    if not fs:
        return [f"Nothing to change: {check['title']} does not apply to your settings now."], 0
    f = fs[0]
    fix = check["fix"]
    edits = [l for l in f["locations"] if l["editable"] and l["path"]]
    if not check.get("applicable") or not edits:
        return ["Not applied: " + ("ask your administrator to change it in managed settings." if any(l["source"] == "managed" for l in f["locations"])
                                   else "this setting is not in a file Context Guru can change. Unset it where you set it.")], 1
    groups: dict[str, list[dict]] = {}
    for l in edits:
        groups.setdefault(l["path"], []).append(l)
    plans = []
    for path, locs in groups.items():
        try:
            doc, raw, mode = _read_doc(path) if os.path.exists(path) else ({}, "", 0o600)
        except (OSError, ValueError) as exc:
            return [f"Not applied: could not read {tilde(path)} ({exc.__class__.__name__}). Nothing was written."], 1
        want = _after(fix)
        todo = []  # (target key, value before) for each key in this file the fix must change
        for l in locs:
            t = _target(fix, l)
            before = _get(doc, fix, t)
            if before != want and t not in [x[0] for x in todo]:
                todo.append((t, before))
        if todo:
            plans.append((path, doc, raw, mode, todo))
    if not plans:
        return [f"Nothing to change: {check['title']} is already as the fix wants it in the files Context Guru can change."], 0
    rec = load_record()
    entries = rec.get(check_id, {}).get("entries", [])
    out = []
    for path, doc, raw, mode, todo in plans:
        existed = os.path.exists(path)
        bak = None
        try:
            if existed:
                bak = backup_name(path)
                _write_atomic(bak, raw.encode("utf-8"), mode)
            for t, _ in todo:
                _put(doc, fix, _after(fix), t)
            _write_atomic(path, _dump(doc, raw), mode)
        except OSError as exc:
            return out + [f"Not applied to {tilde(path)}: {exc.__class__.__name__}. Nothing else was written."], 1
        for t, before in todo:
            entries.append({"file": os.path.realpath(path), "display": path, "target": t, "kind": fix["kind"], "previous": before,
                            "wrote": _after(fix), "backup": bak, "created": not existed, "ts": int(dt.datetime.now().timestamp())})
        out.append(f"Applied: {fix['done']} in {tilde(path)}." + (f" Backup: {tilde(os.path.join(os.path.dirname(path), os.path.basename(bak)))}." if bak else ""))
    rec[check_id] = {"entries": entries}
    try:
        os.makedirs(state_dir(), exist_ok=True)
        _write_atomic(record_path(), json.dumps(rec, indent=2).encode("utf-8"), 0o600)
    except OSError as exc:
        out.append(f"Warning: the change was made, but the undo record could not be written ({exc.__class__.__name__}). Undo from the backup.")
    out.append(fix["effect"])
    out.append(f"Undo: /context-guru:save-more revert settings {check_id}")
    return out, 0


def revert(check_id: str, checks: list[dict] | None = None) -> tuple[list[str], int]:
    """Put back exactly the value the apply recorded, in each file, newest change first. Refuses, and writes
    nothing, if any of those files no longer holds what the apply wrote for that key."""
    checks = checks if checks is not None else load_checks()
    check = next((c for c in checks if c["id"] == check_id), None)
    rec = load_record()
    entries = rec.get(check_id, {}).get("entries") or []
    if check is None or not entries:
        return [f"Nothing to undo: no change to {check_id!r} was made here."], 1
    fix = check["fix"]
    groups: dict[str, list[dict]] = {}
    for en in reversed(entries):
        groups.setdefault(en["file"], []).append(en)
    plans = []
    for file, ens in groups.items():
        try:
            doc, raw, mode = _read_doc(file)
        except (OSError, ValueError) as exc:
            return [f"Not undone: could not read {tilde(ens[0]['display'])} ({exc.__class__.__name__}). Nothing was written."], 1
        for en in ens:
            if _get(doc, fix, en["target"]) != en["wrote"]:
                return [f"Not undone: {tilde(en['display'])} changed since the apply (the key {en['target']} is not what was written). "
                        "Nothing was written. Change it by hand."], 1
        plans.append((ens, doc, raw, mode))
    out = []
    for ens, doc, raw, mode in plans:
        en0 = ens[0]
        for en in ens:
            _put(doc, fix, en["previous"], en["target"])
        try:
            if en0.get("created") and not doc:  # the apply made this file; give it back as it was
                os.unlink(os.path.realpath(en0["file"]))
                out.append(f"Removed {tilde(en0['display'])}, which the apply had created.")
                continue
            _write_atomic(en0["file"], _dump(doc, raw), mode)
        except OSError as exc:
            return out + [f"Not undone for {tilde(en0['display'])}: {exc.__class__.__name__}."], 1
        out.append(f"Restored {', '.join(en['target'] for en in ens)} in {tilde(en0['display'])}.")
    rec.pop(check_id, None)
    try:
        _write_atomic(record_path(), json.dumps(rec, indent=2).encode("utf-8"), 0o600)
    except OSError:
        pass
    return out + [fix["effect"]], 0


if __name__ == "__main__":
    print("costly.py is used through cg.py: python3 cg.py save-more settings", file=sys.stderr)
    sys.exit(2)
