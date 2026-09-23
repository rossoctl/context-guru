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
import hashlib
import json
import os
import re
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


# ---------------------------------------------------------------------------
# The escape hatch: a pre-edit copy of every file we touch, a record of what we touched, and a
# plain-sh script OUTSIDE THIS PLUGIN that puts it all back.
#
# The timestamped backups above are not enough on their own, and the reason is a real incident.
# `/context-guru:uninstall` is a SKILL — it needs a working Claude Code session. But the failure
# it exists for is "every request through the proxy fails", which is exactly the state in which
# no skill can run: the agent that would undo the routing cannot reach a model to be asked. A
# colleague hit that, 401 on every call, with the documented undo path unavailable for the same
# reason he needed it.
#
# So three things are established BEFORE any routing key exists, in one choke point that no
# write path can skip (see save()):
#
# 1. an ORIGINAL copy, taken once, never overwritten, never pruned. The backups beside the file
#    are capped at KEEP_BACKUPS and every add AND remove writes one, so on a machine that has
#    installed and uninstalled a few times the copy holding the user's pre-context-guru state is
#    the FIRST to be deleted. The one copy recovery depends on cannot be on a rolling window.
# 2. a record of which files we edited and whether each existed beforehand — a file we created
#    is put back by deleting it, not by restoring it, and nothing else can tell the difference.
# 3. the hatch script itself, copied out of the plugin into the state directory. A hatch that
#    lives inside the thing that broke goes away with `/plugin uninstall`, a marketplace
#    refresh, or a wiped plugin cache — i.e. it is missing in a fair share of the cases it is
#    for. The copy under the state directory has no dependency on this plugin still existing.
#
# All of it is best-effort: a read-only or missing state directory must not fail an install that
# would otherwise work. It is REPORTED instead (`reset_hatch=`), so the skill can tell the user
# whether they have a hatch rather than assuming it.
# ---------------------------------------------------------------------------

HATCH_NAME = "context-guru-reset"
MANIFEST_VERSION = 1

# Facts collected during the run and printed once by main(), rather than from inside save() —
# the caller reads `key=value` lines and the result line should come first.
HATCH_FACTS: dict[str, str] = {}


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


def _slug(real: str) -> str:
    """A filename for `real`'s original copy: readable, collision-free, path-shaped names flattened.

    The hash is what makes it unique — two projects both called `web` have the same tail — and the
    readable prefix is there because a user reading `ls` in a panic should be able to see which of
    these is their global settings file and which is a project's.
    """
    tail = f"{os.path.basename(os.path.dirname(real))}-{os.path.basename(real)}"
    safe = "".join(c if (c.isalnum() or c in "-._") else "_" for c in tail)
    # Strip the leading dot, because the common case produces one: the parent directory is
    # `.claude`, so the natural name is `.claude-settings.local.json.<hash>.original` — a HIDDEN
    # file. A test caught it: `ls originals/*.original` matched nothing while the file sat right
    # there. The directory exists to be read by a user whose sessions are down, and by any later
    # glob over it; neither sees a dotfile.
    safe = safe.lstrip(".") or "settings"
    return f"{safe}.{hashlib.sha256(real.encode('utf-8')).hexdigest()[:12]}"


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
# drwxrwxrwx. Another local account could then replace an entry in originals/ and have reset.sh write
# attacker-chosen content into the victim's ~/.claude/settings.json — a file that controls where all
# their model traffic goes and can hold a credential. mkdir's mode argument is masked by the umask, so
# the explicit chmod is what actually sets it.
STATE_DIR_MODE = 0o700


def ensure_state_dir(*parts: str) -> str:
    """makedirs the state path, force 0700 on every level we own, and return it. Raises on failure."""
    path = os.path.join(*parts)
    os.makedirs(path, mode=STATE_DIR_MODE, exist_ok=True)
    # Tighten what already existed too: a directory created by an older version of this script (or by
    # anything else) may be group- or world-writable, and trusting it is the whole vulnerability.
    for level in (path, os.path.dirname(path)):
        try:
            if level and os.path.isdir(level) and (os.stat(level).st_mode & 0o077):
                os.chmod(level, STATE_DIR_MODE)
        except OSError:
            pass
    return path


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


def _manifest_paths(state: str) -> tuple[str, str]:
    return (os.path.join(state, "reset-manifest.json"),
            os.path.join(state, "reset-manifest.tsv"))


