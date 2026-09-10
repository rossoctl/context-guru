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


def record_touch(real: str, existed: bool) -> None:
    """Note that we are about to edit `real`, and make sure a way back exists.

    Called from save() before the write, so `existed` is the truth about the file as the user had
    it. Idempotent: the original copy is created with O_EXCL and a second call never replaces it,
    which is the whole point — the tenth edit must not overwrite the record of the first.
    """
    state = state_dir()
    try:
        os.makedirs(os.path.join(state, "originals"), exist_ok=True)
    except OSError as exc:
        HATCH_FACTS["reset_hatch"] = "unavailable"
        HATCH_FACTS["reset_hatch_detail"] = f"cannot write {state}: {exc}"
        return

    original = ""
    if existed:
        dest = os.path.join(state, "originals", _slug(real) + ".original")
        try:
            fd = os.open(dest, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            original = dest          # already recorded, by an earlier run; keep THAT one
        except OSError:
            original = ""
        else:
            try:
                with os.fdopen(fd, "wb") as out, open(real, "rb") as srcf:
                    shutil.copyfileobj(srcf, out)
                original = dest
            except OSError:
                try:
                    os.unlink(dest)  # a half copy is worse than none: it would restore garbage
                except OSError:
                    pass
                original = ""

    jsonp, tsvp = _manifest_paths(state)
    entries: list[dict] = []
    try:
        with open(jsonp, encoding="utf-8") as fh:
            prior = json.load(fh)
        if isinstance(prior, dict) and isinstance(prior.get("files"), list):
            entries = [e for e in prior["files"] if isinstance(e, dict) and e.get("path")]
    except (OSError, json.JSONDecodeError):
        entries = []

    for e in entries:
        if e.get("path") == real:
            # Seen before. The first record is the authoritative one — `existed_before` describes
            # the file as it was before context-guru ever touched it, and re-recording it from a
            # later run would say "it existed" about a file we created ourselves.
            if not e.get("original") and original:
                e["original"] = original
            break
    else:
        entries.append({"path": real, "existed_before": bool(existed),
                        "original": original,
                        "first_touched": _dt.datetime.now().astimezone().isoformat(timespec="seconds")})

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
    if existed and not original:
        # Worth its own line: routing can be removed from the record, but the file's original
        # CONTENT is not recoverable from anything the hatch holds.
        HATCH_FACTS["reset_original"] = "unavailable"


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
    )
    return 0


def ensure_hatch(file: str) -> None:
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
        os.makedirs(os.path.join(state, "originals"), exist_ok=True)
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
    # SCOPE GATE, before anything is read or written.
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
    if is_user_scope(args.file) and not getattr(args, "user_scope", False):
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
    candidates = [
        os.path.join(".claude", "settings.local.json"),
        os.path.join(".claude", "settings.json"),
        os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or
                     os.path.join(os.path.expanduser("~"), ".claude"), "settings.json"),
    ]
    for path in candidates:
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
    cfg = sub.add_parser("config")
    cfg.add_argument("--plugin", default="context-guru@context-guru")

    args = ap.parse_args()
    if args.cmd == "add" and not args.url and not args.statusline:
        ap.error("add needs --url, or --statusline on its own for a statusline-only call")
    rc = {"add": cmd_add, "remove": cmd_remove, "show": cmd_show,
          "config": cmd_config}[args.cmd](args)

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
