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
  settings.py add           --file PATH --url URL [--force] [--upstream URL] [--bin PATH] [--statusline CMD]
  settings.py add           --file PATH --statusline CMD [--force]   # statusline only, no routing change
  settings.py remove        --file PATH [--url URL] [--user-scope]
  settings.py show          --file PATH
  settings.py resolve-scope                                          # where THIS project's routing lives
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import re
import shutil
import socket
import subprocess
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
    way a test caught: another local API proxy answers on `http://127.0.0.1:4000/anthropic`, so "any local
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


def remove_statusline_only(data: dict) -> tuple[bool, str]:
    """Undo exactly what apply_statusline wrote, and nothing else — never touches `env`/routing.
    Returns (changed, restored_json). Shared by `off` (always this, regardless of whether routing
    is also present) and `remove`'s no-routing branch (statusline was the only thing installed).
    """
    meta = data.get(META)
    recorded = meta.get(STATUSLINE_META) if isinstance(meta, dict) else None
    if not recorded or data.get(STATUSLINE_KEY) != {"type": "command", "command": recorded}:
        return False, ""
    restored = ""
    del data[STATUSLINE_KEY]
    if meta.get(STATUSLINE_PREV_META):
        data[STATUSLINE_KEY] = meta[STATUSLINE_PREV_META]
        restored = json.dumps(meta[STATUSLINE_PREV_META], sort_keys=True)
    meta.pop(STATUSLINE_META, None)
    meta.pop(STATUSLINE_PREV_META, None)
    if not meta:
        data.pop(META, None)
    return True, restored


# Values that are URLs get their credentials taken out before they are printed. S4 in review, and
# pre-existing rather than introduced here — but it contradicts the principle this PR spent three
# rounds enforcing in reset.sh, and the leak is the same: `replaced=`, `previous=`, `was=`, `restored=`
# and `base_url=` all echo a base URL the user set, and a base URL can carry `user:pass@` or a
# credential in a query parameter. The install skill prints these lines to the user, which is exactly
# the output a person debugging a 401 pastes into an issue.
_URL_FACTS = ("base_url", "replaced", "previous", "was", "restored", "existing", "proposed",
              "upstream")


def redact_url(value: str) -> str:
    """Strip userinfo and query-string credentials from a URL, leaving the part that identifies it.

    Deliberately keeps scheme/host/path: the whole diagnostic value of these lines is telling the user
    WHICH endpoint they were pointed at, and "<redacted>" would answer the wrong question.
    """
    if not isinstance(value, str) or "://" not in value:
        return value
    scheme, _, rest = value.partition("://")
    host, slash, tail = rest.partition("/")
    if "@" in host:
        host = "<credentials not shown>@" + host.rsplit("@", 1)[1]
    out = f"{scheme}://{host}{slash}{tail}"
    if "?" in out:
        head, _, query = out.partition("?")
        parts = []
        for kv in query.split("&"):
            name = kv.split("=", 1)[0]
            if any(w in name.lower() for w in ("key", "token", "secret", "auth", "password",
                                               "credential", "sig")):
                parts.append(f"{name}=<value not shown>")
            else:
                parts.append(kv)
        out = head + "?" + "&".join(parts)
    return out


def emit(**facts: object) -> None:
    for k, v in facts.items():
        if k in _URL_FACTS and isinstance(v, str):
            v = redact_url(v)
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
    """Copy `path` aside into its recovery folder and return the copy's name. Never overwrites an
    existing backup.

    Lives in `context-guru-settings-json/` beside the file — the same folder `.pre-install` and
    `.pre-reset-*` use — not loose beside the settings file, which is where it used to live. That
    was a real, reported gap: a full copy of a settings file, sitting in a project's working tree,
    uncovered by the `.gitignore` entry `/context-guru:install` adds for the recovery folder,
    because it lived outside it.

    The stamp used to be second-granularity with a plain `copy2`, which meant an
    install-then-uninstall round trip — well inside one second — wrote both backups to the SAME
    filename, and the survivor held the POST-install state. The user was then told to keep that
    path as their undo, and it was a copy of the change, not of what preceded it.

    Microseconds plus O_EXCL: the exclusive create is what actually guarantees it, since two
    writes in the same microsecond are merely unlikely rather than impossible.
    """
    real = os.path.realpath(path)
    backup_dir = recovery_dir_for(real)
    try:
        ensure_dir_0700(backup_dir, chmod_parent=False)
        write_recovery_readme(backup_dir)
    except OSError:
        # Fail open, same principle as the rest of the recovery-folder mechanism: an unwritable
        # recovery folder must not turn an ordinary settings write into a hard failure. Falls back
        # to where backups lived before this folder existed.
        backup_dir = os.path.dirname(real)
    stem = recovery_stem(real)
    stamp = _dt.datetime.now().strftime("%Y%m%d-%H%M%S-%f")
    for attempt in range(100):
        dest = os.path.join(backup_dir, f"{stem}{BACKUP_SUFFIX}{stamp}"
                             + (f".{attempt}" if attempt else "") + BACKUP_EXT)
        try:
            fd = os.open(dest, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            continue
        try:
            with os.fdopen(fd, "wb") as out, open(path, "rb") as src:
                shutil.copyfileobj(src, out)
        except BaseException:
            # Same hole copy_once had (B1): a Ctrl-C here left a TRUNCATED file that the install skill
            # then reported as `backup=<path>` — "that is their undo". A partial copy presented as an
            # undo is worse than no backup, because it is trusted.
            try:
                os.unlink(dest)
            except OSError:
                pass
            raise
        shutil.copystat(path, dest)
        prune_backups(real, backup_dir)
        return dest
    raise RuntimeError(f"could not create a backup for {path}")


# How many backups of one settings file to keep. Each add and each remove writes one, so a user
# who installs and uninstalls a few times accumulated them forever — 40 files after 20 cycles, in
# a directory they read by hand. (An uninstall now deletes all of them anyway — see
# forget_backups() — so this window matters mainly across a run of edits that never uninstalls.)
KEEP_BACKUPS = 10


def prune_backups(path: str, backup_dir: str | None = None) -> None:
    """Delete all but the newest KEEP_BACKUPS backups of `path`, in `backup_dir` (defaults to
    `path`'s recovery folder — the normal case; `backup()` passes its own fallback directory
    explicitly when the recovery folder could not be created). Best effort.
    """
    import glob

    real = os.path.realpath(path)
    if backup_dir is None:
        backup_dir = recovery_dir_for(real)
    stem = recovery_stem(real)
    try:
        # glob.escape on the WHOLE joined path, not just the stem: `backup_dir` is derived
        # from the settings file's own directory, which can itself contain `[`, `?` or `*` (a
        # project at `~/projects/foo[1]/.claude/`, say) — escaping only the stem left the
        # directory half of the pattern live, so `proj[1]` was read as a character class and
        # matched nothing. Silent, because pruning is best-effort by design, and the backups it
        # exists to bound then accumulate without limit.
        found = sorted(glob.glob(glob.escape(os.path.join(backup_dir, stem)) + BACKUP_SUFFIX + "*" + BACKUP_EXT),
                       key=os.path.getmtime)
    except OSError:
        return
    for old in found[:-KEEP_BACKUPS]:
        try:
            os.remove(old)
        except OSError:
            pass


def forget_backups(path: str) -> None:
    """Delete every `.context-guru-backup-*` for `path`, best effort. Called after a successful
    uninstall (cmd_remove, cmd_off): once context-guru's own keys are gone from the file, the
    rolling per-edit checkpoints that led up to this point answer a question nobody has anymore —
    the only reference worth keeping is `.pre-install` (what restore reverts to) or, if the escape
    hatch ever runs here, `.pre-reset-*`. A growing pile of same-purpose backups beside them is
    just something to explain, not something to recover from.

    Not called from `maybe_delete_if_empty`'s success path: when the FILE itself is deleted there
    is nothing left worth curating individually, and that function removes the whole recovery
    folder instead.
    """
    import glob

    real = os.path.realpath(path)
    stem = recovery_stem(real)
    # Escape the WHOLE joined path — see the identical fix and comment in prune_backups().
    pattern = glob.escape(os.path.join(recovery_dir_for(real), stem)) + BACKUP_SUFFIX + "*" + BACKUP_EXT
    try:
        found = glob.glob(pattern)
    except OSError:
        return
    for old in found:
        try:
            os.remove(old)
        except OSError:
            pass


def post_uninstall_backup_note(deleted: bool) -> str:
    """What to report for `backup=` once an uninstall-type write has cleaned up after itself. The
    path `backup()` returned a moment earlier no longer exists by the time this prints — either
    `maybe_delete_if_empty` removed it along with the whole recovery folder, or `forget_backups`
    removed it on its own — so reporting that path would send someone looking for a file that
    is not there.
    """
    if deleted:
        return "(gone — deleted along with the file and its whole recovery folder)"
    return ("(gone — every backup is removed after a clean uninstall; "
            "see the recovery folder's .pre-install)")


# ---------------------------------------------------------------------------
# The escape hatch: a pre-edit copy of every file we touch, and a plain-sh script OUTSIDE THIS
# PLUGIN that puts it all back.
#
# The timestamped backups above are not enough on their own, and the reason is a real incident.
# `/context-guru:uninstall` is a SKILL — it needs a working Claude Code session. But the failure
# it exists for is "every request through the proxy fails", which is exactly the state in which
# no skill can run: the agent that would undo the routing cannot reach a model to be asked. A
# colleague hit that, 401 on every call, with the documented undo path unavailable for the same
# reason he needed it.
#
# So two things are established BEFORE any routing key exists, in one choke point that no write
# path can skip (see save()):
#
# 1. a per-directory recovery folder, `<dir>/context-guru-settings-json/`, next to the settings
#    file it covers — not in a shared state directory. This used to be a single global ledger
#    (`reset-manifest.tsv`) plus hashed copies under `~/.local/state/context-guru/originals/`, and
#    that design had two real problems: nobody who needed to recover something would ever think to
#    look in `~/.local/state`, and — worse — the ledger was append-only forever, so a file touched
#    once (a `--scope user` test months ago, a project cleanly uninstalled since) stayed on it
#    permanently, and `context-guru-reset` would happily revert that unrelated scope while trying
#    to fix a completely different, currently-broken one. Per-directory, scoped colocation makes
#    that class of bug structurally impossible: there is no shared list to leak from, and
#    `context-guru-reset` only ever looks at the (at most three) canonical settings-file paths, each
#    carrying its own folder. Inside it: `<basename>.pre-install` (a one-time, never-overwritten
#    copy of what the file looked like before context-guru's first edit — the backups beside the
#    file are capped at KEEP_BACKUPS and every add AND remove writes one, so that rolling window is
#    never where the ORIGINAL state should live), `<basename>.created-by-us` (an empty marker: its
#    presence is the only thing that means "put this back by deleting it, not restoring it" —
#    everything else defaults to the safer "never auto-delete"), and a short `README.md` explaining
#    what the folder is, written once.
# 2. the hatch script itself, copied out of the plugin into the (still centralised) state
#    directory. A hatch that lives inside the thing that broke goes away with `/plugin uninstall`,
#    a marketplace refresh, or a wiped plugin cache — i.e. it is missing in a fair share of the
#    cases it is for. The copy under the state directory has no dependency on this plugin still
#    existing, and it is the one thing that legitimately has no per-project home.
#
# All of it is best-effort: an unwritable directory must not fail an install that would otherwise
# work. It is REPORTED instead (`reset_hatch=`, `recovery_dir=`), so the skill can tell the user
# whether they have a hatch rather than assuming it.
# ---------------------------------------------------------------------------

HATCH_NAME = "context-guru-reset"
RECOVERY_DIR_NAME = "context-guru-settings-json"
# Content-bearing recovery files end in .json (an editor, a syntax highlighter, `file`, all treat
# them as what they are), so the SUFFIX carries it, not the settings file's own basename directly —
# `settings.local.json.pre-install` doesn't end in .json at all; `settings.local.pre-install.json`
# does. Every construction site below uses `recovery_stem()`, never the raw basename, to get there.
# `.created-by-us` is the one exception: it is an empty marker, not JSON content, so it keeps the
# plain name a marker file should have.
PRE_INSTALL_SUFFIX = ".pre-install.json"
CREATED_MARKER_SUFFIX = ".created-by-us"
# reset.sh (plain POSIX sh, no shared constants with this file) builds `.pre-reset-*.json` the
# same way independently — see its own PRE_RESET-equivalent naming there.
BACKUP_SUFFIX = ".context-guru-backup-"
BACKUP_EXT = ".json"
RECOVERY_README = """\
# context-guru recovery files

This folder was created by the context-guru Claude Code plugin, next to the settings file it
covers, so it is where you look for it — not buried in `~/.local/state`.

- `<name>.pre-install.json` — a one-time copy of that settings file from *before* context-guru's
  very first edit to it. This is what `/context-guru:uninstall` and the `context-guru-reset`
  escape hatch restore from.
- `<name>.created-by-us` — an empty marker. If present, context-guru created that file from
  nothing (it did not exist before), so undoing the install means deleting it, not restoring it.
- `<name>.pre-reset-<timestamp>.json` — a safety copy `context-guru-reset` takes of the file's
  current (routed) content right before it restores or deletes it, so running the hatch is itself
  undoable. The newest 10 are kept per file.
- `<name>.context-guru-backup-<timestamp>.json` — a copy taken before any other edit this plugin
  makes to the file. A clean `/context-guru:uninstall` (or a successful `context-guru-reset` run)
  deletes all of these for that file on its own; they answer a question nobody has once that has
  happened.

`<name>` is the settings file's own name with `.json` removed (`settings.local` for
`settings.local.json`), so every file above still ends in `.json` except the empty marker.

Full explanation: https://github.com/rossoctl/context-guru/blob/main/docs/how-to/plugin-recovery-files.md

This folder is safe to delete once you are sure you no longer need to recover anything through it.
`/context-guru:install` adds a `.gitignore` entry for it automatically when it can.
"""

# Facts collected during the run and printed once by main(), rather than from inside save() —
# the caller reads `key=value` lines and the result line should come first.
HATCH_FACTS: dict[str, str] = {}


def recovery_dir_for(real: str) -> str:
    """Where `real`'s recovery copies live: a folder beside it, shared by every settings file in
    the same directory (a `.claude/` holding both `settings.json` and `settings.local.json` gets
    one folder, not two) — see the module-level comment above for why this replaced a shared,
    hashed, append-only ledger under the state directory.
    """
    return os.path.join(os.path.dirname(real), RECOVERY_DIR_NAME)


def recovery_stem(real: str) -> str:
    """`real`'s basename with a trailing `.json` removed (`settings.local` for
    `settings.local.json`) — every recovery filename is built from this, not the raw basename, so
    that appending a suffix ending in `.json` produces a name that ends in `.json` ONCE, at the
    end, rather than in the middle (`settings.local.json.pre-install`).
    """
    return os.path.splitext(os.path.basename(real))[0]


def ensure_dir_0700(path: str, *, chmod_parent: bool = True) -> str:
    """makedirs `path`, force 0700 on it (and its parent, when `chmod_parent`), and return it.
    Raises on failure.

    B3 in review: a directory created by an older version of this script (or by anything else) may
    be group- or world-writable under a permissive umask, and trusting it is the whole
    vulnerability — another local account could replace an entry in a recovery folder and have
    `context-guru-reset` write attacker-chosen content into the victim's settings file. mkdir's
    mode argument is masked by the umask, so the explicit chmod is what actually sets it.

    `chmod_parent=False` for `recovery_dir_for()`'s result: unlike every other caller, that
    directory's parent is NOT ours — it is `.claude/` (or a project root for a user-scope install),
    which the user or their team may have deliberately set group- or world-readable, and it is
    reached on every single settings write. Reported and verified: a `775 .claude/` silently became
    `700` on the first write and stayed that way on every one after, undoing a real permission
    choice with nothing disclosing it. The recovery folder itself still gets 0700 either way — it
    can hold a credential-bearing copy of the settings file, the same threat model B3 describes —
    only the PARENT chmod is skippable, because only the parent is a directory this script does not
    own.
    """
    os.makedirs(path, mode=STATE_DIR_MODE, exist_ok=True)
    levels = (path, os.path.dirname(path)) if chmod_parent else (path,)
    for level in levels:
        try:
            if level and os.path.isdir(level) and (os.stat(level).st_mode & 0o077):
                os.chmod(level, STATE_DIR_MODE)
        except OSError:
            pass
    return path


def write_recovery_readme(recovery_dir: str) -> None:
    """Best-effort, write-once. Never overwrites — a user may have made this file their own, and a
    missing README is a cosmetic loss, never a reason to fail or repeat work.
    """
    path = os.path.join(recovery_dir, "README.md")
    if os.path.exists(path):
        return
    try:
        _write_atomic(path, RECOVERY_README.encode("utf-8"), mode=0o600)
    except OSError:
        pass


# ---------------------------------------------------------------------------
# Keeping the recovery folder out of git — deterministic, not a question.
#
# A full copy of a settings file can carry a credential, same as the settings file itself does
# today (nothing stops one living in `settings.local.json`, and the per-edit
# `.context-guru-backup-*` files already sit next to it, uncovered by any `.gitignore`, right now).
# Colocating the recovery folder in the project directory doesn't introduce that exposure; it's
# worth closing anyway, and cheaply: check whether the folder is already covered, and if it is not,
# add the one line that covers it. No question is asked — this is routine, reversible housekeeping
# in the same class as taking a backup automatically already is, not a decision like traffic
# interception or `--scope user` that changes what the user is exposed to. install.sh calls this
# unconditionally (see `route_ensure_gitignore` there) right after the routing write succeeds.
# ---------------------------------------------------------------------------


def _in_git_worktree(directory: str) -> bool:
    """Best-effort: is `directory` inside a git working tree? False on anything short of a clean
    "yes" — no git on PATH, not a repo, a git that errors for some other reason. This gate must
    never turn an ambiguous answer into a write.
    """
    try:
        result = subprocess.run(
            ["git", "-C", directory, "rev-parse", "--is-inside-work-tree"],
            capture_output=True, text=True, timeout=10, check=False)
    except (OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0 and result.stdout.strip() == "true"


def _git_already_ignores(directory: str, name: str) -> bool:
    """Is `name` (relative to `directory`) already covered by some `.gitignore` git can see? Only
    meaningful once `_in_git_worktree` has said yes. `check-ignore` exits 1 for "not ignored" —
    that is not an error, just the common case this function exists to detect.

    The trailing slash on `name` matters: this runs BEFORE the recovery folder necessarily exists
    on disk (a first-ever install's `add` may not have created it yet when this is called, and it
    is always true of a standalone `gitignore-ensure` call), and `git check-ignore` only matches a
    directory-only pattern (one ending in `/`, which is exactly what this writes) against a bare
    name when that name is confirmed to be a directory — either because it exists, or because the
    query itself is spelled with a trailing slash. Querying without one made an existing,
    correctly-written `.gitignore` entry read back as "not ignored" whenever the folder had not
    been created yet, which is the common case, not the rare one.
    """
    try:
        result = subprocess.run(
            ["git", "-C", directory, "check-ignore", "-q", name + "/"],
            capture_output=True, timeout=10, check=False)
    except (OSError, subprocess.SubprocessError):
        return False
    return result.returncode == 0


def cmd_gitignore_ensure(args: argparse.Namespace) -> int:
    """Make sure `RECOVERY_DIR_NAME` beside `args.file` is git-ignored, deterministically, with no
    question asked — see the module comment above. Every branch fails open: this must never block
    or fail an install, and it never touches anything but a `.gitignore` it can prove is needed.
    """
    directory = os.path.dirname(os.path.realpath(args.file))
    if not _in_git_worktree(directory):
        emit(result="skipped", reason="not_a_git_repo", dir=directory)
        return 0
    if _git_already_ignores(directory, RECOVERY_DIR_NAME):
        emit(result="unchanged", reason="already_ignored", dir=directory)
        return 0

    gitignore = os.path.join(directory, ".gitignore")
    pattern = RECOVERY_DIR_NAME + "/"
    try:
        existing = ""
        if os.path.exists(gitignore):
            with open(gitignore, encoding="utf-8") as fh:
                existing = fh.read()
        # Idempotent belt-and-suspenders: `check-ignore` above should already have caught this, but
        # a second, cheaper check here means a text-level match is never turned into a duplicate
        # line even if the two ever disagree at the edges (a pattern git resolves differently than
        # a literal string compare, for instance).
        if pattern in existing.splitlines():
            emit(result="unchanged", reason="already_present", file=gitignore)
            return 0
        new_content = existing
        if new_content and not new_content.endswith("\n"):
            new_content += "\n"
        new_content += pattern + "\n"
        _write_atomic(gitignore, new_content.encode("utf-8"), mode=0o644)
    except OSError as exc:
        emit(result="skipped", reason="unwritable", detail=f"{exc}")
        return 0
    emit(result="added", file=gitignore, pattern=pattern)
    return 0


def state_dir() -> str:
    """Where the hatch, the record and the original copies live. Overridable for tests."""
    override = os.environ.get("CONTEXT_GURU_STATE")
    if override:
        return override
    base = os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local", "state")
    return os.path.join(base, "context-guru")


def user_scope_files() -> list[str]:
    """The settings file(s) that route EVERY project on the machine.

    Kept as real paths so a symlinked ~/.claude (dotfiles) is still recognised as user scope.
    """
    base = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".claude")
    return [os.path.realpath(os.path.join(base, "settings.json"))]


def is_user_scope(path: str) -> bool:
    return os.path.realpath(path) in user_scope_files()


def user_scope_key() -> str:
    """The `install-scope.json` key a MACHINE-WIDE (`--scope user`) install files ITSELF under.

    A machine-wide install is not a project, and filing it as one was a real defect rather than a
    cosmetic one. Its record used to be keyed by `project_key(cwd)` — the project the user happened
    to be standing in when they ran the install — with two consequences, both silent:

      * allocation rule 1 ("this project already has a recorded port -> return it unchanged") handed
        the machine-wide install THAT PROJECT's port, so a project install and a machine-wide install
        made from inside it shared one proxy, one store and one preset. The whole reason ports are
        per-project is that sharing one made every session start kill and restart the proxy.
      * `record_install_scope` then overwrote that project's own row with scope=user and the
        user-scope file, so the project's install became invisible and a later uninstall THERE
        released the record describing the MACHINE-WIDE install.

    The user's own answer to "what about the projects that already route themselves" is `leave`,
    i.e. keep both. That answer could not be honoured from inside such a project: the two installs
    could not hold two ports because they could not hold two records.

    A real, existing directory rather than a sentinel like "(user)": `_recorded_ports` treats a
    record whose directory is gone as an orphan and reissues its port, so a sentinel key would have
    the machine-wide install's port quietly handed to the next project that asked. The user config
    dir is exactly as present as the settings file inside it that the install writes.
    """
    return os.path.dirname(user_scope_files()[0])


def scope_name_for(path: str, project_dir: str | None = None) -> str:
    """Which of the three scopes `path` is FOR `project_dir` (default cwd) — not purely from the
    path itself: "project-local" and "project" are relative to a project, and the project meant is
    `project_dir`, which is not always the caller's cwd. Mirrors install.sh's `route_scope_file()`
    mapping (project/team/user), named to match `_option_file_candidates()`'s own ordering
    (project-local, project, user). "custom" covers a hand-supplied path outside all three (e.g.
    `--attach` to something the user pointed at directly) — still worth recording, just not one of
    the three named scopes.

    `project_dir` matters as soon as a caller can pass a file that belongs to a DIFFERENT project
    than its own cwd — which `record_install_scope`'s migration/tiebreak paths can: the winning
    record after a sibling-worktree tiebreak may be a file under a worktree this call did not
    start in. Comparing that file against `os.getcwd()` would misreport it as "custom" (a real
    project-local file, just not THIS process's), which is exactly the bug this parameter exists
    to avoid. Every call site that predates this parameter passed no argument and got `os.getcwd()`
    — unchanged behavior for all of them.
    """
    base = project_dir or os.getcwd()
    real = os.path.realpath(path)
    if real == os.path.realpath(os.path.join(base, ".claude", "settings.local.json")):
        return "project-local"
    if real == os.path.realpath(os.path.join(base, ".claude", "settings.json")):
        return "project"
    if is_user_scope(path):
        return "user"
    return "custom"


# ---------------------------------------------------------------------------
# Project identity — the key a per-project record (install-scope.json's port field, and anything
# else genuinely per-project) is filed under.
#
# The defect this exists to fix: the plugin's state directory was keyed by PORT, and the port
# defaulted to 8787 for every project regardless of what each project's own options said. Options
# ARE correctly read per-project (see `_option_file_candidates`) — but two projects with
# different presets were still sharing one proxy process, and start-proxy.sh's fingerprint check
# resolved the disagreement by killing whichever proxy did not match the session that just
# started. Every kill wiped the in-memory cache/store — the exact regression the idle-exit floor
# in store/store_test.go already refuses to permit for cost reasons, arriving here for free every
# time two projects disagreed. `port_alloc()` (below) is the other half of the actual fix; this
# function only answers "which project is this", which is what a per-project port has to be keyed
# on to mean anything.
#
# realpath(dir) alone is NOT the right identity, because a git worktree's directory is not the
# project — a worktree of a repo IS that repo, sharing its options (`.claude/settings*.json`
# candidates are read relative to cwd, and a worktree's own `.claude/` is typically the checked-out
# tree, not a separate configuration) and, per the decision recorded in the plan this implements,
# meant to share its port too. So the key is the MAIN checkout's directory, derived from git's own
# notion of "the common dir every worktree of this repo shares" — and only realpath(dir) itself for
# anything that is not a git worktree (a bare repo, no git at all, or an unusual layout this does
# not recognise).
_PROJECT_KEY_CACHE: dict[str, str] = {}


def project_key(path: str | None = None) -> str:
    """The identity a per-project record is filed under: the MAIN checkout's directory for a git
    worktree, or realpath(dir) otherwise.

    Fails open, always, and never raises: any missing git, non-zero exit, unrecognised output, or
    timeout falls back to realpath(dir). This is reached from hook-time code paths (every
    SessionStart, by way of `resolve_install_scope`), where nothing may block a session over a
    project-identity lookup — a wrong-but-harmless identity (two projects that could have shared a
    port do not) is a fine outcome; a hang or a traceback is not.

    Memoised per process, keyed on realpath(dir): this is called from places that do not thread a
    single resolved directory through (config reads, hook starts), and re-forking git on every one
    of those calls in the same process would be the literal cost this identity was introduced to
    avoid duplicating.
    """
    real_dir = os.path.realpath(path or os.getcwd())
    cached = _PROJECT_KEY_CACHE.get(real_dir)
    if cached is not None:
        return cached
    key = _resolve_project_key(real_dir)
    _PROJECT_KEY_CACHE[real_dir] = key
    return key


def _resolve_project_key(real_dir: str) -> str:
    common_dir = _git_common_dir(real_dir)
    if common_dir and os.path.basename(common_dir.rstrip(os.sep)) == ".git":
        # The ordinary case, and the worktree case: a plain checkout's common-dir IS its own
        # `.git`, and every worktree of one repo shares that SAME common-dir (worktrees keep their
        # own `.git` FILE, pointing at `<main>/.git/worktrees/<name>`, but `--git-common-dir`
        # resolves past that to the one directory they all share) — so this branch returns the
        # same key for the main checkout and for every worktree branched from it, which is exactly
        # the "one repo, one record, one port" decision this function exists to implement.
        return os.path.realpath(os.path.dirname(common_dir))
    # A bare repo, or git succeeding with something this does not recognise. Guessing at an
    # identity for that case risks being confidently wrong; realpath(dir) is still correct, only
    # not worktree-aware, which is the safe direction to be wrong in.
    return real_dir


def _git_common_dir(real_dir: str) -> str:
    """The absolute `--git-common-dir` git reports for `real_dir`, or "" on anything short of a
    clean answer (missing git, a timeout, not a repo, non-zero exit). ~2s timeout, because this is
    reached from hook-time code paths and a hanging git must not hang a session start.
    """
    try:
        result = subprocess.run(
            ["git", "-C", real_dir, "rev-parse", "--path-format=absolute", "--git-common-dir"],
            capture_output=True, text=True, timeout=2, check=False)
    except (OSError, subprocess.SubprocessError):
        return ""
    if result.returncode == 0:
        out = result.stdout.strip()
        return os.path.realpath(out) if out else ""
    # `--path-format` needs git >= 2.31 ("unknown option" on anything older) — retry the bare form
    # and resolve its answer, which may be RELATIVE, against `real_dir` ourselves.
    try:
        result = subprocess.run(
            ["git", "-C", real_dir, "rev-parse", "--git-common-dir"],
            capture_output=True, text=True, timeout=2, check=False)
    except (OSError, subprocess.SubprocessError):
        return ""
    if result.returncode != 0:
        return ""
    out = result.stdout.strip()
    if not out:
        return ""
    return os.path.realpath(out if os.path.isabs(out) else os.path.join(real_dir, out))


def install_scope_path() -> str:
    """One file, machine-wide, keyed by project — there is one state directory, not one per repo."""
    return os.path.join(state_dir(), "install-scope.json")


def _record_still_routes(record: dict) -> bool:
    """Is `record["file"]` still the file actually routing us — i.e. does it still exist and
    still carry our env var? Fail-open: any read trouble (missing file, unreadable, corrupt JSON)
    reports False rather than raising, the same as a project that was never routed at all. Used
    only to break a tie between two migration-candidate records that both claim the same project;
    a wrong-but-harmless answer here just falls through to the `recorded_at` tiebreak below it.
    """
    file = record.get("file")
    if not file or not os.path.exists(file):
        return False
    try:
        data, _existed = load(file)
    except OSError:
        return False
    current = (data.get("env") or {}).get(KEY)
    return bool(current) and is_ours(data, current)


def _pick_still_routing_record(a: dict, b: dict) -> dict:
    """Tiebreak between two `install-scope.json` records that both migrate onto the same
    `project_key()` — see the MIGRATION TIEBREAK note on `resolve_install_scope`. Prefer whichever
    one's file is still actually routing; when that does not distinguish them (both or neither
    still route), prefer the more recently `recorded_at` one. `recorded_at` is an ISO-8601 UTC
    timestamp, so a plain string comparison orders it correctly; a record missing the field (it
    always has one, but this must never raise) sorts as older than any that has one.
    """
    a_routes, b_routes = _record_still_routes(a), _record_still_routes(b)
    if a_routes != b_routes:
        return a if a_routes else b
    return a if (a.get("recorded_at") or "") >= (b.get("recorded_at") or "") else b


def _valid_scope_record(record: object) -> bool:
    return isinstance(record, dict) and bool(record.get("scope")) and bool(record.get("file"))


def _sibling_legacy_records(new_key: str, old_key: str, projects: dict) -> list[tuple[str, dict]]:
    """Every pre-migration record in `projects`, OTHER than whatever is already filed under
    `new_key`, that belongs to the SAME project identity — this call's own `old_key` record (if
    it has one), plus, opportunistically, any OTHER worktree's still-unmigrated legacy record.

    This call's own `old_key` is trusted without re-deriving its `project_key()` — the caller
    already knows it resolves to `new_key`, by construction. Any OTHER key in the file is only
    probed (a fresh `project_key(key)` call, which forks `git`) when its directory still exists:
    a worktree that has since been deleted cannot be asked its own git-common-dir any more, and
    guessing at its identity from a stale path risks being confidently wrong. Left unprobed, it
    just sits in the file as an orphan record. Nothing routes through a deleted directory
    regardless of what its record says, so it misleads no reader — but it is not entirely free
    either: it names a port, and `_recorded_ports` skips exactly these records for that reason, so
    that an abandoned checkout does not hold a port out of every other project's pool forever.
    """
    out: list[tuple[str, dict]] = []
    own = projects.get(old_key)
    if _valid_scope_record(own):
        out.append((old_key, own))
    for key, record in projects.items():
        if key in (new_key, old_key) or not _valid_scope_record(record):
            continue
        if os.path.isdir(key) and project_key(key) == new_key:
            out.append((key, record))
    return out


def _read_install_scopes() -> dict:
    """{} on anything short of a valid JSON object — absent, unreadable or corrupt are all the
    same to a reader: nothing recorded yet. This is state a session can live without; it must
    never be the thing that turns a working install into a broken one.
    """
    path = install_scope_path()
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, json.JSONDecodeError):
        return {}
    projects = data.get("projects")
    return projects if isinstance(projects, dict) else {}