def _render_tsv(entries: list[dict]) -> bytes:
    """The record in the form the hatch actually reads: one line per file, tab-separated.

    Why not have the hatch parse the JSON: it is POSIX sh with no interpreter available on the
    path that matters, and hand-rolled JSON parsing in sh is precisely the kind of cleverness a
    recovery tool must not contain. The JSON is kept beside it for anything else that wants
    structure.

    A path containing a tab or a newline cannot be represented on a line, and rather than write a
    line that would be misparsed into the wrong filename, such an entry is recorded as a comment
    and the caller is told the record is partial. Deleting or overwriting the wrong file is a much
    worse outcome than telling the user one path needs doing by hand.
    """
    out = [b"# context-guru reset record v%d - <existed_before 1|0>\t<original copy|->\t<path>" %
           MANIFEST_VERSION]
    for e in entries:
        path = e["path"]
        if "\t" in path or "\n" in path or "\r" in path:
            out.append(b"# UNREPRESENTABLE PATH (tab or newline); restore this one by hand: "
                       + repr(path).encode("utf-8"))
            continue
        out.append("\t".join(("1" if e["existed_before"] else "0",
                             e.get("original") or "-", path)).encode("utf-8"))
    return b"\n".join(out) + b"\n"


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
    (litellm's default is 127.0.0.1:4000) is precisely the value most worth having a copy of. Hence
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

    `except OSError` at three inner sites was nearly total and reviewed as not good enough: the body
    also encodes paths to UTF-8, which raises UnicodeEncodeError (not an OSError) for a filename
    carrying surrogates from surrogateescape. "Fail open, always" is a hard boundary in this repo, so
    the hatch machinery must not be able to fail a settings write for ANY reason — the install it
    would break is one that would otherwise have worked.
    """
    try:
        _record_touch(real, existed)
    except Exception as exc:                      # noqa: BLE001 - deliberate, see docstring
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"{type(exc).__name__}: {exc}"


def _record_touch(real: str, existed: bool) -> None:
    """Note that we are about to edit `real`, and make sure a way back exists.

    Called from save() before the write, so `existed` is the truth about the file as the user had
    it. Idempotent: the original copy is created with O_EXCL and a second call never replaces it,
    which is the whole point — the tenth edit must not overwrite the record of the first.
    """
    state = state_dir()
    try:
        ensure_state_dir(state, "originals")
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write {state}: {exc}"
        return

    original = ""
    skipped_copy = False
    if existed and _looks_routed_by_us(real):
        # Recorded, but with no original from THIS call — which is exactly the state ensure_hatch()
        # already produces, and the hatch's missing-copy branch already reports honestly.
        #
        # Deliberately not reported here. It was, and that was a review finding: an ordinary
        # uninstall of an ordinarily-installed project takes this branch (the file IS ours by then),
        # so `reset_original=unavailable` was printed while a good, verified-clean copy from the
        # install sat on disk. install/SKILL.md turns that fact into "the hatch can unroute but not
        # restore", so the skill would have told users their content was unrecoverable when it was
        # not. The fact is a statement about what the hatch HOLDS, so it is decided below, after the
        # manifest entry is known.
        skipped_copy = True
    elif existed:
        original = copy_once(real, os.path.join(state, "originals", _slug(real) + ".original"))

    jsonp, tsvp = _manifest_paths(state)
    entries: list[dict] = []
    try:
        with open(jsonp, encoding="utf-8") as fh:
            prior = json.load(fh)
        if isinstance(prior, dict) and isinstance(prior.get("files"), list):
            entries = [e for e in prior["files"] if isinstance(e, dict) and e.get("path")]
    except (OSError, json.JSONDecodeError):
        entries = []

    held_original = ""
    for e in entries:
        if e.get("path") == real:
            # Seen before. The first record is the authoritative one — `existed_before` describes
            # the file as it was before context-guru ever touched it, and re-recording it from a
            # later run would say "it existed" about a file we created ourselves.
            if not e.get("original") and original:
                e["original"] = original
            held_original = e.get("original") or ""
            break
    else:
        entries.append({"path": real, "existed_before": bool(existed),
                        "original": original,
                        "first_touched": _dt.datetime.now().astimezone().isoformat(timespec="seconds")})
        held_original = original

    hatch = install_hatch(state)
    try:
        _write_atomic(jsonp, (json.dumps({"version": MANIFEST_VERSION, "hatch": hatch,
                                          "files": entries}, indent=2) + "\n").encode("utf-8"))
        _write_atomic(tsvp, _render_tsv(entries))
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write the record: {exc}"
        return

    HATCH_FACTS["reset_hatch"] = hatch or "unavailable"
    HATCH_FACTS["reset_record"] = tsvp
    if existed and not held_original:
        # Judged against what the RECORD ends up holding, not against this call: a file whose
        # original was captured by an earlier install must not be reported as unrecoverable now.
        HATCH_FACTS["reset_original"] = "unavailable"
        HATCH_FACTS["reset_original_reason"] = (
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


def ensure_hatch(file: str) -> None:
    """Fail-open wrapper. See _ensure_hatch, and record_touch for why this is not `except OSError`."""
    try:
        _ensure_hatch(file)
    except Exception as exc:                      # noqa: BLE001 - deliberate, see record_touch
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"{type(exc).__name__}: {exc}"


def _ensure_hatch(file: str) -> None:
    """Put a hatch in place for a project that is already routed, without editing anything.

    There is no pre-edit copy to take — that moment passed, possibly in a version of this plugin
    that did not take one. So the record gets the path with no original, and the hatch reports
    honestly: it names the file, points at the timestamped backups beside it, and refuses to
    restore one automatically because a rolling backup may hold a LATER state rather than the
    user's original. That is strictly more use than "no record of any edit", which is what somebody
    with a pre-hatch install would otherwise get while their sessions were down.
    """
    state = state_dir()
    try:
        ensure_state_dir(state, "originals")
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write {state}: {exc}"
        return
    real = os.path.realpath(file)
    jsonp, tsvp = _manifest_paths(state)
    entries: list[dict] = []
    try:
        with open(jsonp, encoding="utf-8") as fh:
            prior = json.load(fh)
        if isinstance(prior, dict) and isinstance(prior.get("files"), list):
            entries = [e for e in prior["files"] if isinstance(e, dict) and e.get("path")]
    except (OSError, json.JSONDecodeError):
        entries = []
    if not any(e.get("path") == real for e in entries):
        entries.append({"path": real, "existed_before": True, "original": "",
                        "first_touched": "(unknown: routed before this record existed)"})
    hatch = install_hatch(state)
    try:
        _write_atomic(jsonp, (json.dumps({"version": MANIFEST_VERSION, "hatch": hatch,
                                          "files": entries}, indent=2) + "\n").encode("utf-8"))
        _write_atomic(tsvp, _render_tsv(entries))
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write the record: {exc}"
        return
    HATCH_FACTS["reset_hatch"] = hatch or "unavailable"
    HATCH_FACTS["reset_record"] = tsvp
    HATCH_FACTS["reset_original"] = "unavailable"


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
    # SCOPE GATE — on the ROUTING, which is the only thing whose scope matters.
    #
    # B2 in review: this was the first statement in the function, above the statusline-only early
    # return, so `add --file ~/.claude/settings.json --statusline <cmd>` — a call that writes no
    # routing key at all — was refused with `reason=user_scope_needs_flag`. Six documented invocations
    # broke, four in skills/statusline/SKILL.md and two in docs/how-to/install-plugin.md, a file this
    # PR edits. A statusline IS machine-wide by design and routes nothing, so the refusal text
    # ("this file routes EVERY project", "project scope is the default") was wrong for it as well.
    #
    # Gated on args.url for that reason: the blast-radius argument is entirely about where model
    # traffic is pointed. A statusline that fails renders a blank status line and breaks nothing.
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
    if args.url and is_user_scope(args.file) and not getattr(args, "user_scope", False):
        emit(result="error", reason="user_scope_needs_flag", file=args.file,
             note="this file routes EVERY project on the machine, so it needs --user-scope as "
                  "well. Confirm with the user first, naming that blast radius; project scope "
                  "(.claude/settings.local.json) is the default for a reason.")
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

    # --statusline is checked here, ahead of every base_url branch below, for the same reason the
    # `desired` set above exists at all: a conflict on it must stop the WHOLE call, including the
    # base_url side, rather than writing half an install and reporting success. It is a TOP-LEVEL
    # key (see apply_statusline), so it cannot join `desired`/`env` above; this is its own gate.
    sl_command = getattr(args, "statusline", "")
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
    save(args.file, data)
    emit(result="removed", file=args.file, backup=saved, statusline_restored=restored_sl)
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
        emit(result="conflict", reason="not_the_url_we_installed", file=args.file, existing=current,
             expected=args.url,
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
      `$context-guru.installed_base_url`, never from the URL's shape — because litellm's default is
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

    if target is None:
        # Nobody has configured this plugin's options in any of the three files yet. Land in
        # project-local scope by default — reversible, gitignored, affects only this repo — the
        # same default the install skill recommends; --user-scope opts into the machine-wide file
        # instead, for someone who wants every project to pick this preset up.
        target = (os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or
                                os.path.join(os.path.expanduser("~"), ".claude"), "settings.json")
                   if args.user_scope else os.path.join(".claude", "settings.local.json"))

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
                            "without this on `add`; never needed on `remove`.")
        p.add_argument("--statusline", default="",
                       help="on add: also write the TOP-LEVEL statusLine key ({\"type\": "
                            "\"command\", \"command\": <this value>}), refusing to replace one "
                            "that is not ours unless --force. on remove: taken back only if it "
                            "is exactly what a previous --statusline install recorded writing.")
    off = sub.add_parser("off",
        help="turn the status line off without touching routing — the counterpart to `add "
             "--statusline`, for when both were installed together and only the statusline "
             "should come back off")
    off.add_argument("--file", required=True)

    cfg = sub.add_parser("config")
    cfg.add_argument("--plugin", default="context-guru@context-guru")

    # `strategy` is the named-cache-strategy surface: the one place that decides what a name means,
    # so the skills that use it carry a NAME rather than four tuning numbers in a heredoc.
    # check-url exists so a caller can validate a supplied base URL BEFORE acting on it. Without
    # it the only enforcement point was `add`, i.e. after a proxy had been started and a health
    # check spent — a typo cost real work and surfaced as `settings_write_failed`, which names the
    # wrong step.
    cu = sub.add_parser("check-url")
    cu.add_argument("--url", required=True)

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
          "config": cmd_config, "strategy": cmd_strategy, "preset": cmd_preset,
          "check-url": cmd_check_url}[args.cmd](args)

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
