"""The on-disk request budget shared by statusline.py and `cg.py cachestate` (and read by the mods).

Rules (CONTRACT section 4): one private directory per user, `${XDG_CACHE_HOME:-~/.cache}/context-guru/`
(0700, never a shared /tmp path); one cache file per (server, session, tag) with a TTL; a single
flight, so renders that start together do not all fetch: exactly one caller per window wins the
election and the rest serve what is cached; after a failure the next attempts back off exponentially,
1 s, 2 s, 4 s ... up to 5 minutes, and the recorded failure is served meanwhile. Stdlib only and
import-cheap: the status line has a 50 ms budget.
"""

from __future__ import annotations

import json
import os
import time

BACKOFF_BASE_SECONDS = 1.0
BACKOFF_MAX_SECONDS = 300.0

_UNUSABLE = "/nonexistent/context-guru"  # every read and write under it fails like a read-only cache
_dir_memo: dict[str, str] = {}


def cache_dir() -> str:
    """The private cache directory, created 0700 on first use. A directory that exists but is not
    ours (another user's, on a shared host) or is writable by others is never used: the result is a
    path under which nothing can be read or written, so caching quietly turns off instead of
    trusting foreign files."""
    base = os.environ.get("XDG_CACHE_HOME") or os.path.join(os.path.expanduser("~"), ".cache")
    path = os.path.join(base, "context-guru")
    if path in _dir_memo:
        return _dir_memo[path]
    result = path
    try:
        os.makedirs(path, mode=0o700, exist_ok=True)
        st = os.stat(path)
        if st.st_uid != os.getuid():
            result = _UNUSABLE
        elif st.st_mode & 0o077:
            os.chmod(path, 0o700)  # ours but too open (an older umask): close it rather than use it open
    except OSError:
        result = _UNUSABLE
    _dir_memo[path] = result
    return result


def age_of(path: str) -> float | None:
    try:
        return max(0.0, time.time() - os.stat(path).st_mtime)
    except OSError:
        return None


def read_json(path: str) -> dict | None:
    try:
        with open(path, encoding="utf-8") as fh:
            obj = json.load(fh)
    except (OSError, ValueError):
        return None
    return obj if isinstance(obj, dict) else None


def write_atomic(path: str, body: bytes) -> None:
    """tmp + rename, 0600. Never raises: caching is an optimisation."""
    tmp = f"{path}.{os.getpid()}.tmp"
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "wb") as fh:
            fh.write(body)
        os.replace(tmp, path)
    except OSError:
        try:
            os.unlink(tmp)
        except OSError:
            pass


def take_slot(path: str, window: float) -> bool:
    """True for exactly ONE caller per `window` seconds, however many start at once. The lock file's
    name carries the window number and is made with O_EXCL, so creating it is the election: no
    check-then-write gap, and no stale lock to clear (a crashed holder's belongs to an earlier
    window and stops mattering). The previous window's file is removed best effort."""
    slot = int(time.time() // window)
    try:
        os.close(os.open(f"{path}.lock.{slot}", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    except OSError:
        return False  # someone else holds this window, or the cache cannot be written: better no fetch than a storm
    for old in (f"{path}.lock.{slot - 1}", path + ".lock"):
        try:
            os.unlink(old)
        except OSError:
            pass
    return True


# ---- backoff -----------------------------------------------------------------------------------

def _fail_path(path: str) -> str:
    return path + ".fail"


def backoff(path: str) -> dict | None:
    """The recorded failure while its back-off is still running ({n, kind, until}), else None."""
    rec = read_json(_fail_path(path))
    if rec and isinstance(rec.get("until"), (int, float)) and rec["until"] > time.time():
        return rec
    return None


def record_failure(path: str, kind: str) -> dict:
    """Count one more consecutive failure and push the next attempt out: base * 2^(n-1), capped."""
    prev = read_json(_fail_path(path)) or {}
    n = int(prev.get("n", 0)) + 1 if isinstance(prev.get("n"), (int, float)) else 1
    delay = min(BACKOFF_MAX_SECONDS, BACKOFF_BASE_SECONDS * (2 ** min(n - 1, 20)))
    rec = {"n": n, "kind": kind, "until": time.time() + delay}
    write_atomic(_fail_path(path), json.dumps(rec).encode())
    return rec


def clear_failure(path: str) -> None:
    try:
        os.unlink(_fail_path(path))
    except OSError:
        pass
