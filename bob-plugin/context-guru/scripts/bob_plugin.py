#!/usr/bin/env python3
"""Install and run context-guru for IBM Bob (macOS).

Subcommands:
  setup      Plan (--plan), or with consent: get the binary, start the launchd agent, wait for
             /healthz, write the rc block and the hooks, check the rc block, roll back on failure.
  status     Routed? Proxy healthy? Bob traffic seen recently? Prints key=value facts.
  ensure     SessionStart hook. Start the agent if the proxy is down. Prints nothing.
  check      UserPromptSubmit hook. One line when the proxy is down, else nothing.
  signal     SessionStart hook. Send the session id, event and source to /signal. Prints nothing.
  uninstall  Remove the hooks, the rc block and the agent. Keep data unless --purge.

Every hook handler exits 0 (fail open). The hook stdin can hold the user's prompt
(UserPromptSubmit) or model text (Stop, PostCompact). We never write it, print it or send it.
"""

import argparse
import contextlib
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
import bob_hooks  # noqa: E402
import bob_route  # noqa: E402

LABEL = "com.rossoctl.context-guru.bob"
CONFIG_MARKER = "# Managed by the context-guru Bob plugin."
DEFAULT_UPSTREAM = "https://api.us-east.bob.ibm.com"
RELEASE_REPO = "rossoctl/context-guru"
RELEASE_BASE = f"https://github.com/{RELEASE_REPO}/releases"
# The first release with the Bob gateway and its region fix for the profile reply.
MINIMUM_BOB_RELEASE = (0, 3, 1)
# The presets Bob may use: name -> the config body under the marker line. Only presets with no
# lossy component. Every other CG preset can drop content, and CG must be able to give it back
# (the expand loop). That is not proven for Bob yet (plan phase P2, test T4). Phase P2 widens this.
BOB_PRESETS = {"off": "preset: off\n", "lossless": "pipeline: [format, toon, cachesplit]\n"}
DEFAULT_PRESET = "lossless"
# The cache strategies Bob may use. Only none: see NO_KEEPALIVE_REASON.
BOB_CACHE_STRATEGIES = ("none",)
NO_KEEPALIVE_REASON = (
    "Keep-alive pings do not apply to Bob. Bob's model call is OpenAI chat/completions, and the "
    "proxy pings only Anthropic and OpenAI Responses caches. Also, Bob has no SessionEnd event, "
    "so a ping could not stop when a Bob task ends.")
STATUSBAR_ID = "rossoctl.context-guru-bob"
BOBIDE_PATHS = tuple(app / "Contents/Resources/app/bin/bobide" for app in (
    Path.home() / "Applications/IBM Bob.app", Path("/Applications/IBM Bob.app")))
BOBIDE_TIMEOUT_S = 60
HEALTH_WAIT_S = 10
SERVICE_WAIT_S = 5
BOOTSTRAP_TRIES = 3
HOOK_HTTP_TIMEOUT_S = 1
RECENT_TRAFFIC_S = 24 * 3600
LOGIN_NOTE = ("Bob keeps its login per backend address, so you must log in to Bob once after "
              "this change")


# ---------------------------------------------------------------------------- paths

def state_dir():
    base = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
    return base / "context-guru-bob"


def record_path():
    return state_dir() / "install.json"


def hooks_state():
    return state_dir() / "hooks"


def proxy_config(port):
    return state_dir() / f"bob-{port}.yaml"


def dashboard_db(port):
    return state_dir() / f"dashboard-{port}.db"


def managed_binary():
    return state_dir() / "bin/context-guru-proxy"


def launchd_plist():
    return Path.home() / f"Library/LaunchAgents/{LABEL}.plist"


def bob_settings():
    return Path.home() / ".bob/settings/settings.json"


def reset_path():
    return state_dir() / "context-guru-reset"


def statusbar_vsix():
    """The status bar extension, built by `make bob-statusbar` in the context-guru repo.

    Not in git (a build output). Until the install script ships it (plan phase P7), setup
    skips the status bar when this file is absent.
    """
    return Path(__file__).resolve().parent.parent / "statusbar/context-guru-bob.vsix"


def find_bobide():
    """Bob's command line (a VS Code `code` CLI), or None. CONTEXT_GURU_BOBIDE overrides it."""
    override = os.environ.get("CONTEXT_GURU_BOBIDE")
    for path in [Path(override)] if override else BOBIDE_PATHS:
        if path.is_file() and os.access(path, os.X_OK):
            return path
    return None


def bobide(*args, timeout=BOBIDE_TIMEOUT_S):
    """Run bobide. Return its stdout, or None when it is missing, fails or is too slow.

    Its output holds only extension ids and versions. We never print it.
    """
    path = find_bobide()
    if path is None:
        return None
    try:
        done = subprocess.run([str(path), *args], capture_output=True, text=True,
                              timeout=timeout, stdin=subprocess.DEVNULL)
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout if done.returncode == 0 else None


def install_statusbar():
    """Install the status bar extension into Bob. Never raises: it must not fail setup.

    Return (installed, reason). The reason is set only when it was skipped.
    """
    try:
        vsix = statusbar_vsix()
        if not vsix.is_file():
            return False, "no_vsix"
        if find_bobide() is None:
            return False, "no_bobide"
        if bobide("--install-extension", str(vsix)) is None:
            return False, "install_failed"
        return True, None
    except Exception:
        return False, "install_failed"


def statusbar_installed():
    """True, False, or None when bobide cannot say. Read-only."""
    listed = bobide("--list-extensions", timeout=10)
    if listed is None:
        return None
    return STATUSBAR_ID in listed.split()


class StatusbarRefused(Exception):
    pass


def remove_statusbar():
    if bobide("--uninstall-extension", STATUSBAR_ID) is None:
        raise StatusbarRefused(f"bobide could not remove it. Run: bobide --uninstall-extension "
                               f"{STATUSBAR_ID}")
    return "removed"


# ---------------------------------------------------------------------------- small helpers

def facts(**values):
    for key, value in values.items():
        if isinstance(value, (dict, list)):
            value = json.dumps(value, separators=(",", ":"))
        print(f"{key}={value}")