def record_install_scope(file: str, project_dir: str | None = None, scope_dir: str | None = None,
                         drop_keys: list[str] | None = None) -> None:
    """Record which scope `file` is, for THIS project (`project_dir`, default cwd), so a later
    command — the statusline skill, `preset`, anything else that writes a Claude settings file —
    can read back the choice the user already made instead of guessing at one of its own. Called
    from every routing-success branch of `cmd_add`. Fail-open: a write here must never fail the
    routing install it rides on.

    `project_dir` is accepted explicitly, rather than always reading `os.getcwd()`, so
    `resolve_install_scope`'s migration and self-heal branches — which may already have resolved
    an identity for a directory that is not necessarily the caller's cwd at the moment this runs —
    can record under the SAME key they just computed, instead of this function re-deriving it (and
    running `git` a second time) from whatever the cwd happens to be.

    `scope_dir` overrides ONLY what `scope_name_for(file, ...)` compares `file` against; it defaults
    to `project_dir`. They differ when a migration tiebreak's WINNER is a sibling worktree's file
    rather than this call's own: `scope_name_for` needs the directory that `file` actually lives
    under (the sibling's), not `project_dir` (this call's own, which is what `file` would need to
    live under to read as anything but "custom"). Getting this wrong only mislabels the `scope`
    field, never the `file` itself, so it is worth getting right but not worth blocking a write
    over.

    MIGRATION: `project_key()` postdates this file's original key, `realpath(cwd)`. A record still
    filed under that old key for this project is dropped here rather than left beside the new one
    — otherwise a project that predates `project_key()` would fork into two entries (one per
    identity) instead of having its one entry rewritten, and a later read keyed on the OLD identity
    (say, from a worktree whose cwd differs from its main checkout) would find stale data instead
    of nothing.

    `drop_keys` names ADDITIONAL keys to remove in the same write — the losing candidates of a
    migration tiebreak, which are this project's records under sibling worktrees' pre-migration
    keys. Only the caller that ran the tiebreak knows which keys those were, so they have to be
    passed in: without this, a loser sat in the file forever, and because `resolve_install_scope`
    re-probes every surviving sibling key on every call, each leftover cost a `git` fork on every
    SessionStart and had its `port` counted as another project's, burning a port for nothing.
    """
    try:
        projects = _read_install_scopes()
        scope = scope_name_for(file, scope_dir or project_dir)
        # A machine-wide install is filed under the machine, not under the project the user happened
        # to run it from — see user_scope_key(). Everything below about legacy keys and migration is
        # about PROJECT identity (`project_key()` postdating `realpath(cwd)`, worktrees collapsing
        # onto one key) and none of it applies here: the cwd's row belongs to the cwd's project,
        # which may be routing itself on its own port, and popping it is precisely the damage this
        # keying exists to stop. So the user-scope branch migrates nothing and drops nothing.
        user_scoped = scope == "user"
        new_key = user_scope_key() if user_scoped else project_key(project_dir)
        old_key = os.path.realpath(project_dir or os.getcwd())
        old_entry = (None if user_scoped else
                     projects.pop(old_key, None) if old_key != new_key else None)
        # Losers are dropped, but their `port` is still this project's port if nothing else in the
        # file carries one — see the carry-forward note below. `new_key` is never dropped.
        dropped = [] if user_scoped else [projects.pop(key) for key in (drop_keys or [])
                                          if key != new_key and key in projects]
        existing_new_entry = projects.get(new_key)

        # This call IS authoritative about scope/file/recorded_at — it is running because routing
        # (or a migration/tiebreak that just decided the same thing) knows THIS is where the
        # project routes now. Carrying those three fields over from a stale entry would leave a
        # stale `file` in place after e.g. a scope change — exactly the kind of stale-pointer bug
        # Phase 3 exists to fix for proxies, and not one to reintroduce here.
        #
        # `port`, however, is allocated by an entirely separate command (`port alloc`, added in a
        # later phase) that this call knows nothing about; it is the one field to carry FORWARD
        # rather than silently reset, so a routing rewrite never looks like a port release. The
        # entry already at `new_key` wins over a stale `old_key` legacy entry if both have one,
        # which in turn wins over a dropped sibling's — weakest source first, last write wins.
        #
        # A row describing the MACHINE-WIDE install is not one of those candidates, whatever key it
        # is filed under — and before `user_scope_key()` existed it was filed under the project it
        # was run from, so it turns up here as `old_entry`/`existing_new_entry` for a PROJECT install
        # in that directory. Carrying its `port` forward filed the machine-wide port as this
        # project's own with `scope=project`, while this very call was recording routing on a
        # different port: `cmd_port`'s rule-1 laundering again, by the other door. (Narrow in
        # practice — `install.sh` allocs at step 0, which now moves the row before this runs — but
        # `add --url` by hand reaches it.) So it is skipped as a port source, and MOVED to the key
        # the user branch above would have used rather than overwritten by this write.
        candidates: list = [*dropped, old_entry, existing_new_entry]
        if not user_scoped:
            user_key = user_scope_key()
            project_candidates = []
            for candidate in candidates:
                if _is_machine_wide_row(candidate):
                    if user_key not in projects:
                        projects[user_key] = dict(candidate)
                    continue
                project_candidates.append(candidate)
            candidates = project_candidates
        entry: dict = {}
        for candidate in candidates:
            if isinstance(candidate, dict) and "port" in candidate:
                entry["port"] = candidate["port"]
        entry["scope"] = scope
        entry["file"] = os.path.realpath(file)
        entry["recorded_at"] = _dt.datetime.now(_dt.timezone.utc).isoformat()
        projects[new_key] = entry
        ensure_state_dir(state_dir())
        _write_atomic(install_scope_path(),
                       (json.dumps({"version": 1, "projects": projects}, indent=2) + "\n")
                       .encode("utf-8"), mode=0o600)
    except OSError:
        pass


