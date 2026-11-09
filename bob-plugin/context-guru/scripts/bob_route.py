#!/usr/bin/env python3
"""Write and remove the one rc-file block that sends IBM Bob's traffic to context-guru.

The rules come from Cortex (cmd/agentop/cmd_bobshell.go, cmd_bob_launch.go), which uses them in
production:

- One fixed block between two marker lines. Only the port changes. Remove takes out exactly
  that block, so install then remove gives back the same bytes.
- The block starts with its own newline. So the separator belongs to us, and remove takes it
  back. The user's last byte is never changed.
- Ownership comes from the text: ours (the exact block), not ours (the user sets the variable
  himself), or cannot tell (our markers with other text). We write only when it is ours or
  absent.
- Follow one symlink (a dotfiles repo). Refuse a longer chain or a dangling link.
- Write a temp file beside the target, keep its mode, then rename it over the target.
"""

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

START = b"# >>> context-guru (bob) >>>"
END = b"# <<< context-guru (bob) <<<"
BUNDLE_ID = "com.ibm.software.bob"
IDE_VAR = "VITE_GATEWAY_BASE_URL"
SHELL_VAR = "BOB_GATEWAY_URL"
MAX_HOPS = 1
NEW_FILE_MODE = 0o644
PROBE_TIMEOUT_S = 10  # VS Code gives its own environment resolve 10 s.

# A line that sets one of our variables outside our markers. The user's own value wins.
_USER_SETS = re.compile(rb"^\s*(export\s+)?(" + IDE_VAR.encode() + rb"|" + SHELL_VAR.encode() +
                        rb")=", re.M)
_OUR_BLOCK = re.compile(re.escape(b"\n" + START + b"\n") + rb".*?" + re.escape(END + b"\n"), re.S)


class RouteRefused(Exception):
    """The rc file has something we must not change. The message says what and why."""


def _block(port, unset_safe):
    url = f"http://127.0.0.1:{int(port)}"
    # ${VAR:-}: under `set -u` or zsh's `setopt nounset`, a bare unset $VAR stops the shell reading
    # the rest of the user's startup file. The legacy form is the one setup wrote before 2026-11-09.
    env, bundle = (b"${VSCODE_RESOLVING_ENVIRONMENT:-}", b"${__CFBundleIdentifier:-}") if unset_safe \
        else (b"$VSCODE_RESOLVING_ENVIRONMENT", b"$__CFBundleIdentifier")
    return (b"\n" + START + b"\n"
            b"# IBM Bob reads this when it starts. Remove it with the context-guru-uninstall skill.\n"
            b'if [ "' + env + b'" = 1 ] && '
            b'[ "' + bundle + b'" = ' + BUNDLE_ID.encode() + b" ]; then\n"
            b"  export " + IDE_VAR.encode() + b"=" + url.encode() + b"\n"
            b"fi\n"
            b"export " + SHELL_VAR.encode() + b"=" + url.encode() + b"\n" + END + b"\n")


def block(port):
    """The block setup writes, as bytes. One function, so install and remove cannot disagree."""
    return _block(port, unset_safe=True)


def our_blocks(port):
    """Every exact form of our block: the one setup writes now, and the legacy one."""
    return (block(port), _block(port, unset_safe=False))


def _port_of(text):
    match = re.search(rb"export " + IDE_VAR.encode() + rb"=http://127\.0\.0\.1:(\d{1,5})\n", text)
    return int(match.group(1)) if match else None


def ownership(content):
    """Return (state, port) for our block. state: absent, ours, not_ours, cannot_tell, duplicate.

    not_ours: no block of ours, and the user sets one of our variables himself.
    """
    found = _OUR_BLOCK.findall(content)
    markers = content.count(START) + content.count(END)
    if not markers:
        return ("not_ours", None) if _USER_SETS.search(content) else ("absent", None)
    if len(found) > 1:
        return "duplicate", None
    if len(found) == 1 and markers == 2:
        port = _port_of(found[0])
        if port is not None and found[0] in our_blocks(port):
            return "ours", port
    return "cannot_tell", None


def _refuse(path, state):
    if state == "not_ours":
        raise RouteRefused(f"{path} already sets {IDE_VAR} or {SHELL_VAR} outside our block. "
                           "Left alone: your value wins. Remove it by hand to use context-guru.")
    if state == "duplicate":
        count = None
        try:
            count = len(_OUR_BLOCK.findall(Path(path).read_bytes()))
        except OSError:
            pass
        raise RouteRefused(f"{path} has {count or 'several'} copies of the context-guru block. "
                           "Left alone: remove them by hand, between the marker lines.")
    raise RouteRefused(f"{path} has a context-guru block that was edited since it was written. "
                       "Left alone: remove it by hand, between the marker lines "
                       f"{START.decode()} and {END.decode()}.")


BASH_LOGIN_FILES = (".bash_profile", ".bash_login", ".profile")


def candidates(home, shell):
    """Every startup file setup could have written for $SHELL, in the order bash reads them."""
    name = os.path.basename(shell or "")
    home = Path(home)
    if name == "zsh":
        return [home / ".zshrc"]
    if name == "bash":
        return [home / candidate for candidate in BASH_LOGIN_FILES]
    return []


def rc_candidates(home, shell):
    """The rc file for $SHELL, or None. bash: the first file a bash LOGIN shell reads."""
    name = os.path.basename(shell or "")
    home = Path(home)
    if name == "zsh":
        return home / ".zshrc"
    if name == "bash":
        # Bob runs `$SHELL -i -l`. A bash login shell reads the first of these and not .bashrc.
        for candidate in BASH_LOGIN_FILES:
            if os.path.lexists(home / candidate):
                return home / candidate
        return home / ".bash_profile"
    return None