def read_record():
    try:
        record = json.loads(record_path().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return record if isinstance(record, dict) else {}


def record_port(record):
    port = record.get("port")
    return port if isinstance(port, int) and 0 < port < 65536 else 0


def private_dir(path):
    """Make a folder that only the user can read. It holds the dashboard data and logs."""
    path = Path(path)
    path.mkdir(parents=True, exist_ok=True)
    os.chmod(path, 0o700)
    return path


def write_record(record):
    """Write install.json. ensure_ascii=False and one key per line: reset.sh reads it with sed."""
    private_dir(state_dir())
    temporary = record_path().with_suffix(".tmp")
    temporary.write_text(json.dumps(record, indent=2, ensure_ascii=False) + "\n",
                         encoding="utf-8")
    temporary.chmod(0o600)
    os.replace(temporary, record_path())


def unsafe_path(path):
    """reset.sh reads paths from install.json with sed, so a path must not hold `"` or `\\`."""
    text = str(path)
    return '"' in text or "\\" in text or "\n" in text


# Calls to the local proxy never use the user's HTTP_PROXY. On a corporate Mac it is often set with
# no NO_PROXY entry for 127.0.0.1: the calls then fail, and the signal hook would send the Bob
# session id off the machine. Downloads from GitHub keep the user's proxy (fetch, latest_release_tag).
LOOPBACK = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def healthy(port, timeout=1):
    try:
        with LOOPBACK.open(f"http://127.0.0.1:{port}/healthz", timeout=timeout) as reply:
            return reply.status == 200
    except Exception:
        return False


def wait_healthy(port):
    deadline = time.monotonic() + HEALTH_WAIT_S
    while time.monotonic() < deadline:
        if healthy(port):
            return True
        time.sleep(0.1)
    return False


def first_free_port():
    # One port for Bob on this machine: Bob's environment is global, not per project.
    # 8787..8850 belongs to the Claude plugin's per-project ports, so Bob starts above it.
    for port in range(8891, 8991):
        with socket.socket() as sock:
            try:
                sock.bind(("127.0.0.1", port))
                return port
            except OSError:
                pass
    raise RuntimeError("no free port in 8891..8990")


def gui_domain():
    return f"gui/{os.getuid()}"


def launchctl(*args, timeout=10):
    try:
        return subprocess.run(["launchctl", *args], capture_output=True, text=True,
                              timeout=timeout).returncode
    except (OSError, subprocess.TimeoutExpired):
        return -1


def plist_is_ours():
    try:
        with open(launchd_plist(), "rb") as handle:
            return plistlib.load(handle).get("ContextGuruManaged") is True
    except (OSError, ValueError, plistlib.InvalidFileException):
        return False


# ---------------------------------------------------------------------------- binary (from Codex)

def binary_problem(path):
    """Why this binary cannot run the Bob gateway, or None when it can."""
    try:
        done = subprocess.run([path, "--help"], capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.TimeoutExpired) as error:
        return f"{path} does not run ({type(error).__name__})"
    if "-bob-upstream" not in done.stdout + done.stderr:
        return f"{path} is too old: it has no --bob-upstream"
    return None


def find_binary(reasons=None):
    """The first context-guru-proxy that has the Bob gateway. Say why each other one is skipped."""
    candidates = []
    found = shutil.which(os.environ.get("CONTEXT_GURU_BIN", "context-guru-proxy"))
    if found:
        candidates.append(str(Path(found).resolve()))
    if managed_binary().is_file() and os.access(managed_binary(), os.X_OK):
        candidates.append(str(managed_binary().resolve()))
    for candidate in dict.fromkeys(candidates):
        problem = binary_problem(candidate)
        if problem is None:
            return candidate
        if reasons is not None:
            reasons.append(problem)
    return None


def release_platform():
    machines = {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}
    if platform.system() != "Darwin" or platform.machine() not in machines:
        raise RuntimeError(f"unsupported platform {platform.system()}/{platform.machine()}: "
                           "the Bob plugin is macOS only for now")
    return "darwin", machines[platform.machine()]


def latest_release_tag():
    request = urllib.request.Request(
        f"{RELEASE_BASE}/latest", method="HEAD")
    with urllib.request.urlopen(request, timeout=15) as response:
        tag = response.geturl().rstrip("/").rsplit("/", 1)[-1]
    if not tag or tag == "latest":
        raise RuntimeError("could not resolve the latest context-guru release")
    return tag


def fetch(url):
    with urllib.request.urlopen(url, timeout=60) as response:
        return response.read()


RELEASE_TAG = re.compile(r"v?(\d+)\.(\d+)\.(\d+)(-[A-Za-z0-9.]+)?")


def release_version(tag):
    """(major, minor, patch) of a tag like v0.5.1 or 0.5.1-rc.1. Refuse anything else: the tag
    becomes part of a download URL and a file name."""
    match = RELEASE_TAG.fullmatch(tag or "")
    if not match:
        raise RuntimeError(f"unrecognized context-guru release tag: {tag!r}")
    return tuple(int(part) for part in match.groups()[:3])


def compare_versions(a, b):
    """-1, 0 or 1 by number (v0.5.10 > v0.5.9). None when one of them is not a version."""
    try:
        left, right = release_version(a), release_version(b)
    except RuntimeError:
        return None
    return (left > right) - (left < right)


def install_release_binary(version=None, destination=None):
    """Download the release archive, check its SHA-256 against checksums.txt, install it.

    destination: where to put the binary (default: the managed path). update uses a staging path.
    """
    version = version or os.environ.get("CONTEXT_GURU_VERSION") or latest_release_tag()
    if release_version(version) < MINIMUM_BOB_RELEASE:
        raise RuntimeError(f"release {version} has no Bob region fix; v0.3.1 or newer is needed")
    os_name, arch = release_platform()
    archive_name = f"context-guru_{version.removeprefix('v')}_{os_name}_{arch}.tar.gz"
    base = f"{RELEASE_BASE}/download/{version}"
    archive = fetch(f"{base}/{archive_name}")
    expected = None
    for line in fetch(f"{base}/checksums.txt").decode("utf-8").splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[-1].lstrip("*") == archive_name:
            expected = fields[0].lower()
            break
    if not expected:
        raise RuntimeError(f"release checksum does not list {archive_name}")
    if hashlib.sha256(archive).hexdigest() != expected:
        raise RuntimeError(f"release checksum mismatch for {archive_name}")
    destination = Path(destination or managed_binary())
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
        matches = [m for m in bundle.getmembers()
                   if m.isfile() and Path(m.name).name == "context-guru-proxy"]
        if len(matches) != 1:
            raise RuntimeError("release archive does not contain exactly one context-guru-proxy")
        source = bundle.extractfile(matches[0])
        fd, temporary = tempfile.mkstemp(prefix="context-guru-proxy.", dir=destination.parent)
        try:
            with os.fdopen(fd, "wb") as output:
                shutil.copyfileobj(source, output)
            os.chmod(temporary, 0o755)
            os.replace(temporary, destination)
        finally:
            Path(temporary).unlink(missing_ok=True)
    return str(destination.resolve()), version


def binary_version(path):
    """The version a binary reports (`context-guru-proxy v0.5.1 …` -> v0.5.1), or unknown."""
    try:
        done = subprocess.run([path, "--version"], capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.TimeoutExpired):
        return "unknown"
    fields = done.stdout.split()
    return fields[1] if done.returncode == 0 and len(fields) >= 2 else "unknown"


# ---------------------------------------------------------------------------- service

def valid_upstream(url):
    """Only an https Bob host: an agent told by a prompt must not send traffic elsewhere."""
    return bool(re.fullmatch(r"https://[a-z0-9-]+(\.[a-z0-9-]+)*\.bob\.ibm\.com/?", url or ""))


def proxy_command(executable, port, upstream):
    return [executable, "--listen", f"127.0.0.1:{port}", "--bob-upstream", upstream.rstrip("/"),
            "--dashboard", "--dashboard-db", str(dashboard_db(port)),
            "--config", str(proxy_config(port))]


def read_preset(port):
    """The Bob preset in the managed config, DEFAULT_PRESET when there is no file.

    Raise RuntimeError for a file that is not ours. Return "custom" for our marker with a body
    that is not one of BOB_PRESETS.
    """
    path = proxy_config(port)
    try:
        text = path.read_text()
    except FileNotFoundError:
        return DEFAULT_PRESET
    if not text.startswith(CONFIG_MARKER + "\n"):
        raise RuntimeError(f"refusing to overwrite unmanaged proxy config: {path}")
    body = text[len(CONFIG_MARKER) + 1:]
    return next((name for name, value in BOB_PRESETS.items() if value == body), "custom")


def write_proxy_config(port, preset=None):
    """Write the managed config, and return the preset now in it.

    preset None keeps the current choice (a setup re-run). A "custom" file (our marker with a
    body we did not write) is kept as it is: only a named preset replaces it.
    """
    path = proxy_config(port)
    current = read_preset(port)
    if preset is None and current == "custom":
        return current
    preset = preset or (current if current in BOB_PRESETS else DEFAULT_PRESET)
    if preset not in BOB_PRESETS:
        raise RuntimeError(preset_refusal(preset))
    private_dir(path.parent)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(f"{CONFIG_MARKER}\n{BOB_PRESETS[preset]}")
    temporary.chmod(0o600)
    os.replace(temporary, path)
    return preset


def preset_refusal(name):
    allowed = " and ".join(BOB_PRESETS)
    return (f"{name} can drop content that Bob cannot get back yet. Only {allowed} are allowed "
            "for Bob until reversibility is proven.")


def start_service(executable, port, upstream):
    """Write our launchd agent and start it. Always on (plan D4): KeepAlive, no idle exit."""
    plist = launchd_plist()
    if plist.exists() and not plist_is_ours():
        raise RuntimeError(f"refusing to overwrite a launchd agent that is not ours: {plist}")
    plist.parent.mkdir(parents=True, exist_ok=True)
    log = state_dir() / "proxy.log"
    _write_plist(plistlib.dumps({"Label": LABEL, "ContextGuruManaged": True,
                                 "ProgramArguments": proxy_command(executable, port, upstream),
                                 "RunAtLoad": True, "KeepAlive": True,
                                 "ProcessType": "Background", "StandardOutPath": str(log),
                                 "StandardErrorPath": str(log)}))
    load_service()


def _write_plist(data):
    plist = launchd_plist()
    temporary = plist.with_suffix(".tmp")
    temporary.write_bytes(data)
    temporary.chmod(0o600)
    os.replace(temporary, plist)


def load_service():
    """Load the plist that is on disk now. Stop the running copy first. Raise on failure."""
    plist = launchd_plist()
    service = f"{gui_domain()}/{LABEL}"
    launchctl("bootout", service)
    # bootout returns before launchd lets the label go. A bootstrap in that window fails.
    deadline = time.monotonic() + SERVICE_WAIT_S
    while launchctl("print", service) == 0 and time.monotonic() < deadline:
        time.sleep(0.1)
    for attempt in range(BOOTSTRAP_TRIES):
        if launchctl("bootstrap", gui_domain(), str(plist)) == 0:
            break
        time.sleep(0.5 * (attempt + 1))
    else:
        raise RuntimeError("launchctl bootstrap failed")
    launchctl("kickstart", "-k", service)


def stop_service():
    """Stop and remove our agent. Return stopped, absent or not_ours."""
    plist = launchd_plist()
    if not plist.exists():
        return "absent"
    if not plist_is_ours():
        return "not_ours"
    launchctl("bootout", f"{gui_domain()}/{LABEL}")
    plist.unlink(missing_ok=True)
    return "stopped"


def install_escape_hatch():
    """Copy reset.sh out of the plugin folder, so it still works if the plugin is gone."""
    target = reset_path()
    private_dir(target.parent)
    shutil.copyfile(Path(__file__).with_name("reset.sh"), target)
    target.chmod(0o700)
    return target


# ---------------------------------------------------------------------------- setup

def _hook_groups():
    return bob_hooks.our_groups(sys.executable or "/usr/bin/python3",
                                str(Path(__file__).resolve()))


def setup(args):
    shell = os.environ.get("SHELL", "")
    rc_file, advice = bob_route.resolve(Path.home(), shell)
    if args.plan:
        facts(result="planned", shell=shell or "(not set)",
              rc_file=rc_file or "(none: the block is printed for you to add)",
              settings=bob_settings(), service_file=launchd_plist(), state=state_dir(),
              upstream=args.upstream, reset=reset_path(),
              note=LOGIN_NOTE + ", and once more after you remove it.",
              consent_question="Send IBM Bob's model traffic on this Mac through a local "
                               "context-guru proxy that always runs, and add a block to your "
                               "shell startup file and two hooks to Bob's settings?")
        if advice:
            facts(rc_advice=advice)
        return 0
    if not args.i_consent_to_traffic_interception:
        facts(result="refused", reason="consent_required")
        return 2
    if not valid_upstream(args.upstream):
        facts(result="refused", reason="bad_upstream", upstream=args.upstream,
              detail="The upstream must be an https address on a .bob.ibm.com host.")
        return 2
    try:
        settings_target = bob_hooks.target(bob_settings())
    except bob_hooks.HooksRefused as error:
        facts(result="refused", reason="settings_not_plain_json", detail=error)
        return 2
    for path in (Path.home(), state_dir(), rc_file or "", bob_settings(), settings_target):
        if unsafe_path(path):
            facts(result="refused", reason="unsafe_path", path=path,
                  detail='A path with " or \\ breaks the reset script. Use another folder.')
            return 2
    try:
        bob_hooks.check(bob_settings())
    except bob_hooks.HooksRefused as error:
        facts(result="refused", reason="settings_not_plain_json", detail=error)
        return 2
    except OSError as error:
        facts(result="refused", reason="settings_unreadable", detail=error)
        return 2
    if rc_file is not None:
        try:
            bob_route.check(rc_file)
        except bob_route.RouteRefused as error:
            facts(result="refused", reason="rc_block_not_ours", rc_file=rc_file, detail=error)
            return 2

    try:
        with state_lock():
            return _setup_changes(args, shell, rc_file, advice)
    except Busy:
        return _busy()


def _setup_changes(args, shell, rc_file, advice):
    """The part of setup that changes files. Runs under the state lock."""
    skipped = []
    executable = find_binary(skipped)
    for reason in skipped:
        facts(binary_skipped=reason)
    if not executable:
        try:
            executable, version = install_release_binary()
            facts(binary="installed", binary_version=version, binary_path=executable)
        except Exception as error:
            facts(result="binary_install_failed", detail=error)
            return 2

    existing = read_record()
    port = record_port(existing) or first_free_port()
    rc_created = bool(existing.get("rc_created")) or (
        rc_file is not None and not os.path.lexists(rc_file))
    # What was already in place before this run. A failed re-run must not undo it.
    had = {"service": plist_is_ours() and healthy(port),
           "rc": rc_file is not None and bob_route.is_routed(rc_file, port),
           "hooks": bob_hooks.is_installed(bob_settings())}
    # The old agent, so a failed re-run can put it back.
    old_plist = launchd_plist().read_bytes() if plist_is_ours() else None
    record = {"port": port, "binary": executable, "upstream": args.upstream,
              "rc_file": str(rc_file) if rc_file else None, "rc_created": rc_created,
              "settings": str(bob_settings()), "complete": False}
    old_config = _snapshot([proxy_config(port)])  # the user's preset, put back on a failure
    undo = Undo(had, old_plist, existing, port, rc_file, rc_created, old_config)
    try:
        # Written before the first change: if setup is killed, uninstall and reset.sh still
        # know which rc file and port to clean up.
        write_record(record)
        undo.add("config")
        preset = write_proxy_config(port)
        # Each step is added BEFORE it runs: a step that fails half way is undone too.
        undo.add("service")
        start_service(executable, port, args.upstream)
        if not wait_healthy(port):
            ok, failed = undo.run()
            facts(result="start_failed", port=port, log=state_dir() / "proxy.log",
                  rolled_back=str(ok).lower(), **_failed_facts(failed))
            return 1
        if rc_file is not None:
            bob_route.backup_once(rc_file, private_dir(state_dir() / "backup"))
            undo.add("rc")
            bob_route.install(rc_file, port)
        undo.add("hooks")
        bob_hooks.install(bob_settings(), hooks_state(), _hook_groups())
        reset = install_escape_hatch()
        installed, reason = install_statusbar()
        facts(statusbar="installed" if installed else "skipped")
        if reason:
            facts(statusbar_reason=reason)
        # Keep True from an older run: the extension it installed is still there for uninstall.
        record["statusbar"] = installed or existing.get("statusbar") is True
        record["complete"] = True
        write_record(record)
    except (Exception, KeyboardInterrupt) as error:
        ok, failed = undo.run()
        facts(result="setup_failed", detail=str(error) or type(error).__name__,
              rolled_back=str(ok).lower(), **_failed_facts(failed))
        return 130 if isinstance(error, KeyboardInterrupt) else 1

    facts(port=port, preset=preset, service_file=launchd_plist(), settings=bob_settings(),
          reset=reset)
    if preset == "custom":
        facts(preset_note=f"{proxy_config(port)} was edited by hand. Setup kept it as it is.")
    if rc_file is None:
        facts(result="installed_route_by_hand", rc_advice=advice)
        print("Add this block to the startup file your login shell reads:")
        sys.stdout.write(bob_route.block(port).decode())
        _next_steps()
        return 1
    seen = bob_route.probe(Path.home(), shell)
    url = f"http://127.0.0.1:{port}"
    ok = seen["ide"] == url and seen["terminal_ide"] == ""
    facts(rc_file=rc_file, rc_probe="ok" if ok else "miss", rc_probe_seen=seen["ide"] or "(empty)")
    if not ok:
        facts(result="installed_not_verified",
              detail=f"A login shell started the way Bob starts it did not set "
                     f"{bob_route.IDE_VAR}={url}. Check that {shell} -l reads {rc_file}.")
        _next_steps()
        return 1
    facts(result="installed")
    _next_steps()
    return 0


def _next_steps():
    print("Next: quit and reopen Bob (from the Dock), then log in once. " + LOGIN_NOTE + ".")


def _failed_facts(failed):
    if not failed:
        return {}
    return {"rollback_failed": ",".join(failed),
            "fix": f"Run {reset_path()} --yes, or the context-guru-uninstall skill."}


class Undo:
    """What a setup run changed, and how to undo it. Every undo step is safe to run twice."""

    def __init__(self, had, old_plist, old_record, port, rc_file, rc_created, old_config):
        self.steps = []
        self.had, self.old_plist, self.old_record = had, old_plist, old_record
        self.port, self.rc_file, self.rc_created = port, rc_file, rc_created
        self.old_config = old_config

    def add(self, step):
        self.steps.append(step)

    def run(self):
        """Undo newest first. Keep what was there before this run. Return (ok, failed steps).

        The record stays when an undo failed or an older install is still in place, so
        uninstall and reset.sh can finish the job.
        """
        failed = []
        for step in reversed(self.steps):
            try:
                if not self._undo(step):
                    failed.append(step)
            except Exception:
                failed.append(step)
        older = self.old_plist is not None or self.had["rc"] or self.had["hooks"]
        try:
            if older and self.old_record:
                write_record(self.old_record)
            elif not failed and not older:
                record_path().unlink(missing_ok=True)
        except OSError:
            failed.append("record")
        return not failed, failed

    def _undo(self, step):
        if step == "service":
            if self.old_plist is None:
                return stop_service() != "not_ours"
            # A re-run: put the old agent back, and check that it answers again.
            _write_plist(self.old_plist)
            load_service()
            return wait_healthy(record_port(self.old_record) or self.port)
        if self.had.get(step):
            return True
        if step == "hooks":
            bob_hooks.remove(bob_settings(), hooks_state())
        elif step == "rc":
            bob_route.remove(self.rc_file, created=self.rc_created)
        elif step == "config":
            # Put back the file as it was before this run: the user's preset, or no file.
            return not _restore(self.old_config)
        return True


# ---------------------------------------------------------------------------- safe restart

def _snapshot(paths):
    """The bytes of each file now (None when absent), to put them back after a failed change."""
    saved = {}
    for path in paths:
        try:
            saved[path] = Path(path).read_bytes()
        except FileNotFoundError:
            saved[path] = None
    return saved


def _restore_one(path, data):
    path = Path(path)
    if data is None:
        path.unlink(missing_ok=True)
        return
    mode = path.stat().st_mode & 0o7777 if path.exists() else 0o600
    temporary = path.with_name(path.name + ".restore")
    temporary.write_bytes(data)
    os.chmod(temporary, mode)
    os.replace(temporary, path)


def _restore(saved):
    """Put back every file. Try all of them. Return the list of "path: error" that failed."""
    failed = []
    for path, data in saved.items():
        try:
            _restore_one(path, data)
        except Exception as error:
            failed.append(f"{path}: {error}")
    return failed


def change_and_restart(port, change, keep):
    """Run change(), restart our agent, and check /healthz. On any failure put back every file
    in `keep` (the plist always) and the old agent, and check that it answers again.

    Return the facts to print: proxy_restarted, and rolled_back + fix on a failure.
    """
    saved = _snapshot([launchd_plist(), *keep])
    interrupted = None
    try:
        change()
        load_service()
        if wait_healthy(port):
            return {"proxy_restarted": "true"}
        reason = "the proxy did not answer /healthz"
    except BaseException as error:  # Ctrl-C too: restore first, then let it go on
        interrupted = None if isinstance(error, Exception) else error
        reason = str(error) or type(error).__name__
    values = {"proxy_restarted": "false", "detail": reason}
    failed = _restore(saved)
    try:
        load_service()
        values["rolled_back"] = str(not failed and wait_healthy(port)).lower()
    except Exception as error:
        values["rolled_back"] = "false"
        failed.append(f"restart: {error}")
    if failed:
        values["rolled_back"] = "false"
        values["rollback_detail"] = "; ".join(failed)
    if values["rolled_back"] == "false":
        values["fix"] = (f"The old proxy did not come back. See {state_dir() / 'proxy.log'}, or "
                         f"run {reset_path()} --yes so Bob talks to IBM directly.")
    if interrupted is not None:
        facts(**values)
        raise interrupted
    return values


class Busy(Exception):
    """Another setup, configure or update holds the state lock."""


@contextlib.contextmanager
def state_lock():
    """Hold the state folder's lock for one changing run, so two runs cannot interleave."""
    private_dir(state_dir())
    with open(state_dir() / "lock", "a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise Busy() from None
        try:
            yield
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


def _busy():
    facts(result="busy", detail="Another context-guru setup, configure, update or uninstall "
                                "runs now. Wait for it to finish, then try again.")
    return 3


def locked(command):
    """Run a changing subcommand under state_lock. A held lock gives result=busy, exit 3."""

    def run(args):
        try:
            with state_lock():
                return command(args)
        except Busy:
            return _busy()

    run.__name__ = command.__name__
    return run


def _installed():
    """The record and port of a finished install with our agent, or (None, 0)."""
    record = read_record()
    port = record_port(record)
    if not port or not plist_is_ours():
        return None, 0
    return record, port


# ---------------------------------------------------------------------------- configure

def configure(args):
    record, port = _installed()
    if record is None:
        facts(result="not_installed", note="Run the context-guru-setup skill.")
        return 2
    try:
        current = read_preset(port)
    except RuntimeError as error:
        facts(result="refused", reason="unmanaged_config", detail=error)
        return 2
    if args.show:
        facts(result="ok", preset=current, cache_strategy="none",
              allowed_presets=",".join(BOB_PRESETS),
              allowed_cache_strategies=",".join(BOB_CACHE_STRATEGIES),
              cache_note=NO_KEEPALIVE_REASON)
        return 0
    if args.preset and args.preset not in BOB_PRESETS:
        facts(result="refused", reason="preset_not_allowed_for_bob", preset=args.preset,
              detail=preset_refusal(args.preset))
        return 2
    if args.cache_strategy and args.cache_strategy not in BOB_CACHE_STRATEGIES:
        facts(result="refused", reason="cache_strategy_not_allowed_for_bob",
              cache_strategy=args.cache_strategy, detail=NO_KEEPALIVE_REASON)
        return 2
    if not args.preset and current == "custom":
        facts(result="unchanged", preset=current, cache_strategy="none",
              preset_note=f"{proxy_config(port)} was edited by hand. It is kept as it is. "
                          "Name a preset to replace it.")
        return 0
    preset = args.preset or (current if current in BOB_PRESETS else DEFAULT_PRESET)
    if preset == current:
        facts(result="unchanged", preset=preset, cache_strategy="none")
        return 0
    outcome = change_and_restart(port, lambda: write_proxy_config(port, preset),
                                 [proxy_config(port)])
    ok = outcome["proxy_restarted"] == "true"
    facts(result="configured" if ok else "restart_failed", preset=preset if ok else current,
          cache_strategy="none", **outcome)
    return 0 if ok else 1


# ---------------------------------------------------------------------------- update

def update(args):
    record, port = _installed()
    if record is None:
        facts(result="not_installed", note="Run the context-guru-setup skill.")
        return 2
    executable = record.get("binary") or ""
    installed = binary_version(executable) if executable else "unknown"
    try:
        latest = latest_release_tag()
    except Exception as error:
        facts(result="error", reason="release_check_failed", installed_version=installed,
              detail=error)
        return 1
    try:
        release_version(latest)
    except RuntimeError as error:
        facts(result="error", reason="release_check_failed", detail=error)
        return 1
    order = compare_versions(installed, latest)
    note = None
    if order is None:
        available = True
        note = "The installed version is unknown, so the latest release counts as newer."
    elif order > 0:
        available = bool(args.allow_downgrade)
        note = (f"The installed {installed} is newer than the latest release {latest}. "
                "Use --allow-downgrade to install the older one.")
    else:
        available = order < 0
    if args.check:
        facts(result="checked", installed_version=installed, latest_version=latest,
              update_available=str(available).lower(), **({"note": note} if note else {}))
        return 0
    managed = managed_binary()
    if Path(executable).resolve() != managed.resolve():
        facts(result="refused", reason="binary_not_managed", binary=executable,
              detail="This proxy is not the plugin's own copy. Update it the way you installed "
                     "it (for example brew), then restart the agent.")
        return 2
    if not available:
        facts(result="up_to_date", version=installed, **({"note": note} if note else {}))
        return 0
    staging = managed.with_name("context-guru-proxy.new")
    try:
        _, version = install_release_binary(latest, destination=staging)
        problem = binary_problem(str(staging))
        if problem:
            raise RuntimeError(problem)
    except Exception as error:
        staging.unlink(missing_ok=True)
        facts(result="update_failed", reason="download_failed", detail=error,
              installed_version=installed)
        return 1
    previous = managed.with_name("context-guru-proxy.previous")

    def swap():
        shutil.copy2(managed, previous)  # kept, so a later failure can go back to it
        os.replace(staging, managed)

    outcome = change_and_restart(port, swap, [managed])
    staging.unlink(missing_ok=True)
    ok = outcome["proxy_restarted"] == "true"
    facts(result="updated" if ok else "update_failed", version=version if ok else installed,
          **outcome)
    return 0 if ok else 1


# ---------------------------------------------------------------------------- doctor

BOB_APPS = (Path.home() / "Applications/IBM Bob.app", Path("/Applications/IBM Bob.app"))
BUNDLE_JS = "Contents/Resources/app/extensions/bob-code/dist/extension.js"
# Strings in Bob's bundle that this plugin and the proxy depend on (plan 10.1). A missing
# string in REQUIRED means routing cannot work: fail. In OPTIONAL it means one feature may stop.
# A check passes when any one of its spellings is found; a spelling is a tuple of strings that
# must all be there.
BUNDLE_REQUIRED = {
    "VITE_GATEWAY_BASE_URL": "B1: Bob reads its backend address from this variable",
    "region_domain": "B2: the proxy strips this field so model calls come back to it",
    "/inference/v1/chat/completions": "B3: the model call path the proxy reduces",
}
# Bob 2.3.0 builds the model path from two parts far apart in the bundle (131 KB in 2.3.0):
# a constant m3e="/inference/v1", and a request with path:"/chat/completions". So a "same
# window" rule would fail on the real bundle. Each part must instead appear in the code shape
# that uses it, so a stray "/chat/completions" in a text does not pass.
BUNDLE_SPELLINGS = {
    "/inference/v1/chat/completions": (
        (re.compile(re.escape('"/inference/v1/chat/completions"')),),
        (re.compile(r'=\s*"/inference/v1"'), re.compile(r'path\s*:\s*"/chat/completions"')),
    ),
}


def _in_bundle(name, text):
    spellings = BUNDLE_SPELLINGS.get(name)
    if spellings is None:
        return name in text
    return any(all(pattern.search(text) for pattern in spelling) for spelling in spellings)
BUNDLE_OPTIONAL = {
    "X-Task-Id": "B4: the proxy reads the task id from this header",
    **{event: "B6: a hook event" for event in (
        "SessionStart", "UserPromptSubmit", "PreCompact", "PostCompact", "PreToolUse",
        "PostToolUse", "PermissionRequest", "PermissionResolved", "Stop")},
    "Tool does not exist": "B8: the reply the expand repair relies on",
}


def bob_app():
    override = os.environ.get("CONTEXT_GURU_BOB_APP")
    if override:
        return Path(override)
    return next((app for app in BOB_APPS if app.exists()), BOB_APPS[0])


def bob_version(app):
    try:
        with open(app / "Contents/Info.plist", "rb") as handle:
            return str(plistlib.load(handle).get("CFBundleShortVersionString") or "unknown")
    except (OSError, ValueError, plistlib.InvalidFileException):
        return None


def _bundle_check(app, version):
    if not app.exists():
        return "warn", f"Bob is not installed at {app}."
    try:
        text = (app / BUNDLE_JS).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return "fail", (f"Bob's layout changed; context-guru may not work with Bob {version}. "
                        f"{BUNDLE_JS} is not in {app}. Tell the context-guru team the Bob "
                        f"version. If Bob does not work, run {reset_path()} --yes.")
    gone = [name for name in BUNDLE_REQUIRED if not _in_bundle(name, text)]
    if gone:
        why = "; ".join(f"{name} ({BUNDLE_REQUIRED[name]})" for name in gone)
        return "fail", (f"Bob {version} no longer has: {why}. This Bob release may not work "
                        f"with context-guru. Run {reset_path()} --yes so Bob talks to IBM "
                        "directly, and tell the context-guru team the Bob version.")
    gone = [name for name in BUNDLE_OPTIONAL if not _in_bundle(name, text)]
    if gone:
        why = "; ".join(f"{name} ({BUNDLE_OPTIONAL[name]})" for name in gone)
        return "warn", (f"Bob {version} no longer has: {why}. Routing still works. Tell the "
                        "context-guru team the Bob version.")
    return "ok", None


def doctor(args):
    """Read-only checks. One `name=ok|warn|fail` line each, and a `fix=` line when not ok.

    --probe also starts the user's login shell the way Bob does. That runs the user's startup
    files, which can do anything (update a framework, use the network), so it is opt-in.
    """
    results = []

    def report(name, state, fix=None):
        results.append(state)
        facts(**{name: state})
        if fix:
            facts(fix=fix)

    record = read_record()
    port = record_port(record)
    if not port:
        report("install", "fail", "No install found. Run the context-guru-setup skill.")
    else:
        report("install", "ok")
        up = healthy(port, timeout=2)
        report("proxy", "ok" if up else "fail", None if up else (
            f"Nothing answers on 127.0.0.1:{port}. Run: launchctl kickstart -k "
            f"gui/$(id -u)/{LABEL} . See {state_dir() / 'proxy.log'}."))
        if not plist_is_ours():
            report("service", "fail", "Our launchd agent is missing. Run the setup skill again.")
        elif launchctl("print", f"{gui_domain()}/{LABEL}", timeout=3) != 0:
            report("service", "fail", f"The agent is not loaded. Run: launchctl bootstrap "
                                      f"gui/$(id -u) {launchd_plist()}")
        else:
            try:
                with open(launchd_plist(), "rb") as handle:
                    arguments = plistlib.load(handle).get("ProgramArguments") or []
            except (OSError, ValueError, plistlib.InvalidFileException):
                arguments = []
            config = (arguments[arguments.index("--config") + 1]
                      if "--config" in arguments[:-1] else None)
            if f"127.0.0.1:{port}" not in arguments:
                report("service", "fail", "The agent listens on another port than the record. "
                                          "Run the setup skill again.")
            elif config != str(proxy_config(port)):
                report("service", "fail", f"The agent's --config is {config or '(none)'}, not "
                                          f"{proxy_config(port)}. Run the setup skill again.")
            else:
                report("service", "ok")
        rc_file = record.get("rc_file")
        if not rc_file:
            report("rc_block", "warn", "Setup did not write a startup file. Add the block by "
                                       "hand (see the README).")
        else:
            try:
                state, found = bob_route.ownership(Path(rc_file).read_bytes())
            except OSError:
                state, found = "absent", None
            if state == "ours" and found == port and not getattr(args, "probe", False):
                report("rc_block", "ok")
                facts(rc_probe="skipped", rc_probe_hint=(
                    "Run doctor --probe to start your login shell the way Bob does and check "
                    "that it sets the address. That runs your shell startup files."))
            elif state == "ours" and found == port:
                report("rc_block", "ok")
                seen = bob_route.probe(Path.home(), os.environ.get("SHELL", ""))
                url = f"http://127.0.0.1:{port}"
                good = seen["ide"] == url and seen["terminal_ide"] == ""
                report("rc_probe", "ok" if good else "fail", None if good else (
                    f"A login shell started the way Bob starts it does not set "
                    f"{bob_route.IDE_VAR}={url}. Check that your login shell reads {rc_file}."))
            else:
                report("rc_block", "fail", (
                    f"{rc_file} has no context-guru block for port {port} that is exactly "
                    f"ours (found: {state}). Run the setup skill again, or fix it by hand."))
        try:
            preset = read_preset(port)
        except RuntimeError:
            preset = "unmanaged"
        facts(preset_name=preset)
        if preset in BOB_PRESETS:
            report("preset", "ok")
        else:
            report("preset", "warn", (
                f"{proxy_config(port)} is not one of the Bob presets ({', '.join(BOB_PRESETS)}). "
                "It may run a lossy component, and Bob cannot get dropped content back yet. "
                "Run: configure --preset lossless"))
        try:
            hooks = bob_hooks.is_installed(bob_settings())
        except Exception:
            hooks = False
        report("hooks", "ok" if hooks else "warn", None if hooks else (
            f"Our hooks are not in {bob_settings()}. Bob still works; run the setup skill to "
            "add them."))
        executable = record.get("binary") or ""
        problem = binary_problem(executable) if executable else "no binary in the record"
        report("binary", "ok" if problem is None else "fail", problem and (
            f"{problem}. Run the context-guru-update skill, or the setup skill."))
        if up:
            try:
                ts = last_bob_request_ms(port)
                age = None if ts is None else max(0, int(time.time() - ts / 1000))
            except Exception:
                age = None
            recent = age is not None and age <= RECENT_TRAFFIC_S
            report("bob_traffic", "ok" if recent else "warn", None if recent else (
                "No Bob request reached the proxy in the last 24 hours. If Bob is open, quit "
                "it and open it from the Dock."))
        if record.get("statusbar") is True:
            listed = statusbar_installed()
            report("statusbar", "ok" if listed else "warn", None if listed else (
                f"The status bar extension {STATUSBAR_ID} is not in Bob"
                f"{'' if listed is False else ' (bobide did not answer)'}. Bob still works. "
                "Rebuild it with make bob-statusbar in the context-guru repo, then run setup "
                "again."))
        else:
            facts(statusbar="not_installed")
    app = bob_app()
    version = bob_version(app)
    facts(bob_app=app, bob_version=version or "(not found)")
    state, fix = _bundle_check(app, version or "unknown")
    report("bob_bundle", state, fix)
    facts(result="fail" if "fail" in results else "warn" if "warn" in results else "ok")
    return 1 if "fail" in results else 0


# ---------------------------------------------------------------------------- status

def last_bob_request_ms(port):
    """The time (epoch ms) of the newest Bob request in the dashboard, or None."""
    url = f"http://127.0.0.1:{port}/api/requests?limit=1&agent=bob"
    with LOOPBACK.open(url, timeout=2) as reply:
        rows = json.load(reply).get("requests") or []
    return rows[0].get("ts") if rows else None


def status(_args):
    record = read_record()
    port = record_port(record)
    if not port:
        facts(result="not_installed", note="Run the context-guru-setup skill.")
        return 1
    rc_file = record.get("rc_file")
    routed = bool(rc_file) and bob_route.is_routed(rc_file, port)
    hooks = bob_hooks.is_installed(bob_settings())
    up = healthy(port, timeout=2)
    values = {"port": port, "rc_file": rc_file or "(none)", "routed": str(routed).lower(),
              "hooks_installed": str(hooks).lower(), "proxy_up": str(up).lower(),
              "dashboard": f"http://127.0.0.1:{port}/dashboard/"}
    age = None
    if up:
        try:
            ts = last_bob_request_ms(port)
            if isinstance(ts, (int, float)):
                age = max(0, int(time.time() - ts / 1000))
        except Exception as error:
            values["traffic_error"] = type(error).__name__
    values["last_bob_request_age_s"] = "(none)" if age is None else age
    if record.get("complete") is False:
        values["result"] = "setup_incomplete"
        values["hint"] = (f"Setup did not finish. Run the context-guru-setup skill again, or "
                          f"{reset_path()} --yes.")
    elif not (routed and up):
        values["result"] = "not_ready"
    elif age is None or age > RECENT_TRAFFIC_S:
        values["result"] = "no_bob_traffic"
        values["hint"] = ("No Bob request reached the proxy recently. If Bob is open, quit it and "
                          "open it from the Dock, so it reads the startup file again.")
    else:
        values["result"] = "ok"
    values["note"] = LOGIN_NOTE + " (after setup and after uninstall)."
    facts(**values)
    return 0 if values["result"] == "ok" else 1


# ---------------------------------------------------------------------------- hook handlers

def ensure(_args):
    """SessionStart: if the proxy is down, ask launchd to start our agent. Silent, exit 0."""
    try:
        port = record_port(read_record())
        if port and not healthy(port) and plist_is_ours():
            # kickstart fails when the agent is not loaded (someone ran bootout). Load it then.
            # Bob kills the hook after 5 s, so each call gets 1.5 s at most.
            if launchctl("kickstart", f"{gui_domain()}/{LABEL}", timeout=1.5) != 0:
                launchctl("bootstrap", gui_domain(), str(launchd_plist()), timeout=1.5)
    except Exception:
        pass
    return 0


def check(_args):
    """UserPromptSubmit: one short line when the proxy is down. Never reads stdin. Exit 0."""
    try:
        port = record_port(read_record())
        if port and not healthy(port):
            print(f"context-guru: the local proxy at 127.0.0.1:{port} is not answering, so Bob "
                  f"cannot reach its backend. Fix: run {reset_path()} --yes in a terminal, then "
                  "quit and reopen Bob and log in.")
    except Exception:
        pass
    return 0


_SESSION_ID = re.compile(r"[A-Za-z0-9._:-]{1,128}")
_SOURCES = {"startup", "resume", "compact"}


def signal_hook(_args):
    """SessionStart: tell the proxy about the session. Silent, exit 0.

    Of the payload we send only the three fields /signal reads (session_id, hook_event_name,
    source), under the names it accepts. Bob has no SessionEnd, and its other events carry model
    text (Stop, PostCompact) or the prompt, so we send nothing for them.
    """
    try:
        payload = json.load(sys.stdin)
        port = record_port(read_record())
        if not port or not isinstance(payload, dict):
            return 0
        session = payload.get("session_id")
        source = payload.get("source")
        if payload.get("hook_event_name") != "SessionStart" or source not in _SOURCES or \
                not isinstance(session, str) or not _SESSION_ID.fullmatch(session):
            return 0
        body = json.dumps({"session_id": session, "hook_event_name": "SessionStart",
                           "source": source}).encode()
        request = urllib.request.Request(f"http://127.0.0.1:{port}/signal", data=body,
                                         method="POST",
                                         headers={"Content-Type": "application/json"})
        LOOPBACK.open(request, timeout=HOOK_HTTP_TIMEOUT_S).close()
    except Exception:
        pass
    return 0


# ---------------------------------------------------------------------------- uninstall

def _step(name, action, refused):
    """Run one uninstall step and report it. Return True when it left nothing of ours behind."""
    try:
        facts(**{name: action()})
        return True
    except refused as error:
        facts(**{name: "left_alone", f"{name}_detail": error})
    except Exception as error:
        facts(**{name: "failed", f"{name}_detail": f"{type(error).__name__}: {error}"})
    return False


def _find_our_rc_file():
    """The first startup file for $SHELL that holds our exact block, or None."""
    for candidate in bob_route.candidates(Path.home(), os.environ.get("SHELL", "")):
        found, _ = bob_route.follow(candidate)
        try:
            if found and found.is_file() and bob_route.ownership(found.read_bytes())[0] == "ours":
                return str(found)
        except OSError:
            continue
    return None


def uninstall(args):
    record = read_record()
    rc_file = record.get("rc_file")
    if not rc_file:
        # No record (a setup killed before it wrote one): look where setup could have written.
        rc_file = _find_our_rc_file()
    port = record_port(record)
    facts(result="planned" if args.dry_run else "removing", rc_file=rc_file or "(none)",
          settings=bob_settings(), service_file=launchd_plist(), state=state_dir(),
          purge=str(args.purge).lower())
    if args.dry_run:
        return 0
    clean = _step("hooks", lambda: bob_hooks.remove(bob_settings(), hooks_state()),
                  bob_hooks.HooksRefused)
    if rc_file:
        clean = _step("rc_block", lambda: bob_route.remove(
            rc_file, created=bool(record.get("rc_created"))), bob_route.RouteRefused) and clean
    if record.get("statusbar") is True:
        # Not part of `clean`: a left-over status bar item only shows `CG –`. It does not route.
        _step("statusbar", remove_statusbar, StatusbarRefused)
    try:
        service = stop_service()
    except Exception as error:
        service = "failed"
        facts(service_detail=f"{type(error).__name__}: {error}")
    facts(service=service)
    clean = clean and service in ("stopped", "absent")
    if port:
        config = proxy_config(port)
        try:
            if config.exists() and config.read_text().startswith(CONFIG_MARKER):
                config.unlink()
        except OSError:
            pass
    if clean:
        record_path().unlink(missing_ok=True)
        if args.purge:
            shutil.rmtree(state_dir(), ignore_errors=True)
            facts(data="removed")
        else:
            facts(data="kept", data_dir=state_dir())
    facts(result="removed" if clean else "partly_removed")
    print("Next: quit and reopen Bob, then log in once. " + LOGIN_NOTE + ".")
    return 0 if clean else 1


# ---------------------------------------------------------------------------- main

def main():
    parser = argparse.ArgumentParser(prog="bob_plugin.py")
    commands = parser.add_subparsers(dest="command", required=True)
    install = commands.add_parser("setup")
    install.add_argument("--plan", action="store_true")
    install.add_argument("--i-consent-to-traffic-interception", action="store_true")
    install.add_argument("--upstream", default=DEFAULT_UPSTREAM)
    commands.add_parser("status")
    commands.add_parser("ensure")
    commands.add_parser("check")
    commands.add_parser("signal")
    settings = commands.add_parser("configure")
    settings.add_argument("--show", action="store_true")
    # No `choices`: a name outside the allow-list gets the plain refusal, not an argparse error.
    settings.add_argument("--preset")
    settings.add_argument("--cache-strategy")
    upgrade = commands.add_parser("update")
    upgrade.add_argument("--check", action="store_true")
    upgrade.add_argument("--allow-downgrade", action="store_true")
    diagnose = commands.add_parser("doctor")
    diagnose.add_argument("--probe", action="store_true")
    remove = commands.add_parser("uninstall")
    remove.add_argument("--dry-run", action="store_true")
    remove.add_argument("--purge", action="store_true")
    args = parser.parse_args()
    command = {"setup": setup, "status": status, "ensure": ensure, "check": check,
               "signal": signal_hook, "configure": configure, "update": update,
               "doctor": doctor, "uninstall": uninstall}[args.command]
    # A run that changes files takes the state lock. --plan, --show, --check and --dry-run
    # change nothing, so they run without it.
    reads_only = any(getattr(args, name, False) for name in ("plan", "show", "check", "dry_run"))
    # setup takes the lock itself, after its refusals, so a refused setup writes nothing.
    if args.command in ("configure", "update", "uninstall") and not reads_only:
        command = locked(command)
    return command(args)



if __name__ == "__main__":
    sys.exit(main())