def resolve_install_scope(project_dir: str | None = None) -> tuple[str | None, str | None, str | None]:
    """(scope, file, source) for `project_dir` (default cwd) — the one place every settings-writer
    other than `add`'s own routing write should ask "which file", instead of hardcoding one.

    `source` is "recorded" (install already ran and told us, filed under the current key),
    "inferred" (no record under either key, but one of the three candidate files is routing THIS
    project — a project that predates this feature, or was routed by hand; the answer is
    self-healed into the record so the probe below runs at most once per project), or None
    (nothing anywhere — the caller's answer is "ask").

    MIGRATION, which must not be skipped: `install-scope.json` records written before
    `project_key()` existed are keyed on the OLD identity, `realpath(cwd)`. Every read here tries
    the current key first and, only on a miss, the old one — and a hit under the old key is
    rewritten under the new one and dropped from the old, the same self-healing shape the
    "inferred" branch below already uses for a project with no record at all. Skipping this would
    leave every pre-existing install invisible to `project_key()`-keyed lookups: `resolve-scope`
    would report "(ask)" for a project that is, in fact, already routed.

    MIGRATION TIEBREAK: `project_key()` collapses every worktree of one repo onto the SAME
    `new_key`, but each worktree kept its OWN pre-migration record (one per `realpath(worktree)`).
    Those records migrate lazily, one `project_dir` at a time — so the FIRST call for ANY worktree
    of a repo can land here with no entry at `new_key` yet, only its own `old_key` legacy record,
    while a SIBLING worktree's legacy record (under a different, not-yet-migrated `old_key`) sits
    unexamined elsewhere in the same file. Migrating blindly onto whichever call happens to run
    first would let an abandoned worktree's stale record win permanently over the one that is
    actually live, just because it was resolved first. So every call gathers every candidate that
    could be THIS project's record — the current `new_key` entry if any, this call's own `old_key`
    legacy entry if any, and any OTHER not-yet-migrated key whose directory still exists and whose
    own `project_key()` also comes out to `new_key` — and picks among ALL of them: whichever
    record's `file` is still ACTUALLY routing (still has our env var pointed at it) wins, and only
    when that does not distinguish them (all or none still route) does the more recently
    `recorded_at` one win. A sibling worktree that was later deleted cannot be probed any more (its
    directory is gone) and is left as a harmless orphan record rather than guessed at.
    """
    projects = _read_install_scopes()
    new_key = project_key(project_dir)
    old_key = os.path.realpath(project_dir or os.getcwd())

    recorded = projects.get(new_key)
    candidates: list[tuple[str, dict]] = []
    if isinstance(recorded, dict) and recorded.get("scope") and recorded.get("file"):
        candidates.append((new_key, recorded))
    if old_key != new_key:
        candidates.extend(_sibling_legacy_records(new_key, old_key, projects))

    winner_key, winner = None, None
    for key, record in candidates:
        if winner is None:
            winner_key, winner = key, record
        elif _pick_still_routing_record(winner, record) is record:
            winner_key, winner = key, record

    if winner is not None:
        # A single candidate that is ALREADY the current `new_key` entry needs no write — the
        # common case, and the one the fast path used to take before any sibling ever existed.
        # Anything else (a legacy record to migrate, or more than one candidate to collapse into
        # the winner and clean the losers out of the file) is worth the write.
        if not (len(candidates) == 1 and candidates[0][0] == new_key):
            # `winner_key` is the directory `winner["file"]` actually lives under — this call's
            # own `old_key`, or a sibling worktree's key from `_sibling_legacy_records` — UNLESS
            # the winner is the entry already at `new_key`, whose own originating directory this
            # call was never told; `scope_dir=None` there falls back to `project_dir` (see
            # `record_install_scope`'s docstring for what that can still get wrong).
            scope_dir = winner_key if winner_key != new_key else None
            record_install_scope(winner["file"], project_dir, scope_dir=scope_dir,
                                 drop_keys=[key for key, _ in candidates if key != new_key])
        return winner["scope"], winner["file"], "recorded"

    prev_cwd = os.getcwd()
    try:
        if project_dir:
            os.chdir(project_dir)
        for path in _option_file_candidates(""):
            if not os.path.exists(path):
                continue
            data, _existed = load(path)
            current = (data.get("env") or {}).get(KEY)
            if current and is_ours(data, current):
                record_install_scope(path, project_dir)
                return scope_name_for(path), os.path.realpath(path), "inferred"
    finally:
        os.chdir(prev_cwd)
    return None, None, None


# ---------------------------------------------------------------------------
# Port allocation — one port per project, always.
#
# The defect this half of the fix removes: the port defaulted to 8787 for EVERY project, so two
# projects with different presets shared one proxy process, and start-proxy.sh's fingerprint
# check resolved the disagreement by killing whichever proxy did not match — wiping the in-memory
# cache/store on every flip between them. `project_key()` (above) answers "which project is
# this"; this section answers "which port is THIS project's, and does it collide with anyone
# else's" — the two questions a per-project port has to answer to mean anything.
#
# Allocation order, cheapest and least surprising first:
#   1. this project already has a recorded port -> return it UNCHANGED. Reinstall, update and
#      repair must never move a project's port: the URL is already written into a settings file,
#      and moving it here would silently break every session still pointed at the old one.
#   2. an EXPLICITLY configured port (the key is PRESENT in pluginConfigs[plugin].options, not
#      merely equal to the default) -> honour it and record it, never reallocated later even if
#      the option is subsequently removed from the file.
#   3. otherwise scan upward from the default, skipping every port another project has already
#      recorded and every port that refuses to bind right now.
# The default is 8787 (plugin.json's `port.default`, asserted equal to this by a drift test —
# see the note on `_explicit_configured_port` about presence vs. value). `CONTEXT_GURU_PORT_BASE`
# overrides where the SCAN starts, without touching that default: it exists only so a test suite
# running on a box another engineer's real services also use can point the scan at a range nobody
# else holds, rather than actually binding real ports in 8787..8850 on a shared machine. Never set
# by a normal install; there is no user-facing reason to move the scan window.
PORT_SCAN_START = int(os.environ.get("CONTEXT_GURU_PORT_BASE") or 8787)
PORT_SCAN_END = PORT_SCAN_START + 63  # "8787..8850 is plenty" — 64 ports is far more than this
                       # machine will ever have projects for, and an unbounded scan is a way to
                       # hang on a machine whose firewall makes every bind attempt time out
                       # instead of refuse.


def _port_bindable(port: int) -> bool:
    """Best-effort: can 127.0.0.1:<port> be bound right now? Bound and closed immediately,
    deliberately WITHOUT SO_REUSEADDR, so a port something else is actively listening on is
    reported as taken.

    This is TOCTOU by nature — nothing stops a second `install` running between this probe and
    the proxy actually binding. That is accepted deliberately rather than added locking, because
    Phase 3 makes start-proxy.sh REPORT a foreign occupant instead of killing it: the failure mode
    of a lost race is a clear conflict message, never silent co-tenancy or a killed proxy.
    """
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        sock.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        sock.close()


def _explicit_configured_port(plugin: str, only: list[str] | None = None) -> tuple[int | None, str]:
    """(port, file) from the first candidate file (most specific first) where `options.port` is
    literally PRESENT, or (None, "") if none has it.

    Presence, not value, is what "explicit" means here — deliberately. Comparing the configured
    value against the plugin.json default (8787) cannot tell "the user typed 8787 on purpose"
    apart from "nothing is configured and 8787 is simply what applies", and the two have to be
    treated differently: the first must never be reallocated even though it happens to equal what
    an unconfigured project would have gotten anyway.

    `only` narrows the candidates, for a caller asking on behalf of something that is not the cwd
    project. The default list is Claude Code's own precedence, project-local first, which is the
    right answer for every question asked ABOUT this project and the wrong one for a MACHINE-WIDE
    install: standing in a project that pins its own port, "is a port configured for the
    machine-wide install" would answer with that project's. See cmd_port's alloc.
    """
    for path in (only if only is not None else _option_file_candidates(plugin)):
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, json.JSONDecodeError):
            continue
        opts = (((data.get("pluginConfigs") or {}).get(plugin) or {}).get("options") or {})
        if not isinstance(opts, dict) or "port" not in opts:
            continue
        value = opts["port"]
        # bool is an int subclass in Python — exclude it explicitly, since `"port": true` is not a
        # port and treating it as one (True == 1) would silently "honour" a typo as port 1.
        if isinstance(value, int) and not isinstance(value, bool):
            return value, path
        if isinstance(value, str) and value.strip().isdigit():
            return int(value), path
        # Present but not a usable number — a hand-edited file, most likely. Not trustworthy
        # enough to honour OR to record; fall through to the scan rather than write back
        # something nobody actually configured.
        continue
    return None, ""


def _is_machine_wide_row(rec: object) -> bool:
    """Does this row describe the MACHINE-WIDE install? `scope` is the discriminator `install.sh`
    already uses; the `file` check catches a row written before `scope` was recorded.
    """
    if not isinstance(rec, dict):
        return False
    file = rec.get("file") or ""
    return rec.get("scope") == "user" or bool(file and is_user_scope(file))


def _machine_wide_record(projects: dict) -> tuple[str, dict] | None:
    """The row describing a MACHINE-WIDE install, under whatever key it happens to be filed.

    NOT `projects.get(user_scope_key())`. That key is new: every machine-wide install made before it
    existed is filed under the project the user happened to run it from, carrying `scope=user` and
    the machine-wide file. Keying this question on the key made every check built on it fire only
    for installs this version created — so on an existing machine the checks were all no-ops, which
    is the same as not having written them. `scope` is the discriminator `install.sh` already uses
    for the same question, and it is in the row regardless of what the row is called.

    First match wins: there is one machine-wide install by definition (one file, one option, one
    port), and a file holding two such rows is a legacy row plus its migrated self, which name the
    same install. Callers that must not hand out a machine-wide port use `_machine_wide_ports`
    instead, which does not have to pick.
    """
    for key, rec in projects.items():
        if _is_machine_wide_row(rec):
            return key, rec
    return None


def _machine_wide_ports(projects: dict) -> set[int]:
    """Every port any machine-wide row records — for the scan, which must not hand out a port some
    install is serving on. `_machine_wide_record` picks one row because the guards need one answer;
    a port must not be reissued just because its row lost that tie-break.
    """
    ports = set()
    for _key, rec in projects.items():
        if _is_machine_wide_row(rec) and isinstance(rec.get("port"), int):
            ports.add(rec["port"])
    return ports


def _recorded_ports(exclude_key: str) -> set[int]:
    """Every port some OTHER project already has recorded, so the scan in `cmd_port`'s `alloc`
    never hands out one of them. `exclude_key` is this project's own key — its own recorded port
    (if any) is not a collision with itself, and rule 1 already returns it unchanged before this
    is ever consulted.

    A record whose project directory no longer exists is SKIPPED. Without that, every deleted
    checkout held its port out of the pool permanently: the scan range is 64 ports wide, uninstall
    is the only thing that releases a record, and deleting a project directory is a far more
    ordinary way for one to end than running uninstall first. The pool would shrink by one for
    every clone a user ever threw away, and the eventual failure is `no_port_available` on a
    machine where nothing is listening on any of them.

    Skipping is safe because it is not the last line of defence: the caller only accepts a
    candidate that `_port_bindable` also confirms nothing is listening on, so re-issuing a port
    whose record is an orphan can never steal one from a live proxy. It can only reuse a number
    whose owner is genuinely gone. (`proxy-<port>.owner` is the other half — it is what refuses an
    install onto a port a LIVE proxy owns, and uninstall removes it alongside the record.)

    Returns (used, orphans), where `orphans` maps a port to the absent-directory records holding
    it. Skipping alone was not enough and the second half is not optional: the orphan record STAYS
    in the file, and rule 1 of `alloc` hands a recorded port straight back, so a directory that
    came back — a re-clone at a path used before, an unmounted volume returning — ended up with a
    record naming a port that had since been reissued to someone else. Two projects on one port is
    the exact defect this whole change exists to remove, so the caller drops the orphan record in
    the same write that reissues its port.
    """
    used: set[int] = set()
    orphans: dict[int, list[str]] = {}
    for key, rec in _read_install_scopes().items():
        if key == exclude_key:
            continue
        if not isinstance(rec, dict) or not isinstance(rec.get("port"), int):
            continue
        # os.path.isdir, not any attempt to re-derive the project's identity: a deleted directory
        # cannot be asked anything, and the only question here is whether it is still there.
        if os.path.isdir(key):
            used.add(rec["port"])
        else:
            orphans.setdefault(rec["port"], []).append(key)
    return used, orphans


def owner_token() -> tuple[str, str]:
    """Who owns the proxy on this project's port, as `start-proxy.sh` must compare it.

    Returns (token, scope).

    The owner file exists so that one project never kills a proxy another project is being served
    by. `project_key()` was the wrong token for that, because it answers a different question:
    "who am I", not "who is this port shared with". Under `--scope user` there is ONE settings
    file, therefore one `port` option, therefore ONE port shared by every project on the machine —
    that being exactly what machine-wide means — while every project still has its own
    `project_key()`. Keying the owner on the project made every project but the first a stranger to
    the proxy it is supposed to be using: the veto fires before the fingerprint comparison, so a
    preset change made from any non-owning project never restarted the proxy and never took
    effect, and every one of those sessions was told "this project should have its own port", which
    is the precise opposite of the install the user asked for.

    So the token is keyed on the ROUTING SCOPE, which is the thing that actually decides whether
    the port is shared: a machine-wide route yields one token for every project that shares it; a
    project-scoped route yields this project's key, because only that project routes to it.
    """
    scope, file, _source = resolve_install_scope()
    if scope == "user":
        # The user settings file, not the bare word: two HOMEs on one machine (a test harness, a
        # second account) are two machine-wide installs, and they must not claim each other's
        # proxy just because both call themselves `user`.
        return "user:" + (os.path.realpath(file) if file else user_scope_files()[0]), scope
    return project_key(), scope


def cmd_owner_token(args: argparse.Namespace) -> int:
    token, scope = owner_token()
    scope = scope or "(none)"   # nothing routed yet; a literal "None" in the output would be noise
    if not args.observed:
        emit(result="ok", owner=token, scope=scope)
        return 0

    observed = args.observed.strip()
    if observed == token:
        emit(result="ok", owner=token, scope=scope, verdict="ours", reason="exact")
        return 0

    if scope == "user" and not observed.startswith("user:"):
        # A bare path under a MACHINE-WIDE route. Two ways to get here, and they need opposite
        # answers, so the file alone cannot decide — the install records can:
        #
        #  - a proxy started before this token existed, by whichever project happened to go first.
        #    Ours: the port is shared by every project this route covers, and deferring to a claim
        #    from a scheme we no longer use would veto the restart forever, on state nobody can
        #    clear. That is the regression finding 2 of the review is about — a preset change made
        #    from any non-owning project never took effect.
        #  - a project that installed ITSELF on this port and is genuinely being served by that
        #    proxy. Theirs, machine-wide route or not: it is not covered by this route (its own
        #    settings file is more specific and keeps winning), so stopping its proxy would take
        #    away the one it is actually using.
        #
        # The discriminator is whether that path still routes itself, which is precisely what the
        # `--on-existing-projects` gate asks about. Note `adopt` makes the answer "no" for the
        # projects it folds in, which is right: they are covered by this route afterwards.
        rec = _read_install_scopes().get(observed)
        if not (isinstance(rec, dict)
                and rec.get("scope") in ("project", "project-local", "custom")
                and bool(rec.get("file")) and os.path.isdir(observed)):
            emit(result="ok", owner=token, scope=scope, verdict="ours", reason="legacy_unscoped",
                 observed=observed)
            return 0

    # Any other difference is a stranger, unchanged from the plain string comparison this replaced:
    # under a PROJECT-scoped route the port belongs to one project, so a different owner — recorded
    # or not, existing or not — is somebody else's and is left alone. Deliberately not self-healed
    # here: a project-scoped session has its own port to go to, so the cost of being wrong is a
    # session without a proxy, against killing a proxy another project is using.
    emit(result="ok", owner=token, scope=scope, verdict="theirs", observed=observed)
    return 0