def _target(path):
    """Follow at most MAX_HOPS links. Return (end, hops, error). hops > MAX_HOPS: too deep."""
    current = Path(path)
    hops = 0
    while True:
        try:
            if not os.path.islink(current):
                os.stat(current)
                return current, hops, None
        except FileNotFoundError as error:
            return current, hops, error
        if hops >= MAX_HOPS:
            return current, hops + 1, None
        destination = Path(os.readlink(current))
        current = destination if destination.is_absolute() else current.parent / destination
        hops += 1


def follow(path):
    """Apply the symlink rule to any file we edit. Return (path to write, None) or (None, why).

    A missing file at the first hop is fine (we create it). A dangling link, a chain deeper than
    MAX_HOPS or a loop is refused. A file we cannot look at (EACCES, ELOOP) is refused too.
    """
    try:
        end, hops, error = _target(path)
    except OSError as error:
        return None, f"I cannot read {path}: {error}."
    if error is not None and hops > 0:
        return None, f"{path} is a dangling symlink. I do not write through it."
    if hops > MAX_HOPS:
        return None, f"{path} is a symlink chain deeper than {MAX_HOPS}. I do not follow it."
    return end, None


def resolve(home, shell):
    """Return (path to write, None) or (None, advice). Advice means: print the block instead."""
    try:
        path = rc_candidates(home, shell)
    except OSError as error:
        return None, f"I cannot read {home}: {error}."
    if path is None:
        return None, (f"I only know where zsh and bash read their startup files, and $SHELL is "
                      f"{shell or '(not set)'}.")
    return follow(path)


def _atomic_write(path, data):
    path = Path(path)
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


def _read(path):
    try:
        return Path(path).read_bytes()
    except FileNotFoundError:
        return None


def _present(content, port):
    """The exact form of our block that content holds. Call it only when ownership said ours."""
    return next(b for b in our_blocks(port) if b in content)


def _writable_state(content):
    """Return (state, port), with not_ours when the user also sets a variable outside our block."""
    state, port = ownership(content)
    if state == "ours" and _USER_SETS.search(content.replace(_present(content, port), b"", 1)):
        return "not_ours", None
    return state, port


def check(path):
    """Raise RouteRefused when install would refuse. Writes nothing."""
    state, _ = _writable_state(_read(path) or b"")
    if state not in ("absent", "ours"):
        _refuse(path, state)


def install(path, port):
    """Add or update our block. Return "installed" or "already". Raise RouteRefused."""
    content = _read(path)
    current = content or b""
    state, old_port = _writable_state(current)
    # A legacy block on the same port is not "already": it is replaced by the unset-safe form.
    if state == "ours" and old_port == int(port) and block(port) in current:
        return "already"
    if state == "ours":
        updated = current.replace(_present(current, old_port), block(port), 1)
    elif state == "absent":
        updated = current + block(port)
    else:
        _refuse(path, state)
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    _atomic_write(path, updated)
    return "installed"


def remove(path, created=False):
    """Remove our block. Return "removed" or "absent". Raise RouteRefused.

    created: we made the file, so remove it when nothing else is left in it.
    """
    content = _read(path)
    if content is None:
        return "absent"
    state, port = ownership(content)
    if state in ("absent", "not_ours"):
        return "absent"
    if state != "ours":
        _refuse(path, state)
    updated = content.replace(_present(content, port), b"", 1)
    if created and not updated:
        os.unlink(path)
    else:
        _atomic_write(path, updated)
    return "removed"


def is_routed(path, port=None):
    content = _read(path)
    if content is None:
        return False
    state, found = ownership(content)
    return state == "ours" and (port is None or found == int(port))


def backup_once(path, folder):
    """Copy the rc file once, before our first write. A later setup keeps the first copy."""
    folder = Path(folder)
    target = folder / (Path(path).name + ".before-context-guru")
    if target.exists() or not Path(path).exists():
        return target
    folder.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(path, target)
    os.chmod(target, 0o600)
    return target


_PROBE = (f'printf "<<cg>>%s<<cg>>%s<<cg>>" "${IDE_VAR}" "${SHELL_VAR}"')


def _run_probe(home, shell, bob):
    env = {"HOME": str(home), "SHELL": shell, "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
           "USER": os.environ.get("USER", ""), "TERM": "dumb"}
    if bob:
        env.update(VSCODE_RESOLVING_ENVIRONMENT="1", __CFBundleIdentifier=BUNDLE_ID)
    try:
        done = subprocess.run([shell, "-i", "-l", "-c", _PROBE], env=env, cwd=str(home),
                              stdin=subprocess.DEVNULL, capture_output=True,
                              timeout=PROBE_TIMEOUT_S)
    except (OSError, subprocess.TimeoutExpired):
        return None
    parts = done.stdout.split(b"<<cg>>")
    if len(parts) < 4:
        return None
    return parts[-3].decode(errors="replace"), parts[-2].decode(errors="replace")


def probe(home, shell):
    """Start the login shell the way Bob does, and once as a plain terminal.

    Return the values seen: ide (Bob's resolve), terminal_ide (must stay empty: the guard
    works), terminal_shell (Bob Shell in a terminal). An empty string is a miss.
    """
    bob = _run_probe(home, shell, True)
    terminal = _run_probe(home, shell, False)
    return {"ide": bob[0] if bob else "", "terminal_ide": terminal[0] if terminal else "",
            "terminal_shell": terminal[1] if terminal else ""}