def cmd_port(args: argparse.Namespace) -> int:
    key = project_key()
    # `alloc` and `show` act on the record NAMED, when one is named. This is how a machine-wide
    # install reaches its own record (user_scope_key()) instead of the record of whichever project
    # the user is standing in: without it, rule 1 below hands a `--scope user` install that
    # project's port, and the two installs cannot hold two ports.
    #
    # LITERALLY, with no project_key() resolution, unlike `release`'s own --key handling further
    # down: an allocation writes the key it was given, so a key that resolved to something else
    # would record the port under a row the caller never named. `release` resolves as a FALLBACK
    # only because it also has to accept a plain directory from `adopt`.
    if getattr(args, "key", None) and args.op in ("alloc", "show"):
        key = args.key

    if args.op == "show":
        rec = _read_install_scopes().get(key)
        port = rec.get("port") if isinstance(rec, dict) else None
        # `record=` and `option_port=` exist so a caller can undo PRECISELY. install.sh's step 0
        # commits the port before anything else can fail, and its unwind could not tell "the record I
        # just made" from "the record that was already here": `result=ok` also covers
        # source=recorded and source=configured, so a failed RE-install of a working install deleted
        # that project's record and the user's pinned `options.port` while `ANTHROPIC_BASE_URL` still
        # named the old port — routed at a port the next session would not even compute. Read the
        # prior state, undo only what you made.
        state = "present" if isinstance(port, int) else "absent"
        opt: object = "(none)"
        if args.file:
            try:
                with open(args.file, encoding="utf-8") as fh:
                    data = json.load(fh)
                opts = (((data.get("pluginConfigs") or {}).get(args.plugin) or {})
                        .get("options") or {})
                if isinstance(opts, dict) and "port" in opts:
                    opt = opts["port"]
            except FileNotFoundError:
                pass
            except (OSError, json.JSONDecodeError):
                # Unreadable is NOT absent. A caller that treats it as absent would "undo" an option
                # it never saw, which is the same overreach in a quieter form.
                opt = "(unreadable)"
        if isinstance(port, int):
            emit(result="ok", port=port, record=state, option_port=opt)
        else:
            emit(result="ok", port="(none)", record=state, option_port=opt,
                 note="no port recorded for this project")
        return 0

    if args.op == "release":
        # Uninstall's call. Removes the WHOLE project record, not just the port field — a project
        # that has stopped routing has nothing left for `resolve_install_scope` to answer either,
        # and leaving a scope/file/port shell behind would misreport it as still configured.
        # Fail-open: this must never be load-bearing for uninstall completing, per the plan.
        projects = _read_install_scopes()
        # --key names the project directly, for a caller acting on a project it is not running in
        # (install.sh's `adopt`). Without it the only way to name a project was to `cd` into it,
        # which is a second way for a path to get mangled on the way there.
        #
        # The LITERAL key first, and only then the resolved one. `project_key()` of a git worktree is
        # the MAIN CHECKOUT, so resolving unconditionally meant that for a record keyed by a worktree
        # path — which is what every install from a worktree before the migration left behind, since
        # the migration lives in `record_install_scope` and the `scopes` gate never reaches a write
        # path — this popped the main checkout's row instead. `adopt` then deleted the record the
        # running install had just written for itself and left the row it was told to remove in
        # place: the exact inverse of the request, silently, reported as `released`.
        if getattr(args, "key", None):
            # The machine-wide row is named literally whether or not it is still there. Resolution is
            # a fallback for a plain DIRECTORY, and user_scope_key() is a directory - so for anyone
            # whose home is a git checkout (versioned dotfiles) project_key("~/.claude") answers the
            # repo root, and the release names a row nothing files anything under. Today that can only
            # fail to pop a row that is already gone, which is why it is one line and not a fix to a
            # visible bug.
            if args.key in projects or args.key == user_scope_key():
                key = args.key
            elif os.path.isdir(args.key):
                key = project_key(args.key)
            else:
                key = args.key
            # And never this project, whatever the key resolved to. A caller that names ANOTHER
            # project can only be wrong if it ends up here, and the record this process is running
            # under is the one nothing else can reconstruct — `ANTHROPIC_BASE_URL` in the settings
            # file survives it and names a port nobody can compute again. The own-project release is
            # uninstall's, and uninstall passes no --key.
            #
            # Fail-open like the rest of `release`: exit 0, because this must never be the thing that
            # blocks an uninstall, and a refusal that is reported is not a failure.
            if key == project_key():
                emit(result="refused", reason="key_is_this_project", key=args.key, resolved=key,
                     note="--key names the project this process is running in; releasing it here "
                          "would delete the caller's own record. Run `release` without --key to "
                          "release this project deliberately.")
                return 0
        removed = projects.pop(key, None)
        if removed is None:
            emit(result="unchanged", note="no record for this project")
            return 0
        try:
            ensure_state_dir(state_dir())
            _write_atomic(install_scope_path(),
                           (json.dumps({"version": 1, "projects": projects}, indent=2) + "\n")
                           .encode("utf-8"), mode=0o600)
        except OSError as exc:
            emit(result="skipped", reason="unwritable", detail=f"{exc}",
                 note="the record could not be removed, but this must never block uninstall")
            return 0
        released_port = removed.get("port", "(none)") if isinstance(removed, dict) else "(none)"
        emit(result="released", port=released_port)
        return 0

    if args.op == "unset":
        # Remove the `port` OPTION from one settings file, leaving everything else in it alone.
        # Needed by install.sh's `--on-existing-projects adopt`: a project being folded into a
        # user-scope install stops having its own route, and a leftover per-project `port` would
        # then point its hooks at a port nothing serves — worse than the state before, because the
        # project looks configured. Deliberately does NOT touch install-scope.json; `release` owns
        # that, and the caller runs both.
        if not args.file:
            emit(result="error", reason="unset_needs_a_file")
            return 3
        data, existed = load(args.file)
        if not existed:
            emit(result="unchanged", file=args.file, note="no such file")
            return 0
        options = (((data.get("pluginConfigs") or {}).get(args.plugin) or {}).get("options"))
        if not isinstance(options, dict) or "port" not in options:
            emit(result="unchanged", file=args.file, note="no port option here")
            return 0
        saved = backup(args.file)
        del options["port"]
        save(args.file, data)
        emit(result="removed", file=args.file, backup=saved)
        return 0

    # op == alloc
    projects = _read_install_scopes()
    user_key = user_scope_key()
    mw_key, mw_rec = _machine_wide_record(projects) or (None, None)
    rec = projects.get(key)
    # A row describing the MACHINE-WIDE install is not this project's record, whatever key it is
    # filed under — and on every machine that installed before `user_scope_key()` existed, it is
    # filed under a project's. Rule 1 below ("this project already has a recorded port -> return it
    # unchanged") then handed a PROJECT install in that directory the machine-wide port, and
    # `record_install_scope` rewrote the row as `scope=project` carrying that port: one shared port
    # laundered into a row that looks perfectly legitimate, and the documented cleanup for a
    # legacy row ("re-install or uninstall in that project") was the thing that did it.
    #
    # `key == user_key` is the one caller the row does belong to — that is the machine-wide install
    # asking for its own port, which is exactly what rule 1 is for.
    migrate_mw_row = False
    if rec is not None and rec is mw_rec and key != user_key:
        rec = None
        # Disowning the row is not enough: the write at the end of this function is
        # `projects[key] = dict(rec or {}) | {"port": port}`, so a disowned row under THIS key is
        # OVERWRITTEN by a bare `{"port": <this project's new port>}`. Measured: this project got its
        # own port correctly, and then the machine-wide install had no record at all — so the very
        # next project on the machine read the machine-wide `options.port` and answered
        # `configured`, because every protection here asks `_machine_wide_record` and the row it asks
        # about was gone. The guard erased the evidence the guard depends on.
        #
        # So MOVE it, in the same write, to the key `record_install_scope`'s user branch would have
        # used. This is the one rewrite of a legacy row that is safe: the key is derived, not
        # guessed, and `scope`/`file`/`port` are carried verbatim — it renames a row that already
        # describes the machine-wide install, rather than inferring anything about it.
        migrate_mw_row = True

    drop_orphans: list[str] = []
    if isinstance(rec, dict) and isinstance(rec.get("port"), int):
        port, source = rec["port"], "recorded"
        # VERIFY, do not assume. Returning a recorded port unchanged is right — the URL naming it
        # is already written into a settings file, and moving it would strand that file — but it
        # must not paper over a port two live projects both have recorded. Reachable from a
        # hand-edited install-scope.json, a restored backup, or (before the orphan drop above) a
        # directory that came back after its port was reissued. No allocation can resolve this:
        # both records are equally real, and picking one silently is how the projects end up
        # sharing a proxy with different presets. `port=` is still emitted, deliberately, so a
        # caller that fails open does so onto THIS project's own recorded port rather than onto
        # the plugin.json default, which would be a different project's proxy again.
        # SHARING ONE FILE IS NOT A CLASH. A machine-wide (`--scope user`) install is one settings
        # file, therefore one `port` option, therefore one port for every project it routes — by
        # design, that being what machine-wide means. So the question is not "does another project
        # have this port" but "does another project ROUTE ITSELF to it": same file, no clash;
        # different file (or no file yet, which cannot be shown to be the same one), a clash.
        my_file = rec.get("file") if isinstance(rec, dict) else None
        clash = sorted(k for k, r in projects.items()
                       if k != key and isinstance(r, dict) and r.get("port") == port
                       and os.path.isdir(k)
                       and not (my_file and r.get("file") == my_file))
        if clash:
            emit(result="error", reason="port_recorded_by_another_project", port=port,
                 source=source, other_project=clash[0], others=len(clash),
                 note=f"port {port} is recorded for this project AND for {clash[0]}; nothing was "
                      "written. Clear the port option in one of them (or uninstall there) so a "
                      "fresh port is allocated — two projects on one port is what this allocation "
                      "exists to prevent")
            return 3
    else:
        # Rule 2 must not hand either kind of install the OTHER kind's port. The invariant is
        # symmetric — a machine-wide install and a project install on one port means one proxy, one
        # store, one preset and a kill-and-restart at every session start — so both directions are
        # excluded here, and the record is what tells them apart.
        #
        # Outwards: a machine-wide install reads its own file only. Project-local options outrank user
        # ones, so from inside a project that pins a port — including one this plugin pinned there
        # itself, which an `allocated` install writes into that project's settings — rule 2 handed
        # the machine-wide install THAT project's port.
        only = user_scope_files() if key == user_key else None
        explicit_port, explicit_file = _explicit_configured_port(args.plugin, only)
        # Inwards: `_option_file_candidates`' last entry IS the machine-wide file, so a PROJECT install
        # read the machine-wide install's own `options.port` and called it `configured` — the same
        # defect in the other direction, on the order the docs recommend (machine-wide first, then a
        # project). Worse than a shared number, because `configured` skips the option write below: the
        # project's hooks would resolve their port THROUGH a file it does not own, and the machine-wide
        # uninstall unsetting that option (correctly) would drop every project that inherited it to the
        # plugin.json default while its own URL still named the old port.
        #
        # A user-level `port` with NO machine-wide record is a different thing — a pin the user
        # typed, which every project should honour. The record is the whole distinction, and it
        # exists — under whatever key, which is why this asks `_machine_wide_record` and not the
        # key. Asking the key made this fire only for installs this version created, i.e. never on
        # an existing machine.
        if (explicit_port is not None and key != user_key and is_user_scope(explicit_file)
                and mw_rec is not None):
            # Falls through to the scan, which excludes the machine-wide port via `_recorded_ports`
            # (its key is a real directory, so it is not an orphan) and writes the port it picks into
            # this project's own file.
            explicit_port = None
        if explicit_port is not None:
            port, source = explicit_port, "configured"
        else:
            used, orphans = _recorded_ports(key)
            # A machine-wide port is never a candidate, even when its row is filed under THIS key
            # (the legacy shape above, which `_recorded_ports` skips as "this project's own") and
            # even when its directory is gone (an orphan row cannot make a routing file stop naming
            # the port it names). Every such row, not just the one `_machine_wide_record` picks.
            used |= _machine_wide_ports(projects)
            port = None
            for candidate in range(PORT_SCAN_START, PORT_SCAN_END + 1):
                if candidate in used or not _port_bindable(candidate):
                    continue
                port = candidate
                break
            # Whatever absent-directory records were holding the port we just took go with it — see
            # _recorded_ports. Nothing is lost by dropping them: if such a project comes back, its
            # own settings file still carries the `port` option this command wrote, so rule 2
            # (`configured`) hands it the same port back without the record.
            drop_orphans = list(orphans.get(port, [])) if port is not None else []
            if port is None:
                emit(result="error", reason="no_port_available",
                     note=f"every port from {PORT_SCAN_START} to {PORT_SCAN_END} is either "
                          "recorded by another project or refused to bind; nothing was written")
                return 3
            source = "allocated"

    if args.dry_run:
        # A preview only — no write to install-scope.json, no write to any settings file, no
        # reservation. Used by install.sh's `--plan`, which must not write anything at all: an
        # allocation made during a plan a user never confirms would commit this project to a port
        # (and take it out of the pool for every other project) before consent was even asked.
        emit(result="ok", port=port, source=source,
             note="dry run: nothing was recorded or written")
        return 0

    if args.promised_port is not None and port != args.promised_port:
        # The port a `--dry-run` reported (and this project's user consented to, in whatever URL
        # install.sh's --plan showed) is no longer the port a REAL alloc would pick — someone else
        # took it, or an explicit config changed, in the gap between the plan and this call.
        # plugin.json says the port "must be FIXED rather than negotiated": writing a URL nobody
        # actually agreed to would negotiate it anyway, silently. Refuse instead of writing
        # anything, distinctly from "no port available" — the caller must re-plan and get fresh
        # consent for whatever port is real now, not just retry.
        emit(result="error", reason="port_changed_since_plan", promised=args.promised_port,
             port=port, note="the port promised at plan time is no longer the one a real "
                  "allocation would pick; nothing was written — re-run --plan for fresh consent")
        return 3

    try:
        entry = dict(rec) if isinstance(rec, dict) else {}
        entry["port"] = port
        if migrate_mw_row and user_key not in projects:
            # Before `projects[key]` is reassigned, and only when the destination is free: two rows
            # naming the machine-wide install is the legacy row plus its migrated self, and the one
            # already under `user_key` is the newer of the two.
            projects[user_key] = dict(mw_rec)
        projects[key] = entry
        for orphan_key in drop_orphans:
            projects.pop(orphan_key, None)
        ensure_state_dir(state_dir())
        _write_atomic(install_scope_path(),
                       (json.dumps({"version": 1, "projects": projects}, indent=2) + "\n")
                       .encode("utf-8"), mode=0o600)
    except OSError as exc:
        emit(result="error", reason="unwritable_install_scope", detail=f"{exc}",
             note="the port was chosen but could not be recorded; a later call may pick a "
                  "different one")
        return 3

    # Also write it into the plugin's OWN options, at the scope routing already uses — not a
    # second scope decision, the SAME one `resolve_install_scope` already answers for the
    # statusline and for `preset`'s fallback (see cmd_preset's own `target is None` branch, which
    # takes the identical fallback below when nothing is configured or routed yet). This is what
    # makes every hook see the right port with no new lookup: CLAUDE_PLUGIN_OPTION_PORT already
    # resolves per-scope once this key exists in the file the scope points at.
    #
    # Skipped when the port came from rule 2 (`configured`): it is already present in whichever
    # file the user (or a previous install) put it in, and writing it again into a DIFFERENT file
    # — `target` here need not be the same file `_explicit_configured_port` found it in — would
    # create a redundant second copy rather than honour the one that already exists.
    target = args.file
    if source != "configured":
        if not target:
            _scope, resolved_file, _source = resolve_install_scope()
            target = resolved_file or os.path.join(".claude", "settings.local.json")
        data, existed = load(target)
        plugins = data.setdefault("pluginConfigs", {})
        if not isinstance(plugins, dict):
            emit(result="error", reason="pluginConfigs_not_an_object", file=target)
            return 3
        plugin_entry = plugins.setdefault(args.plugin, {})
        if not isinstance(plugin_entry, dict):
            emit(result="error", reason="plugin_entry_not_an_object", file=target)
            return 3
        options = plugin_entry.setdefault("options", {})
        if not isinstance(options, dict):
            emit(result="error", reason="options_not_an_object", file=target)
            return 3
        if options.get("port") != port:
            # A JSON int, never a string — plugin.json types `port` as `"number"`, and a stray
            # quoted value here would make `CLAUDE_PLUGIN_OPTION_PORT` carry the string "8787"
            # rather than the number every reader of it expects.
            if existed:
                backup(target)
            options["port"] = port
            save(target, data)

    emit(result="ok", port=port, source=source, file=target or "(unchanged; already configured)")
    return 0


def _write_atomic(path: str, data: bytes, mode: int = 0o600) -> None:
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=os.path.basename(path) + ".tmp-")
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


# 0700, and re-asserted rather than assumed. B3 in review: the COPIES were hardened to 0600 and the
# DIRECTORIES were not, so under a permissive umask (`umask 000` reproduces it) both came out
# drwxrwxrwx. Another local account could then replace an entry in a recovery folder and have
# reset.sh write attacker-chosen content into the victim's settings file — one that controls where
# all their model traffic goes and can hold a credential. mkdir's mode argument is masked by the
# umask, so the explicit chmod (in `ensure_dir_0700`, defined above alongside `recovery_dir_for`) is
# what actually sets it.
STATE_DIR_MODE = 0o700


def ensure_state_dir(*parts: str) -> str:
    """makedirs the state path, force 0700 on every level we own, and return it. Raises on failure.

    A thin wrapper over `ensure_dir_0700` for state-dir callers that build their path from parts
    rather than already holding a full one.
    """
    return ensure_dir_0700(os.path.join(*parts))


def copy_once(src: str, dest: str) -> str:
    """Copy `src` to `dest` exactly once, atomically. Returns `dest`, or "" if it could not be done.

    B1 in review, and the most serious defect the hatch has had: this used to open `dest` with
    O_EXCL and stream into it. Only OSError unlinked a partial, and **KeyboardInterrupt is a
    BaseException** — so a Ctrl-C during the first install (the install is the slow, interruptible
    part, and a user watching it hang is exactly who presses Ctrl-C) left a TRUNCATED file at `dest`.
    O_EXCL then guaranteed by design that it was never replaced. reset.sh gated on `-f` rather than
    `-s`, and its "verify" step greps for key names — which a destroyed file passes trivially — so the
    hatch restored an 8-byte fragment over a real settings file, printed "Done. 1 file(s) put back.",
    and exited 0. Reproduced.

    The irony was precise: the never-overwrite property that makes this copy trustworthy is exactly
    what turned a transient interrupt into permanent, unrecoverable state.

    So: stream into a temp name, fsync it, then hardlink it into place. os.link is atomic and fails if
    the destination exists, which preserves never-overwrite *without* a partially written destination
    ever being reachable. The temp is removed on every path including BaseException — which is allowed
    to propagate, because a user pressing Ctrl-C means it.
    """
    tmp = f"{dest}.partial-{os.getpid()}"
    try:
        try:
            fd = os.open(tmp, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except OSError:
            return ""
        with os.fdopen(fd, "wb") as out, open(src, "rb") as fh:
            shutil.copyfileobj(fh, out)
            out.flush()
            os.fsync(out.fileno())
        try:
            os.link(tmp, dest)
        except FileExistsError:
            # Already recorded by an earlier run. That copy is authoritative; keep it.
            return dest if os.path.exists(dest) else ""
        except OSError:
            return ""
        return dest
    finally:
        try:
            os.unlink(tmp)
        except OSError:
            pass


def install_hatch(state: str) -> str:
    """Copy reset.sh out of the plugin and into `state`. Returns the installed path, or "".

    Refreshed when the content differs, so a plugin update updates the hatch — the copy is the
    one that will actually be run, and a stale copy of a recovery tool is its own hazard.
    """
    src = os.path.join(os.path.dirname(os.path.abspath(__file__)), "reset.sh")
    dest = os.path.join(state, HATCH_NAME)
    try:
        with open(src, "rb") as fh:
            new = fh.read()
    except OSError:
        # Source gone (a partial plugin install). An older copy already out here is still valid.
        return dest if os.path.exists(dest) else ""
    try:
        current = None
        if os.path.exists(dest):
            with open(dest, "rb") as fh:
                current = fh.read()
        if current != new:
            _write_atomic(dest, new, 0o700)
        elif not os.access(dest, os.X_OK):
            os.chmod(dest, 0o700)
    except OSError:
        return ""
    # A second copy where the user's shell will find it by name. Only if the directory already
    # exists — this is the proxy binary's own destination, so on a machine that ran the install it
    # does; creating it would be a PATH change nobody asked for.
    bindir = os.path.join(os.path.expanduser("~"), ".local", "bin")
    if os.path.isdir(bindir):
        try:
            link = os.path.join(bindir, HATCH_NAME)
            existing = None
            if os.path.exists(link):
                with open(link, "rb") as fh:
                    existing = fh.read()
            if existing != new:
                _write_atomic(link, new, 0o700)
        except OSError:
            pass
    return dest


def _created_by_us(real: str) -> bool:
    """Did context-guru's own first edit CREATE `real`, per the `.created-by-us` marker in its
    recovery folder? Missing, unreadable or unrecorded all answer False — the side that never
    deletes a file we are not certain we brought into existence.
    """
    marker = os.path.join(recovery_dir_for(real), recovery_stem(real) + CREATED_MARKER_SUFFIX)
    return os.path.exists(marker)


def maybe_delete_if_empty(path: str, data: dict) -> bool:
    """If removing our keys left `data` with nothing else in it AND context-guru created `path`,
    delete the file instead of leaving an empty `{}` shell behind — uninstalling from a project
    that had no settings file at all must not end with one. Returns True if the file was deleted,
    in which case the caller must not also call save().

    Never deletes a file that held something before context-guru's first edit (per
    `_created_by_us`): that content — a theme, a permission grant, another plugin's config — is
    the user's, whatever is left in `data` right now, and removing it would take that with it.

    Also removes the file's entire recovery folder, not just its backups: with the file gone,
    there is no `.pre-install` to hold (it never existed either — see `_created_by_us`) and nothing
    left worth curating individually.
    """
    if data:
        return False
    real = os.path.realpath(path)
    if not _created_by_us(real):
        return False
    try:
        os.remove(path)
    except OSError:
        return False
    shutil.rmtree(recovery_dir_for(real), ignore_errors=True)
    return True


def _is_loopback(url: str) -> bool:
    """Does this URL name this machine? Used only as the weakest signal in _looks_routed_by_us.

    Written out rather than a `startswith` tuple because every shape the tuple missed was a false
    NEGATIVE — no port (`http://127.0.0.1/anthropic`), `0.0.0.0`, `[::1]` without a port, `https://`
    — and a false negative here is the direction that re-introduces the defect the caller exists to
    prevent: taking a copy of an already-routed file and calling it an original.
    """
    rest = url
    for scheme in ("http://", "https://"):
        if rest.startswith(scheme):
            rest = rest[len(scheme):]
            break
    else:
        return False
    host = rest.split("/", 1)[0]
    if host.startswith("["):                      # [::1] or [::1]:8787
        host = host.split("]", 1)[0] + "]"
    else:
        host = host.split(":", 1)[0]
    return host in ("127.0.0.1", "localhost", "[::1]", "0.0.0.0", "::1")


def _looks_routed_by_us(real: str) -> bool:
    """Is this file ALREADY in our post-install state, so a copy of it is not an "original"?

    This is the guard on a severe defect found in review. `record_touch` runs from `save()`, and
    `save()` is also `cmd_remove`'s write path — so when the FIRST recorded touch of a file was a
    REMOVAL, the O_EXCL "copy taken before the first edit" was a copy of the ROUTED file, and being
    O_EXCL it was permanent. The hatch then faithfully restored routing into an unrouted project:
    the one behaviour a recovery tool cannot have, reproduced, and worst on exactly the population
    this hatch is for — every pre-hatch install that upgrades and then uninstalls.

    Content, not intent: `remove` is the obvious way in, but threading "this is an install" through
    `save()` would still take a routed copy when an ADD runs against a file we had already routed
    (a wiped state directory, a re-run with --force). What makes a copy meaningless is that the file
    already carries our work, whoever is writing and why.

    Erring is asymmetric, so this errs one way on purpose. A false positive costs the user a
    whole-file restore they could have had, and the hatch says so honestly and points at the
    timestamped backups. A false negative re-applies the routing they ran the hatch to escape.

    A base URL that is NOT ours is deliberately not a signal — a user's own loopback gateway
    (another local API proxy, say) is precisely the value most worth having a copy of. Hence
    the record and our own keys below, plus the narrow loopback test, rather than "any base URL".
    """
    try:
        with open(real, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, json.JSONDecodeError, UnicodeDecodeError):
        # Unparseable never reaches a write (load() refuses first), so this is a file we are not
        # going to touch anyway. Say "not ours" and let the O_EXCL copy happen: an unreadable file
        # is the case where a byte-for-byte copy is worth most.
        return False
    if not isinstance(data, dict):
        return False
    if META in data:
        # We have written this file before, and recorded it. The strongest signal there is, and the
        # one the pre-hatch population carries: this metadata predates the hatch.
        return True
    env = data.get("env")
    if not isinstance(env, dict):
        return False
    if UPSTREAM_KEY in env or BIN_KEY in env:
        return True   # nobody else writes these two
    url = env.get(KEY)
    if isinstance(url, str) and _is_loopback(url):
        # A pre-hatch install old enough to have no metadata. Ambiguous with a user's own loopback
        # proxy, and resolved toward the safer failure per the docstring.
        return True
    return False


def record_touch(real: str, existed: bool) -> None:
    """Fail-open wrapper. See _record_touch.

    `except Exception` rather than `except OSError`: "fail open, always" is a hard boundary in this
    repo, so the hatch machinery must not be able to fail a settings write for ANY reason — the
    install it would break is one that would otherwise have worked.
    """
    try:
        _record_touch(real, existed)
    except Exception as exc:                      # noqa: BLE001 - deliberate, see docstring
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"{type(exc).__name__}: {exc}"


def _record_touch(real: str, existed: bool) -> None:
    """Note that we are about to edit `real`, and make sure a way back exists.

    Called from save() before the write, so `existed` is the truth about the file as the user had
    it. Idempotent: `.pre-install` is created via `copy_once` (O_EXCL under the hood) and
    `.created-by-us` is only ever written if neither marker exists yet — the tenth edit never
    overwrites what the first one recorded. The `_looks_routed_by_us` branch has no marker of its
    own by design; it is a pure function of the file's current content, so re-deriving the same
    answer on every call is fine and even more robust than caching one.
    """
    recovery = recovery_dir_for(real)
    try:
        ensure_dir_0700(recovery, chmod_parent=False)
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write {recovery}: {exc}"
        return
    write_recovery_readme(recovery)

    stem = recovery_stem(real)
    pre_install = os.path.join(recovery, stem + PRE_INSTALL_SUFFIX)
    created_marker = os.path.join(recovery, stem + CREATED_MARKER_SUFFIX)

    skipped_copy = False
    if not os.path.exists(pre_install) and not os.path.exists(created_marker):
        # First time THIS file is being recorded — decide, once, what "before" means for it.
        if not existed:
            try:
                _write_atomic(created_marker, b"", mode=0o600)
            except OSError:
                pass
        elif _looks_routed_by_us(real):
            # No original to take — the moment for that already passed, possibly in a version of
            # this plugin that predates recovery folders (or one that predates them existing for
            # THIS file). `ensure_hatch()` reaches the same state through its own path; both leave
            # neither marker behind, and both are reported identically below.
            #
            # Deliberately not reported as unavailable here without checking `pre_install` first —
            # a review finding on the old design: an ordinary uninstall of an ordinarily-installed
            # project takes this branch (the file IS ours by then), and reporting "no original" at
            # this point would have overwritten a good, verified copy already sitting on disk. That
            # cannot happen with this design (there is nothing here to overwrite), but the same
            # "check what's actually there, not what this call decided" discipline is kept below.
            skipped_copy = True
        else:
            copy_once(real, pre_install)

    hatch = ""
    try:
        # install_hatch() writes straight into state_dir() with no makedirs of its own — it relied
        # on ensure_state_dir(state, "originals") having already created the parent as a side
        # effect, back when that ran unconditionally here. It does not anymore (only the RECOVERY
        # directory is ensured above), so the state directory needs its own explicit creation.
        ensure_state_dir(state_dir())
        hatch = install_hatch(state_dir())
    except OSError:
        pass
    HATCH_FACTS["reset_hatch"] = hatch or "unavailable"
    HATCH_FACTS["recovery_dir"] = recovery
    if os.path.exists(pre_install):
        HATCH_FACTS["recovery_original"] = pre_install
    elif os.path.exists(created_marker):
        HATCH_FACTS["recovery_original"] = "(created by us; nothing to restore)"
    elif existed:
        HATCH_FACTS["recovery_original"] = "unavailable"
        HATCH_FACTS["recovery_original_reason"] = (
            "the file already carried context-guru's keys when it was first recorded"
            if skipped_copy else "no pre-edit copy could be taken")


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
    # Every write in this script funnels through here, which is why the hatch is established here
    # and not in the six callers: `add` alone has six exits that write, and one of them forgetting
    # to record the file would produce a routed machine with no recorded way back — the one bug
    # class this must not have. `real`, not `path`: a dotfiles symlink is restored by writing the
    # file it points at, the same reason the write below resolves it.
    record_touch(real, os.path.exists(real))
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
        # Provenance, from the record rather than the URL's shape. install.sh decided "already
        # routed" by matching `127.0.0.1:<port>` against the current value, which is the inference
        # valid_base_url()'s own docstring forbids: two local proxies are indistinguishable by URL,
        # so somebody else's proxy on our port read as ours and the conflict was never reported.
        # `show` emitted nothing carrying this, so the caller had nothing better to use. Now it does.
        ours=str(bool(current) and is_ours(data, current)).lower(),
    )
    return 0


def cmd_resolve_scope(_args: argparse.Namespace) -> int:
    """Read-only: which file THIS project's routing already lives in, so a skill never has to
    hardcode `~/.claude/settings.json` or guess. Every settings-writer other than `add`'s own
    routing write should call this instead of deciding scope on its own — see `resolve_install_scope`.
    """
    scope, file, source = resolve_install_scope()
    if scope is None:
        emit(result="ok", scope="(ask)", file="(ask)", source="(none)",
             note="no routing recorded or found for this project; ask project vs. machine-wide, "
                  "the same choice /context-guru:install would ask")
        return 0
    emit(result="ok", scope=scope, file=file, source=source)
    return 0


def cmd_project_key(args: argparse.Namespace) -> int:
    """Read-only: the identity this project's per-project state is filed under. Nothing but a
    lookup — `project_key` itself already fails open to `realpath(cwd)`, so this cannot fail in a
    way a caller has to handle, which is what lets start-proxy.sh use it on a hook path.

    `--user-scope` answers the other identity: the one a MACHINE-WIDE install files itself under.
    It is here rather than computed in install.sh so that where that record lives has ONE encoding —
    two (one in Python, one in bash) is how a record ends up written under a key nothing looks up.
    """
    if getattr(args, "user_scope", False):
        emit(result="ok", key=user_scope_key(), scope="user")
        return 0
    emit(result="ok", key=project_key())
    return 0


def cmd_scopes(_args: argparse.Namespace) -> int:
    """Read-only: every per-project routing record there is, one line each.

    Exists for install.sh's `--scope user` gate. A user-scope install routes EVERY project on the
    machine, so a project that already has its OWN routing is about to start overriding the
    machine-wide one it just asked for — silently, because the more specific settings file simply
    wins. The install cannot answer whether that is what the user wanted, so it has to be able to
    LIST them and ask; that is all this does.

    One line per record, each carrying the whole record, rather than numbered keys: the consumer is
    bash, and a `project_1=`/`scope_1=`/`port_1=` shape makes it assemble records out of parallel
    arrays, which is how a report ends up pairing one project's path with another's port.
    """
    projects = _read_install_scopes()
    emit(result="ok", count=len(projects))
    for key in sorted(projects):
        record = projects[key]
        if not isinstance(record, dict):
            continue
        # A record with a port but no scope/file is NOT a project that routes itself. `port alloc`
        # writes the port before the routing write, so a plan that stopped at a later gate, or an
        # install that failed after step 0, leaves exactly that shape behind. Reported on its own
        # line: the `--scope user` gate reads `existing_project=` to decide whose routing would
        # override the machine-wide one, and a project with no routing overrides nothing — listing
        # it there refused an install over a project that was never installed.
        line = "existing_project" if (record.get("scope") and record.get("file")) \
            else "port_only_record"
        print("{}={} scope={} port={} file={}".format(
            line, key, record.get("scope") or "(unknown)", record.get("port") or "(none)",
            record.get("file") or "(none)"))
    return 0


def ensure_hatch(file: str) -> None:
    """Put a hatch in place for a project that is already routed, without editing anything.

    Called only when `cmd_add` reports `result=unchanged` — i.e. the file is confirmed routed to
    us already — so `_record_touch`'s own `_looks_routed_by_us` check takes its "no pre-edit copy
    to take" branch: that moment already passed, possibly in a version of this plugin that
    predates recovery folders. This used to be a separate function that unconditionally assumed
    that outcome instead of checking for it; collapsed here because the check and the resulting
    facts are otherwise a second encoding of exactly what `_record_touch` already does, and a
    second encoding is how these two drifted before.
    """
    try:
        _record_touch(os.path.realpath(file), existed=True)
    except Exception as exc:                      # noqa: BLE001 - deliberate, see record_touch
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"{type(exc).__name__}: {exc}"


def cmd_add(args: argparse.Namespace) -> int:
    # SHAPE GATE, before anything is read or written. See valid_base_url() for why this is not
    # is_ours() and why it does not touch --upstream. Exit 2 rather than 1: this is a refusal a
    # caller can distinguish from a failure, the same code the scope and conflict gates use.
    if args.url:
        if why := valid_base_url(args.url):
            emit(result="error", reason="invalid_base_url", detail=why, url=args.url,
                 note="refused before writing: a routing key pointing at this would break every "
                      "request, and `add` reporting success is what made that hard to notice")
            return 2
    # SCOPE GATE — on anything this call would write into the machine-wide file: ROUTING or
    # STATUSLINE. Read the statusline flag early: neither read below depends on `load()`.
    sl_command = getattr(args, "statusline", "")
    #
    # B2 in review (superseded, see below): the gate used to be `args.url`-only, so `add --file
    # ~/.claude/settings.json --statusline <cmd>` — a call that writes no routing key at all — was
    # refused with `reason=user_scope_needs_flag`. Six documented invocations broke. The reasoning
    # at the time was that a statusline is machine-wide by design and routes nothing, so it was
    # exempted from the routing gate entirely.
    #
    # That covered "what happens if this write fails" and missed "what happens if it succeeds": a
    # statusline written at user scope RENDERS in every project on the machine — the exact blast
    # radius this gate exists to require sign-off for, just described with "renders in" instead of
    # "routes". `install.sh` used to install the status line at user scope unconditionally on
    # every install, which is what let a project-scope routing install silently write and back up
    # the user's machine-wide settings file. It now writes the status line to the same file
    # routing used (see `record_install_scope`/`resolve_install_scope`), so this gate is mostly
    # defense-in-depth for a direct or hand-written call that bypasses that resolution — but it
    # still has to hold, because a skill hardcoding a path is exactly how this went wrong before.
    #
    # A project-scope install that goes wrong breaks one project. The same mistake in
    # ~/.claude/settings.json breaks EVERY Claude Code session on the machine — including the ones
    # the user would use to fix it, and including every project that has nothing to do with
    # context-guru. That asymmetry is why project scope is the documented default.
    #
    # It was ONLY documented, though: the default lived in the install skill's prose, and the script
    # would write whatever --file it was handed. A prompt is not a guardrail — it can be skipped, or
    # read differently by the next model, and the failure it guards against is a machine-wide
    # lockout. So the refusal lives here, where nothing can talk it round, and takes an explicit
    # flag that means the user was asked. Removal is deliberately NOT gated: uninstall must be able
    # to clean every scope, and blocking recovery would be the wrong side to err on.
    if (args.url or sl_command) and is_user_scope(args.file) and not getattr(args, "user_scope", False):
        writes = "both" if (args.url and sl_command) else ("routing" if args.url else "statusline")
        blast = {"routing": "routes EVERY project on the machine",
                 "statusline": "renders in EVERY project on the machine",
                 "both": "routes and renders in EVERY project on the machine"}[writes]
        emit(result="error", reason="user_scope_needs_flag", file=args.file, writes=writes,
             note=f"this file {blast}, so it needs --user-scope as well. Confirm with the user "
                  "first, naming that blast radius; project scope (.claude/settings.local.json) "
                  "is the default for a reason.")
        return 2

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

    # --statusline (read above, at the scope gate) is checked here, ahead of every base_url branch
    # below, for the same reason the `desired` set above exists at all: a conflict on it must stop
    # the WHOLE call, including the base_url side, rather than writing half an install and
    # reporting success. It is a TOP-LEVEL key (see apply_statusline), so it cannot join
    # `desired`/`env` above; this is its own gate.
    sl_desired = {"type": "command", "command": sl_command} if sl_command else None
    if sl_command:
        existing_sl = data.get(STATUSLINE_KEY)
        if existing_sl is not None and not is_ours_statusline(data, existing_sl) and not args.force:
            emit(result="conflict", reason="statusline_already_set", file=args.file,
                 existing=json.dumps(existing_sl, sort_keys=True),
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
        # --no-backup: install.sh's own statusline follow-up call passes this. It runs seconds
        # after the routing `add` in the SAME install, which already backed up the file's
        # pre-install state — a second backup here would only capture "routed, no statusline yet",
        # a state nobody would ever want to restore to, and it left two backup files on disk for
        # one conceptual install. `/context-guru:statusline`'s own standalone calls never pass this,
        # so they still get the backup a user-initiated change is entitled to.
        saved = "" if args.no_backup else (backup(args.file) if existed else "")
        apply_statusline(data, sl_command)
        save(args.file, data)
        emit(result="added", file=args.file,
             backup=saved or ("(covered by the routing install's backup)" if args.no_backup
                               else "(new file)"),
             statusline=sl_command)
        return 0

    current = env.get(KEY)
    if current == args.url and all(env.get(k) == v for k, v in desired.items()) and sl_unchanged:
        record_install_scope(args.file)
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
        record_install_scope(args.file)
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
        record_install_scope(args.file)
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
        # `reason=` is emitted because callers read it. install.sh's step 7 reports
        # `detail=$(kv "$aout" reason)`, and this path emitted `existing=`/`proposed=` but no reason -
        # so the one line that would have explained a refusal came back EMPTY. Fixed here rather than
        # in the reader: every caller of this script keys on `reason=`, so a refusal that does not
        # carry one is the defect, and naming it once fixes it for all of them.
        emit(result="conflict", reason="base_url_already_set", file=args.file, existing=current,
             proposed=args.url,
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
    record_install_scope(args.file)
    emit(result="added", file=args.file, base_url=args.url,
         replaced=current if current else "", backup=saved or "(new file)",
         other_env_keys=len([k for k in env if k not in OURS]))
    return 0


def cmd_off(args: argparse.Namespace) -> int:
    """Turn the status line off, and only the status line — unlike `remove`, this never touches
    `env`/routing even when both were installed together. Ships alongside statusline being on by
    default so there is a one-word undo that does not also require uninstalling the proxy.
    """
    data, existed = load(args.file)
    if not existed:
        emit(result="unchanged", file=args.file, note="no such file")
        return 0
    changed, restored_sl = remove_statusline_only(data)
    if not changed:
        emit(result="unchanged", file=args.file, note="no context-guru statusline installed here")
        return 0
    saved = backup(args.file)
    deleted = maybe_delete_if_empty(args.file, data)
    if not deleted:
        save(args.file, data)
        # Once context-guru's keys are gone, every backup here — including the one just taken —
        # answers a question nobody has anymore. See forget_backups()'s own docstring.
        forget_backups(args.file)
    saved = post_uninstall_backup_note(deleted)
    emit(result="removed", file=args.file, backup=saved, statusline_restored=restored_sl,
         file_deleted=str(deleted).lower())
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
        changed, restored_sl = remove_statusline_only(data)
        if changed:
            saved = backup(args.file)
            deleted = maybe_delete_if_empty(args.file, data)
            if not deleted:
                save(args.file, data)
                forget_backups(args.file)
            saved = post_uninstall_backup_note(deleted)
            emit(result="removed", file=args.file, backup=saved, statusline_restored=restored_sl,
                 file_deleted=str(deleted).lower(),
                 note="statusline-only removal; no routing was present to touch")
            return 0
        emit(result="unchanged", file=args.file, note=f"no env.{KEY} here")
        return 0
    current = env[KEY]
    # Ours is the URL passed in, or the one we recorded at install time — which covers the case
    # where the configured port changed since. It is NOT "any loopback /anthropic URL": another
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
        emit(result="conflict", reason="not_the_url_we_installed", file=args.file, existing=current,
             expected=args.url,
             note="this base URL is not the one context-guru installed; left untouched")
        return 2
    # ...and refuse to remove ANOTHER context-guru install's routing. The check above only asks
    # "did we write this", which the MACHINE-WIDE install's own file answers yes to as loudly as
    # the project's. `--url` does not narrow it either, by design: it WIDENS (see above — it is the
    # escape hatch for a record we never wrote, and it covers a port that changed since install),
    # so a project uninstall passing its own port still deletes the machine-wide `ANTHROPIC_BASE_URL`
    # and reports `removed` with `was=` naming a port it was never asked about. Measured, on the two
    # installs the README now documents: resetting ONE project unrouted EVERY project on the machine,
    # exit 0. The uninstall skill then unsets the machine-wide `options.port` too, since its gate
    # (correctly) trusts `removed`.
    #
    # The record is what distinguishes them: whatever file routes THIS project is this project's to
    # remove, and the machine-wide file when it is not that file belongs to an install this caller
    # was not asked to touch. `resolve_install_scope` answers which file that is; for a project with
    # no routing of its own it INFERS the machine-wide file, which is why the two reasons below split
    # on `other` rather than on the removal being allowed. Neither is allowed without the flag — see
    # the note on the exemption below, which this used to describe.
    #
    # Fail open on anything unexpected: an uninstall that cannot read the record must still be able
    # to uninstall. `--user-scope` is the deliberate override, the same flag `add` needs to write
    # this file in the first place.
    if is_user_scope(args.file) and not getattr(args, "user_scope", False):
        try:
            _own_scope, own_file, _own_src = resolve_install_scope()
        except Exception:
            own_file = None
        other = bool(own_file) and os.path.realpath(own_file) != os.path.realpath(args.file)
        # EVERY caller, not only one that routes itself. The first version of this guard exempted a
        # project with no routing of its own, on the reasoning that the machine-wide file is then
        # this project's only routing and refusing would leave it unable to uninstall at all. Both
        # halves were wrong. That describes every project except the ones that installed
        # themselves — including every project `adopt` folded in, and every project on a
        # machine-wide-only machine — so the exemption covered the common case rather than an edge
        # one, and there `/context-guru:uninstall` unrouted the whole machine at exit 0 with no
        # flag and no question. And nothing is unable to uninstall: `--user-scope` works from
        # anywhere, so the cost of the refusal is one confirmed re-run, not a lockout. (The path for
        # a user who cannot get a session to talk at all was never this command either — it is
        # `context-guru-reset`, which restores whole files and needs no Claude.)
        #
        # The record cannot tell "reset this project" from "remove context-guru" here: a folded-in
        # project and a machine-wide-only machine look identical in it, and only the user knows
        # which they meant. So this asks, and `--user-scope` is the answer — the same flag `add`
        # needs to write this file in the first place, so there is one name for "yes, the whole
        # machine". The two reasons differ only in how sharp the case is, and both refuse.
        emit(result="conflict",
             reason="another_installs_routing" if other else "machine_wide_routing",
             file=args.file, existing=current, this_project_routes_in=own_file or "(none)",
             note=("this is the MACHINE-WIDE install's own settings file and this project routes "
                   "through a file of its own; a project uninstall leaves it alone. "
                   if other else
                   "this file routes EVERY project on this machine, and removing it uninstalls "
                   "context-guru everywhere rather than resetting one project. ") +
                  "Doing it anyway is a separate, confirmed step: --user-scope")
        return 2
    backup(args.file)
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
    deleted = maybe_delete_if_empty(args.file, data)
    if not deleted:
        save(args.file, data)
        forget_backups(args.file)
    emit(result="removed", file=args.file, was=current, backup=post_uninstall_backup_note(deleted),
         restored=restored, env_block_left=str(bool(env)).lower(),
         statusline_restored=restored_sl, file_deleted=str(deleted).lower())
    return 0


def _option_file_candidates(plugin: str) -> list[str]:
    """The three files a plugin's configured options can live in, most specific first.

    Shared by `config` (read-only) and `preset` (which also writes), so the two commands can
    never disagree about where "the configured value" actually lives.
    """
    return [
        os.path.join(".claude", "settings.local.json"),
        os.path.join(".claude", "settings.json"),
        os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or
                     os.path.join(os.path.expanduser("~"), ".claude"), "settings.json"),
    ]


def cmd_config(args: argparse.Namespace) -> int:
    """Print the plugin's CONFIGURED option values, which the install cannot otherwise see.

    This exists because of a defect that made the `port` option impossible to honour. Claude Code does
    not put `CLAUDE_PLUGIN_OPTION_*` into the environment of a Bash tool call — only of a hook — so an
    install skill reading `${CLAUDE_PLUGIN_OPTION_PORT:-8787}` in a shell command always saw the
    default. The consequence was not a cosmetic default: the routing key named 8787 while every later
    hook read the CONFIGURED port and self-gated on it, so the hooks treated the project as unrouted,
    and the one running proxy had no auto-restart behind it. Once it idle-exited, nothing brought it
    back — silently, which is the exact failure this plugin exists to prevent.

    The values are on disk, so read them: Claude Code stores them under
    `pluginConfigs["<plugin>@<marketplace>"].options` in a settings file. Checked in precedence order,
    most specific first, because a project-scope install writes them into the project's file.
    """
    for path in _option_file_candidates(args.plugin):
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, json.JSONDecodeError):
            continue
        opts = (((data.get("pluginConfigs") or {}).get(args.plugin) or {}).get("options") or {})
        if isinstance(opts, dict) and opts:
            emit(result="ok", source=path, **{f"option_{k}": v for k, v in sorted(opts.items())})
            return 0
    emit(result="ok", source="(none)",
         note="no configured options found; the defaults in plugin.json apply")
    return 0


# ---------------------------------------------------------------------------
# Named cache strategies
# ---------------------------------------------------------------------------
#
# The proxy has three distinct cache mechanisms and, until this table existed, only one of them
# had a name a user could say back to us. `/context-guru:keepalive` armed the middle one by
# writing four tuning numbers into a YAML file from a heredoc embedded in a skill; switching back
# meant remembering what those numbers had been. A NAME is the whole point: "put it back on
# 5-min-ping" has to be a sentence, not an archaeology exercise.
#
# The names are deliberately the same names the hosted control plane uses for
# tenant.Strategy.Name, so a strategy means the same thing on a laptop and on a gateway
# deployment. If these two vocabularies drift, the naming has bought nothing.
#
# WHY THIS LIVES IN settings.py rather than in the skill that calls it: this file is the one place
# that already writes user-visible files carefully — atomically, with an ownership marker, and
# refusing to clobber anything it did not write. A heredoc in a skill body had to re-derive all of
# that in prose, and prose is what the surrounding design document is about.

# The marker's FIRST line carries the strategy name, so `strategy show` can report the name rather
# than reverse-engineering it from the parameters. Ownership is decided by the `# context-guru:`
# prefix plus `written by`, which keeps files written by the OLD keepalive skill recognisable as
# ours - they exist in the wild, and refusing to touch them would strand anyone who armed
# keep-alive before this change.
STRATEGY_MARKER_PREFIX = "# context-guru:"
STRATEGY_MARKER_TAIL = "written by"

DEFAULT_STRATEGY = "5-min-ping"

# The default PRESET, in the one place that decides it. It is `off` - the passthrough pipeline, no
# components at all - and that is a deliberate product claim rather than a conservative guess:
#
#   with the defaults, this plugin does not touch your context. Requests are forwarded
#   byte-for-byte, and the only thing that happens is that your prompt cache is kept warm.
#
# It used to be `cache`, i.e. cachesplit alone, on the strength of a -34.1% figure that came from
# SWE-bench CLI traffic and does not describe interactive use (#263). The measurements that do:
# with cachesplit OFF a real session's first request still read 45,805 tokens from cache and only
# 8,499 more moved when it was on (apply/prefixsplit_test.go), and its production credit over the
# measured window was $0.03 against keep-alive's +$125. So the component that earns the install is
# keep-alive, and the pipeline default should not be quietly editing anybody's requests to add 18%
# to what their client already caches for free.
#
# `cache` remains selectable. Only the default changed. Choosing a preset such as `housellm` IS
# opting into context editing, which is fine - it is a deliberate step rather than a default.
#
# FIVE files encoded this default before it moved here (plugin.json, install.sh, start-proxy.sh,
# check-proxy.sh, and the empty-preset note below). A drift test now fails if they disagree.
DEFAULT_PRESET = "off"

# `split` was the old name for `none`. It was named after `cachesplit`, which stopped being the
# default, so the name referred to a component that is not running - the defect shape #263 is about.
# Renamed outright rather than aliased: the plugin is unreleased, so there is no install in the wild
# to keep working, and an alias would be a second name for one thing living in the code forever to
# serve nobody. A file can never carry `strategy=split` either, because `split` WAS the absence of a
# file - so there is no stored state to migrate.
# Each entry: the `cache:` keys it sets, and one line of honest description.
#
# `none` writes NO FILE AT ALL, deliberately. --config REPLACES --preset rather than layering over
# it, so the way to express "just the preset, nothing else" is the absence of a config, not a
# config that says keepalive: false. That also means switching to `none` is a removal, which is
# why it cannot be expressed as a dict here.
STRATEGIES: dict[str, dict] = {
    "none": {
        "cache": {},
        "spends": False,
        "desc": "no cache strategy: the preset runs and nothing else. No pings, no model calls, "
                "no spend. Written as the ABSENCE of a config file.",
    },
    DEFAULT_STRATEGY: {
        # Explicit rather than relying on the library defaults, for two reasons: the file becomes
        # self-documenting for anyone who opens it, and a later change to a default in config.go
        # cannot silently re-tune an already-armed install.
        "cache": {
            "keepalive": True,
            "keepalive_idle_seconds": 280,
            "keepalive_max_pings": 2,
            "keepalive_max_usd_per_ping": 0.25,
            "keepalive_min_prefix_tokens": 20000,
        },
        "spends": True,
        "desc": "an idle ping just under the provider's 5-minute TTL (280s, at most 2 per "
                "idle span, >=20k-token prefix, <=$0.25/ping). SPENDS THE CALLER'S CREDENTIAL "
                "while nobody is at the keyboard.",
    },
    "1-hour-head": {
        "cache": {"head_ttl_1h": True, "head_ttl_min_tokens": 50000},
        "spends": False,
        # The >=50k gate is disclosed HERE, in plugin.json and in the picker's table, because it is
        # the difference between this strategy doing something and doing nothing - and because both
        # measurements quoted as its evidence are BELOW it (36,574 on Haiku, 48,212 on Sonnet). A
        # strategy that advertises a measurement has to state the threshold that measurement would
        # not have passed, or "verify with Usage.CacheWrite1h" reads zero for an undisclosed second
        # reason and the user concludes the tier was refused.
        "desc": "asks for the 1-hour tier on the tools/system breakpoints, and ONLY on "
                "requests with a >=50k-token prefix (below that it does nothing at all; the "
                "size gate is what makes it pay, +$48.81 against -$18.34 applied blanket). No "
                "pings. Measured GRANTED on Haiku 4.5 and SILENTLY DOWNGRADED on Sonnet 5 (zero "
                "1h writes in 19,805 production requests), so on Opus/Sonnet the honest projection "
                "is $0 - verify with Usage.CacheWrite1h before believing otherwise, and note both "
                "of those measurements were on prefixes UNDER the 50k gate this ships with.",
    },
}


def strategy_path(port: str) -> str:
    """The config file start-proxy.sh looks for, named after the PORT.

    Named after the port because the file has to be findable by a proxy that was started for a
    particular port, and because two proxies on one machine must not share one strategy.
    """
    return os.path.join(state_dir(), f"keepalive-{port}.yaml")


def _strategy_is_ours(text: str) -> bool:
    first = text.splitlines()[0] if text.splitlines() else ""
    return first.startswith(STRATEGY_MARKER_PREFIX) and STRATEGY_MARKER_TAIL in first


def _strategy_name_in(text: str) -> str:
    """The name recorded in the marker, or "" for a file that predates named strategies."""
    first = text.splitlines()[0] if text.splitlines() else ""
    m = re.search(r"strategy=([A-Za-z0-9._-]+)", first)
    return m.group(1) if m else ""


def _render_strategy(name: str, preset: str) -> str:
    spec = STRATEGIES[name]
    lines = [
        f"{STRATEGY_MARKER_PREFIX} strategy={name} {STRATEGY_MARKER_TAIL} "
        f"/context-guru:cache-strategy-picker",
        "# Do not hand-edit: the picker refuses to touch a file whose first line is not this one,",
        "# so an edit that removes the marker also removes your ability to switch back with a name.",
        "#",
        "# `preset:` is stated explicitly because --config REPLACES --preset entirely rather than",
        "# layering over it. A config that omitted it would silently turn compaction off at the",
        "# moment a cache strategy was armed - the exact opposite of the intent.",
        "#",
        "# It is NOT the source of truth for the preset. `strategy sync` re-renders this line from",
        "# the plugin option every time a proxy starts, because the file owning it meant a user who",
        "# changed the option saw it stick in the config UI and kept running the preset that was",
        "# recorded here when the strategy was first armed - indefinitely, and with no way to tell.",
        f"preset: {preset}",
    ]
    cache = spec["cache"]
    if cache:
        lines.append("cache:")
        for k, v in cache.items():
            lines.append(f"  {k}: {json.dumps(v)}")
    return "\n".join(lines) + "\n"


def valid_base_url(url: str) -> str:
    """Is this a base URL we are willing to WRITE into somebody's routing key? Returns "" if so,
    else a short reason.

    This is the guard that did not exist, and its absence had a straightforward consequence: `add`
    accepted any string at all — verified, `127.0.0.1/anthropic` with no scheme and no port — wrote
    it into `env.ANTHROPIC_BASE_URL`, and reported `result=added` with exit 0. The project is then
    routed to something unroutable, which is the hang state this whole design exists to avoid, and
    the report says it worked.

    Three things it deliberately is NOT:

    * NOT `is_ours()`. That answers "did we write this?" from the recorded
      `$context-guru.installed_base_url`, never from the URL's shape — because another local proxy's is
      `http://127.0.0.1:4000/anthropic` and two local proxies are indistinguishable by URL.
      Provenance and validity are different questions.
    * NOT `_is_loopback()`, which is deliberately generous: every shape it missed was a false
      NEGATIVE, and a false negative there re-introduces a different defect.
    * NOT applied to `--upstream`. That is somebody else's gateway and its shape is theirs — the
      tests alone carry `http://gw.example:4000/v1?tenant=acme&mode=chain`. We validate the URL
      whose shape WE control.

    And it is applied on `add` only, never on `remove`: removal is the recovery path, so it has to
    be able to clean up a malformed value that is already in the file. Strict on write, permissive
    on removal.
    """
    if not isinstance(url, str) or not url.strip():
        return "empty"
    if "://" not in url:
        return "no_scheme"
    scheme, _, rest = url.partition("://")
    if scheme.lower() not in ("http", "https"):
        return "bad_scheme"
    if not rest or rest.startswith("/"):
        return "no_host"
    hostport, _, path = rest.partition("/")
    # A bracketed v6 literal keeps its brackets; the port is whatever follows the closing one.
    if hostport.startswith("["):
        host, _, after = hostport.partition("]")
        host += "]"
        port = after[1:] if after.startswith(":") else ""
    else:
        host, _, port = hostport.partition(":")
    if not host:
        return "no_host"
    # A host must look like a host. This was only checked for emptiness, and the consequence was not
    # cosmetic: `http://x;touch /tmp/PWNED;cd /anthropic` was approved as `result=ok`, install.sh
    # interpolated that approved string unquoted into the `confirm_command=` line that SKILL.md tells
    # a model to run verbatim and that the suite runs through `bash -c`, and the injected command
    # executed. Worse, it executed regardless of the consent answer, because it rides the string the
    # *plan* prints — upstream of the gate in route_main.
    #
    # install.sh now also shell-quotes every value it interpolates, which closes the same hole from
    # the other side. Both are deliberate: this one refuses a URL we would never want to write, and
    # the quoting stops the next field added to that line from reopening the vector.
    #
    # Underscore is permitted though it is not strictly legal in a hostname, because internal
    # gateways use it and a false refusal here blocks an install; it is shell-inert either way.
    if host.startswith("["):
        if not re.fullmatch(r"\[[0-9A-Fa-f:.]+\]", host):
            return "bad_host"
    elif not re.fullmatch(r"[A-Za-z0-9_]([A-Za-z0-9._-]*[A-Za-z0-9_])?", host):
        return "bad_host"
    if port and not port.isdigit():
        return "bad_port"
    if port and not (0 < int(port) < 65536):
        return "port_out_of_range"
    # The path the proxy serves the Anthropic dialect on. Writing a base URL without it routes the
    # session to a 404 on every call, which looks exactly like a broken proxy.
    if path.rstrip("/").rsplit("/", 1)[-1] != "anthropic":
        return "path_not_anthropic"
    # An explicit port is required for loopback and NOT otherwise: our own proxy is always on a
    # chosen port, while a gateway on `https://gw.internal/anthropic` is legitimately at 443.
    # `http://127.0.0.1/anthropic` is the malformed shape actually observed in the wild.
    if _is_loopback(url) and not port:
        return "loopback_without_port"
    return ""


def cmd_check_url(args: argparse.Namespace) -> int:
    """Validate a base URL and write nothing. Exit 0 if usable, 2 with a reason if not."""
    why = valid_base_url(args.url)
    if why:
        emit(result="error", reason="invalid_base_url", detail=why, url=args.url)
        return 2
    emit(result="ok", url=args.url)
    return 0


def cmd_strategy(args) -> int:
    if args.op == "list":
        # `names=` is the machine-readable list. The per-strategy keys below mangle `-` to `_` to be
        # valid fact keys, so they cannot be parsed back into names — install.sh needs to validate a
        # `--cache-strategy` value BEFORE it downloads a binary, and this is what it reads.
        emit(result="ok", default=DEFAULT_STRATEGY, names=",".join(STRATEGIES))
        for name, spec in STRATEGIES.items():
            emit(**{f"strategy_{name.replace('-', '_')}": spec["desc"],
                    f"spends_{name.replace('-', '_')}": "true" if spec["spends"] else "false"})
        return 0

    port = args.port
    path = strategy_path(port)

    if args.op == "show":
        if not os.path.exists(path):
            # No file is not "unknown": it is exactly what `none` means.
            emit(result="ok", strategy="none", file="(none)", port=port,
                 note="no config for this port, so the preset runs alone and nothing is spent")
            return 0
        try:
            text = open(path, encoding="utf-8").read()
        except OSError as exc:
            emit(result="error", reason="unreadable", file=path, detail=f"{exc}")
            return 3
        if not _strategy_is_ours(text):
            emit(result="ok", strategy="(foreign)", file=path, port=port,
                 note="a config exists at our path that we did not write; left alone")
            return 0
        name = _strategy_name_in(text)
        # A name we do not recognise is reported AS RECORDED, with a note saying so. Two wrong
        # answers were available here and both were worse: mapping it to a current name would claim
        # the file does something it does not, and reporting "(unnamed)" would hide that the file
        # names a strategy at all. `split` is the realistic case - it was removed rather than
        # aliased, so a hand-written file can still carry it - and the file EXISTS and arms pings,
        # which is exactly what the reader needs to know.
        if name and name not in STRATEGIES:
            emit(result="ok", strategy=name, file=path, port=port,
                 note=f"this config names `{name}`, which is not a strategy this version knows "
                      f"({','.join(STRATEGIES)}). The file is still what the proxy loads; re-set it "
                      f"with `strategy set --name <known>` to bring it back under a name")
            return 0
        emit(result="ok", strategy=name or "(unnamed)", file=path, port=port,
             note="" if name else "written before strategies had names; re-set it to name it")
        return 0

    if args.op == "sync":
        # Called by start-proxy.sh immediately before it launches a proxy, and this is what makes a
        # `/plugin configure` preset change take effect on the next session.
        #
        # The problem it solves: --config REPLACES --preset, so once a strategy was armed the preset
        # recorded in its file was in force forever. A user set `housellm`, saw it stick in the config
        # UI, and kept running whatever was recorded when they first armed keep-alive. There is no way
        # to interrogate a running proxy about it either - /healthz answers the literal string "ok" -
        # so nothing anywhere could report the divergence.
        #
        # Re-renders from the NAME in the file plus the preset the caller passes, rather than editing
        # the preset line in place: the name is the durable choice, the preset is the option, and
        # rendering the pair through the one function that writes these files keeps a synced file
        # byte-identical to a freshly-set one. Anything else is a second encoding of the same rule.
        if not args.preset.strip():
            emit(result="error", reason="empty_preset",
                 note="sync needs the preset to write; nothing was changed")
            return 2
        if not os.path.exists(path):
            # `none` is the absence of a file, so there is nothing to sync and that is not a failure:
            # the preset reaches the proxy directly as --preset.
            emit(result="unchanged", strategy="none", file="(none)", port=port,
                 note="no config for this port, so --preset reaches the proxy unmediated")
            return 0
        try:
            text = open(path, encoding="utf-8").read()
        except OSError as exc:
            # Fails OPEN and reports it. This runs on the SessionStart path: a file we cannot read is
            # a reason to leave the proxy's own handling alone, never a reason to refuse to start.
            emit(result="skipped", reason="unreadable", file=path, detail=f"{exc}",
                 note="left alone; the proxy will load it as it stands")
            return 0
        if not _strategy_is_ours(text):
            emit(result="skipped", reason="not_ours", file=path,
                 note="a config we did not write is at this path; its preset is its owner's business")
            return 0
        name = _strategy_name_in(text)
        if not name:
            emit(result="skipped", reason="unnamed", file=path,
                 note="written before strategies had names, so there is no name to re-render from; "
                      "`strategy set --name <name>` names it")
            return 0
        if name not in STRATEGIES:
            # A file naming a strategy this version does not know. Re-rendering it would silently
            # change what it does; reporting it is the honest move.
            emit(result="skipped", reason="unknown_strategy", file=path, requested=name,
                 known=",".join(STRATEGIES))
            return 0
        if name == "none":
            # A file that names the absence of a file. Contradictory, so do not act on it.
            emit(result="skipped", reason="none_has_no_file", file=path,
                 note="this config names `none`, which is expressed as no config at all; "
                      "`strategy set --name none` removes it")
            return 0
        want = _render_strategy(name, args.preset)
        if want == text:
            emit(result="unchanged", strategy=name, file=path, port=port, preset=args.preset)
            return 0
        _write_atomic(path, want.encode("utf-8"), mode=0o600)
        emit(result="synced", strategy=name, file=path, port=port, preset=args.preset,
             note="the config's preset now matches the plugin option")
        return 0

    if args.op == "clear":
        # `set --name split` reaches here by recursion, because split IS the absence of a config. The
        # caller still asked to SET something, and answering a `set` with `cleared` made success have
        # three words - install.sh's `set|cleared|unchanged` case was the tell, and every future caller
        # would have inherited the obligation to know that. So the OP the caller invoked decides the
        # word, and the mechanism stays a removal.
        as_set = getattr(args, "as_set", False)
        done, nothing_to_do = ("set", "set") if as_set else ("cleared", "unchanged")
        # `file=` would name a path that deliberately does not exist, which is the sort of confidently
        # wrong detail this script is careful about elsewhere.
        where = "(none)" if as_set else path
        if not os.path.exists(path):
            emit(result=nothing_to_do, strategy="none", file=where, port=port,
                 note="nothing to remove")
            return 0
        # An unreadable file is NOT a file we may delete. This swallowed the OSError, left `text`
        # empty, and `text and not _strategy_is_ours(text)` is then false - so the ownership check was
        # skipped entirely and execution fell through to os.unlink(), reporting `result=cleared` for a
        # file we never verified we wrote. A permission-restricted foreign config is exactly what that
        # produces, and exactly the case the check exists for. `set` already handles the same
        # situation as `unreadable`; this now matches it rather than contradicting the invariant this
        # module advertises and tests for.
        try:
            text = open(path, encoding="utf-8").read()
        except OSError as exc:
            emit(result="error", reason="unreadable", file=path, detail=f"{exc}",
                 note="a config exists at this path and cannot be read, so ownership cannot be "
                      "checked; refusing to delete it. Remove it by hand if it is yours.")
            return 3
        if text and not _strategy_is_ours(text):
            emit(result="conflict", reason="not_ours", file=path,
                 note="this config was not written by context-guru; remove it by hand")
            return 2
        os.unlink(path)
        emit(result=done, strategy="none", file=where, port=port,
             note="takes effect the next time the proxy starts, not now")
        return 0

    # op == "set"
    name = args.name
    if name not in STRATEGIES:
        emit(result="error", reason="unknown_strategy", requested=name,
             known=",".join(STRATEGIES))
        return 2
    # `none` writes NO FILE, so it reaches `clear` and never touches the preset - checking the preset
    # first refused `strategy set --name split` (with no --preset, which the parser does not require)
    # as `empty_preset`, whose note about silently disabling compaction describes a file that would
    # never be written. It also regressed the old keep-alive-off path, which was `rm -f "$CFG"` and
    # required no preset at all. Ordered before the guard for that reason.
    if name == "none":
        # Expressed as a removal, per the STRATEGIES comment - but reported as a `set`, because that is
        # the operation the caller asked for. See the clear branch.
        return cmd_strategy(argparse.Namespace(op="clear", port=port, as_set=True))

    # An unresolved preset is NOT a harmless default: written empty, the proxy loads a config with
    # no pipeline and reports success, so compaction is off while a strategy keeps running. The old
    # keepalive skill guarded this in shell; it is enforced here so every caller inherits it.
    if not args.preset.strip():
        emit(result="error", reason="empty_preset",
             note=f"pass --preset (option_preset= from `settings.py config`, else the plugin.json "
                  f"default `{DEFAULT_PRESET}`); an empty preset is not the same thing as `off` - "
                  f"it loads a config with no pipeline AND no name for what it is doing")
        return 2

    if os.path.exists(path):
        try:
            existing = open(path, encoding="utf-8").read()
        except OSError as exc:
            emit(result="error", reason="unreadable", file=path, detail=f"{exc}")
            return 3
        if not _strategy_is_ours(existing):
            emit(result="conflict", reason="not_ours", file=path,
                 note="a config we did not write is already at this path; edit or remove it by hand")
            return 2

    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    _write_atomic(path, _render_strategy(name, args.preset).encode("utf-8"), mode=0o600)
    emit(result="set", strategy=name, file=path, port=port, preset=args.preset,
         spends="true" if STRATEGIES[name]["spends"] else "false",
         note="takes effect the next time the proxy starts; a proxy already running keeps its "
              "current config until it is stopped and started again")
    return 0


# ---------------------------------------------------------------------------
# Preset — the plugin option, not a per-port file
# ---------------------------------------------------------------------------
#
# `/plugin configure` can set this too, but it is a generic options form with no per-option
# guidance: five fields, no validation, no "what does house actually do" a person can read before
# picking. `preset set --name <name>` is the same write — pluginConfigs[<plugin>].options.preset —
# made nameable and explainable, the same reason `strategy` exists for cache_strategy above.
PRESETS: dict[str, str] = {
    "off": "nothing runs; requests are forwarded exactly as they arrived",
    "cache": "keeps the prompt cache warm by splitting off the volatile part of the system "
             "prompt; drops nothing",
    "house": "trims obvious waste from tool output (repeats, dead runs) — the safe first step "
             "into carrying less",
    "codesmart": "house's trimming plus a cheap model that keeps only what looks relevant",
    "housellm": "the deepest cut: a compaction model periodically rewrites the whole "
                "conversation — the only preset that spends on its own",
}
DEFAULT_PRESET = "off"


def cmd_preset(args: argparse.Namespace) -> int:
    if args.op == "list":
        emit(result="ok", default=DEFAULT_PRESET, names=",".join(PRESETS))
        for name, desc in PRESETS.items():
            emit(**{f"preset_{name}": desc})
        return 0

    if args.op == "show":
        for path in _option_file_candidates(args.plugin):
            if not os.path.exists(path):
                continue
            data, _ = load(path)
            opts = (((data.get("pluginConfigs") or {}).get(args.plugin) or {}).get("options") or {})
            if isinstance(opts, dict) and "preset" in opts:
                emit(result="ok", preset=opts["preset"], file=path)
                return 0
        emit(result="ok", preset=DEFAULT_PRESET, file="(none)",
             note="no preset configured; the plugin default applies")
        return 0

    # op == "set"
    name = args.name
    if name not in PRESETS:
        emit(result="error", reason="unknown_preset", requested=name,
             known=",".join(PRESETS))
        return 2

    target = None
    for path in _option_file_candidates(args.plugin):
        if not os.path.exists(path):
            continue
        data, _ = load(path)
        opts = (((data.get("pluginConfigs") or {}).get(args.plugin) or {}).get("options") or {})
        if isinstance(opts, dict) and opts:
            target = path
            break

    if target is None and args.user_scope:
        # Explicit override: write the machine-wide file regardless of what routing chose,
        # for someone who deliberately wants this preset on every project.
        target = os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or
                               os.path.join(os.path.expanduser("~"), ".claude"), "settings.json")
    if target is None:
        # Nobody has configured this plugin's options anywhere yet, and no override was given —
        # inherit whatever scope THIS project's routing already used, rather than guessing
        # project-local independently of that choice (see resolve_install_scope). Only a project
        # that was never routed at all (routing_scope is None) falls back to project-local, since
        # there is nothing yet to inherit.
        _scope, resolved_file, _source = resolve_install_scope()
        target = resolved_file or os.path.join(".claude", "settings.local.json")

    data, existed = load(target)
    plugins = data.setdefault("pluginConfigs", {})
    if not isinstance(plugins, dict):
        emit(result="error", reason="pluginConfigs_not_an_object", file=target)
        return 3
    entry = plugins.setdefault(args.plugin, {})
    if not isinstance(entry, dict):
        emit(result="error", reason="plugin_entry_not_an_object", file=target)
        return 3
    options = entry.setdefault("options", {})
    if not isinstance(options, dict):
        emit(result="error", reason="options_not_an_object", file=target)
        return 3

    if options.get("preset") == name:
        emit(result="unchanged", preset=name, file=target)
        return 0

    if existed:
        backup(target)
    options["preset"] = name
    save(target, data)
    emit(result="set", preset=name, file=target,
         note="takes effect the next time the proxy starts, not this session — stop and restart "
              "it, or start a new session, to pick it up")
    return 0


# ---------------------------------------------------------------------------
# The proxy BINARY's own release channel (v0.1.1 … v0.3.0 on GitHub) is separate from this
# PLUGIN's version in plugin.json, and updating the plugin through the marketplace does not touch
# the binary. This is the small state file that lets start-proxy.sh tell the user, at most once
# per released tag, that a newer binary exists — and remember what they said so it never nags
# again about a tag it already asked about.
#
# Modelled on the strategy file above: a marker on the first line, an ownership guard that
# refuses to touch a file it did not write, and one render function that is the only composer.
UPDATE_MARKER_PREFIX = "# context-guru:"
UPDATE_MARKER_TAIL = "written by"

# `ask` (default): show the notice once per released tag not yet in `skipped`. `auto`: the
# SessionStart hook upgrades in the background without asking again ("always"). `never`: the
# check is off. `skipped` is independent of `answer` — it mutes ONE tag, not every future one, so
# declining v0.3.0 does not hide a v0.4.0 that ships a real fix.
UPDATE_ANSWERS = ("always", "skip", "never")


def update_check_path() -> str:
    """One file, machine-wide: there is one proxy binary on PATH, not one per port."""
    return os.path.join(state_dir(), "update-check.yaml")


def _update_is_ours(text: str) -> bool:
    first = text.splitlines()[0] if text.splitlines() else ""
    return first.startswith(UPDATE_MARKER_PREFIX) and UPDATE_MARKER_TAIL in first


def _update_fields_in(text: str) -> dict[str, str]:
    fields = {"answer": "ask", "skipped": "", "notified": "", "latest": ""}
    for line in text.splitlines():
        m = re.match(r"^(answer|skipped|notified|latest):\s*(.*)$", line)
        if m:
            fields[m.group(1)] = m.group(2).strip()
    return fields


def _render_update_check(answer: str, skipped: str, notified: str, latest: str) -> str:
    return "\n".join([
        f"{UPDATE_MARKER_PREFIX} update_check=1 {UPDATE_MARKER_TAIL} /context-guru:update",
        "# Do not hand-edit: this file's ownership is decided by its first line, so an edit that",
        "# removes the marker also turns the update notice off for good.",
        f"answer: {answer or 'ask'}",
        # `skipped` is a REAL decision: the user explicitly said no to this tag, via
        # `update-check answer --answer skip`. `notified` is bookkeeping only: the notice was
        # PRINTED for this tag, so it does not repeat — it carries no claim about what the user
        # did with it. The two used to be the same field, written together at print time, which
        # meant a session start hook the user never saw (its output competing against an
        # unrelated first message) recorded a permanent "declined" the user never actually said.
        f"skipped: {skipped}",
        f"notified: {notified}",
        f"latest: {latest}",
    ]) + "\n"


def _read_update_check() -> tuple[dict[str, str] | None, str]:
    """(fields, "") normally — absent is a real, synthetic "ask" state, not an error. (None,
    reason) when the file exists but must not be touched: unreadable, or written by something
    else. Every caller must fail OPEN on the second case, since this runs on the SessionStart path.
    """
    path = update_check_path()
    if not os.path.exists(path):
        return {"answer": "ask", "skipped": "", "notified": "", "latest": ""}, ""
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        return None, "unreadable"
    if not _update_is_ours(text):
        return None, "not_ours"
    return _update_fields_in(text), ""


def _write_update_check(fields: dict[str, str]) -> str:
    """Returns "" on success, else a reason. Never raises — a state write must never fail a
    session that only needs a proxy, not a notice."""
    try:
        ensure_state_dir(state_dir())
        _write_atomic(update_check_path(), _render_update_check(**fields).encode("utf-8"),
                       mode=0o600)
    except OSError as exc:
        return f"unwritable:{exc}"
    return ""


def installed_proxy_version() -> str:
    """The version install.sh last confirmed on disk, or "" if it never has.

    Read from a plain file, not from running the binary — the same reason start-proxy.sh reads
    it this way (see install.sh's record_installed_version): a caller that is not the hook is
    still better off not guessing at what invoking an arbitrary configured binary might do.
    """
    try:
        with open(os.path.join(state_dir(), "proxy-version"), encoding="utf-8") as fh:
            return fh.readline().strip()
    except OSError:
        return ""


def cmd_update_check(args) -> int:
    if args.op == "show":
        fields, reason = _read_update_check()
        if fields is None:
            emit(result="skipped", reason=reason)
            return 0
        # `installed=` is its OWN fact, not something a reader should infer from `skipped=` (the
        # tag a past notice was DECLINED for) or `latest=` (what the last check found). A prior
        # version of this surface omitted it entirely, and the gap was filled by a model
        # conflating "skipped" with "installed" and reporting a version that was simply wrong.
        emit(result="ok", answer=fields["answer"], skipped=fields["skipped"],
             notified=fields["notified"], latest=fields["latest"],
             installed=installed_proxy_version() or "unknown")
        return 0

    if args.op == "stamp":
        # Called by the release check after it resolves the latest tag (or fails to). The file's
        # mtime IS the 5-minute throttle: start-proxy.sh gates the next check on this file's age
        # with `find … -mmin -5` before it forks python at all, so no date arithmetic exists in
        # shell and a clock moving backwards only ever makes it check MORE, never less.
        fields, reason = _read_update_check()
        if fields is None:
            emit(result="skipped", reason=reason)
            return 0
        fields["latest"] = args.latest
        write_reason = _write_update_check(fields)
        path = update_check_path()
        try:
            os.utime(path, None)
        except OSError:
            pass
        if write_reason:
            emit(result="skipped", reason=write_reason)
            return 0
        emit(result="ok", latest=args.latest)
        return 0

    if args.op == "notify":
        # Called the moment the hook PRINTS the notice for a tag, so it does not repeat every
        # session. Deliberately separate from `answer --answer skip`: printing a note into a
        # SessionStart hook's output is not the same event as the user actually declining it, and
        # a real user hit exactly this gap — their first message that session was unrelated, the
        # note was never relayed, and the record still ended up saying "skipped" for an offer they
        # never saw. `notified` carries no claim about what the user did; it only suppresses a
        # repeat of the same tag's notice. `skipped` stays reserved for `answer --answer skip`.
        if not args.version:
            emit(result="error", reason="missing_value", note="notify needs --version")
            return 2
        fields, reason = _read_update_check()
        if fields is None:
            emit(result="skipped", reason=reason)
            return 0
        fields["notified"] = args.version
        write_reason = _write_update_check(fields)
        if write_reason:
            emit(result="skipped", reason=write_reason)
            return 0
        emit(result="recorded", notified=fields["notified"])
        return 0

    # op == "answer"
    if args.answer not in UPDATE_ANSWERS:
        emit(result="error", reason="unknown_answer", known=",".join(UPDATE_ANSWERS))
        return 2
    if args.answer == "skip" and not args.version:
        emit(result="error", reason="missing_value", note="skip needs --version: the tag it mutes")
        return 2
    fields, reason = _read_update_check()
    if fields is None:
        emit(result="skipped", reason=reason)
        return 0
    if args.answer == "always":
        fields["answer"] = "auto"
    elif args.answer == "never":
        fields["answer"] = "never"
    else:
        fields["skipped"] = args.version
    write_reason = _write_update_check(fields)
    if write_reason:
        emit(result="skipped", reason=write_reason)
        return 0
    emit(result="recorded", answer=fields["answer"], skipped=fields["skipped"])
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
        p.add_argument("--user-scope", action="store_true",
                       help="permit writing the machine-wide settings file "
                            "(~/.claude/settings.json), which routes EVERY project. Refused "
                            "without this on `add`. On `remove` it is needed only from inside a "
                            "project that routes through a file of its OWN: taking the "
                            "machine-wide routing down from there unroutes every other project "
                            "too, so it has to be asked for.")
        p.add_argument("--statusline", default="",
                       help="on add: also write the TOP-LEVEL statusLine key ({\"type\": "
                            "\"command\", \"command\": <this value>}), refusing to replace one "
                            "that is not ours unless --force. on remove: taken back only if it "
                            "is exactly what a previous --statusline install recorded writing.")
        p.add_argument("--no-backup", action="store_true",
                       help="on a statusline-only add: skip taking a timestamped backup. For a "
                            "follow-up call that runs seconds after another `add` already backed "
                            "up this file in the same install — never for a standalone change.")
    off = sub.add_parser("off",
        help="turn the status line off without touching routing — the counterpart to `add "
             "--statusline`, for when both were installed together and only the statusline "
             "should come back off")
    off.add_argument("--file", required=True)

    cfg = sub.add_parser("config")
    cfg.add_argument("--plugin", default="context-guru@context-guru")

    # resolve-scope takes no arguments: it always resolves for the CURRENT directory, the same
    # cwd every routing `add` call already keys its own record on (see record_install_scope).
    sub.add_parser("resolve-scope")

    # project-key exists for start-proxy.sh, which has to write the OWNER of a proxy it starts and
    # must not re-implement project_key()'s git-worktree rule in bash — two encodings of one
    # identity rule is how a proxy ends up owned by a key nothing else ever looks up.
    pk = sub.add_parser("project-key")
    pk.add_argument("--user-scope", action="store_true",
                    help="print the key a MACHINE-WIDE install files itself under, instead of this "
                         "project's. For install.sh and the uninstall skill, which must not "
                         "re-derive it.")

    # scopes exists for install.sh's --scope user gate; read-only, no arguments — see cmd_scopes.
    sub.add_parser("scopes")

    # `strategy` is the named-cache-strategy surface: the one place that decides what a name means,
    # so the skills that use it carry a NAME rather than four tuning numbers in a heredoc.
    # check-url exists so a caller can validate a supplied base URL BEFORE acting on it. Without
    # it the only enforcement point was `add`, i.e. after a proxy had been started and a health
    # check spent — a typo cost real work and surfaced as `settings_write_failed`, which names the
    # wrong step.
    cu = sub.add_parser("check-url")
    cu.add_argument("--url", required=True)

    gi = sub.add_parser("gitignore-ensure",
        help="make sure the recovery folder beside --file is git-ignored, deterministically, "
             "with no question asked (see the module comment above cmd_gitignore_ensure)")
    gi.add_argument("--file", required=True)

    st = sub.add_parser("strategy")
    st.add_argument("op", choices=("list", "show", "set", "clear", "sync"))
    st.add_argument("--name", default="",
                    help="strategy name for `set`; one of " + ", ".join(STRATEGIES))
    st.add_argument("--port", default="",
                    help="the CONFIGURED port. Required for show/set/clear: the config file is "
                         "named after it, so a defaulted port writes a file nothing reads.")
    st.add_argument("--preset", default="",
                    help="the preset to state in the file. Required for `set` and `sync`, because "
                         "--config REPLACES --preset rather than layering over it.")

    pr = sub.add_parser("preset")
    pr.add_argument("op", choices=("list", "show", "set"))
    pr.add_argument("--plugin", default="context-guru@context-guru")
    pr.add_argument("--name", default="",
                    help="preset name for `set`; one of " + ", ".join(PRESETS))
    pr.add_argument("--user-scope", action="store_true",
                    help="only used when no options file exists yet: write the machine-wide "
                         "settings file (~/.claude/settings.json) instead of the project-local "
                         "one. Ignored if the plugin's options already live somewhere — that "
                         "file is updated in place regardless of this flag.")

    # Per-project port allocation. `--file` lets a caller that already knows the scope (install.sh,
    # which derives it from --scope) name the settings file directly, the same way `add` does,
    # rather than making `alloc` re-resolve it; omitted, it falls back to `resolve_install_scope()`
    # exactly as `preset set` does when nothing is configured or routed yet.
    pt = sub.add_parser("port")
    pt.add_argument("op", choices=("alloc", "show", "release", "unset"))
    pt.add_argument("--plugin", default="context-guru@context-guru")
    pt.add_argument("--file", default="",
                    help="for `alloc`: the settings file to write pluginConfigs.options.port "
                         "into, or for `unset`: the settings file to remove it FROM, where "
                         "it is required. For `show`: the file whose existing option value to "
                         "report as option_port=, for a caller that has to know what was there "
                         "BEFORE it wrote. Optional for `alloc`; defaults to "
                         "resolve_install_scope()'s answer.")
    pt.add_argument("--dry-run", action="store_true",
                    help="for `alloc`: report the port that would be used without recording or "
                         "writing anything. For install.sh's --plan, which must write nothing.")
    pt.add_argument("--promised-port", type=int, default=None,
                    help="for the REAL `alloc` that follows a `--dry-run`: the port that dry run "
                         "reported (and the user consented to, in whatever URL a plan showed). "
                         "If the real allocation would now pick a DIFFERENT port, refuse instead "
                         "of silently writing a URL nobody agreed to.")
    pt.add_argument("--key", default="",
                    help="the install-scope record to act on, when it is not this project's. For "
                         "`release`: the project to release, for a caller acting on a project it "
                         "is not running IN (install.sh's `adopt`) - a directory is resolved "
                         "through project_key(), anything else is taken as a key already. For "
                         "`alloc` and `show`: the record to allocate/report, taken literally - "
                         "how a machine-wide install reaches its own record "
                         "(`project-key --user-scope`) rather than the cwd project's. Omitted, "
                         "this project's record is used.")

    # Who owns the proxy on this port — see owner_token(). Its own command rather than a field of
    # `port show`, because start-proxy.sh asks it on every session start, before it has decided
    # whether it is allowed to touch anything at all.
    ot = sub.add_parser("owner-token")
    ot.add_argument("--observed", default="",
                    help="the contents of proxy-<port>.owner as read from disk. Given, the answer "
                         "carries verdict=ours|theirs; omitted, it is just this project's token, "
                         "for writing that file.")

    # The proxy-binary release-notice surface: separate from `strategy`/`preset` because it is
    # machine-wide (one binary on PATH) rather than per-port or per-project.
    uc = sub.add_parser("update-check")
    uc.add_argument("op", choices=("show", "stamp", "notify", "answer"))
    uc.add_argument("--latest", default="", help="tag the redirect resolved; for `stamp`")
    uc.add_argument("--answer", default="",
                    help="for `answer`: one of " + ", ".join(UPDATE_ANSWERS))
    uc.add_argument("--version", default="",
                    help="the tag `notify` shows or `answer --answer skip` mutes")

    args = ap.parse_args()
    if args.cmd == "add" and not args.url and not args.statusline:
        ap.error("add needs --url, or --statusline on its own for a statusline-only call")
    if args.cmd == "strategy":
        if args.op != "list" and not args.port:
            ap.error("strategy " + args.op + " needs --port: the config file is named after the "
                     "port, so a defaulted one writes a file nothing ever reads")
        if args.op == "set" and not args.name:
            ap.error("strategy set needs --name; one of " + ", ".join(STRATEGIES))
    if args.cmd == "preset" and args.op == "set" and not args.name:
        ap.error("preset set needs --name; one of " + ", ".join(PRESETS))
    rc = {"add": cmd_add, "remove": cmd_remove, "off": cmd_off, "show": cmd_show,
          "config": cmd_config, "resolve-scope": cmd_resolve_scope,
          "project-key": cmd_project_key, "scopes": cmd_scopes,
          "strategy": cmd_strategy, "preset": cmd_preset, "port": cmd_port,
          "owner-token": cmd_owner_token,
          "check-url": cmd_check_url, "update-check": cmd_update_check,
          "gitignore-ensure": cmd_gitignore_ensure}[args.cmd](args)

    # The hatch facts are printed HERE rather than from inside save(), so the `result=` line the
    # caller keys on stays first, and so every writing path reports them without six call sites
    # having to remember to.
    #
    # The fallback matters as much as the facts: a re-run that changes nothing (`result=unchanged`)
    # never reaches save(), and the install skill would then have no path to hand the user. The
    # hatch is on disk from the first install, so report it whenever it is there.
    if HATCH_FACTS:
        emit(**HATCH_FACTS)
    elif args.cmd == "add" and rc == 0:
        # A successful `add` that wrote NOTHING — `result=unchanged`, i.e. the project is already
        # routed to this proxy. save() never ran, so without this branch the machines that most need
        # a hatch are exactly the ones that never get one: everybody who installed before the hatch
        # existed, whose re-run of /context-guru:install is a no-op. Re-running the install is also
        # the first thing anyone does when something looks wrong, so it is the natural repair moment.
        #
        # Gated on rc == 0, which is what makes it safe to record the file: `result=conflict` exits
        # 2, and that is somebody ELSE'S base URL we refused to touch — recording it here would tell
        # the hatch that context-guru edited a file it never wrote to.
        ensure_hatch(args.file)
        if HATCH_FACTS:
            emit(**HATCH_FACTS)
    return rc


if __name__ == "__main__":
    # Every failure is a key=value line, including the ones nobody planned for.
    #
    # The docstring promises "one key=value line per fact on stdout", and every failure path honoured
    # that EXCEPT genuine OS errors in backup()/save(): an unwritable directory produced a raw Python
    # traceback and no `result=` line at all. The calling skill is told to read these lines rather than
    # guess, so a traceback breaks the contract it depends on precisely when something is already wrong.
    #
    # Non-destructive in every case observed — the original file was untouched — but "it did not damage
    # anything" is not the same as "the caller can tell what happened".
    try:
        sys.exit(main())
    except OSError as exc:
        emit(result="error", reason="os_error", detail=f"{exc}")
        sys.exit(4)
