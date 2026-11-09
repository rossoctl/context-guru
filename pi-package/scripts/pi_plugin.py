#!/usr/bin/env python3
"""Install and operate context-guru for Pi.

The Pi extension calls this script for every action that writes or starts something. Output is one
`key=value` fact per line, so the extension (and a person in a shell) can read each fact on its own.

Design, in short:
  * Routing is one key per provider in Pi's models.json (`providers.<name>.baseUrl`). Nothing is
    checked per session: Pi reads the file, and the request goes to the proxy.
  * The proxy runs as a user service (launchd on macOS, systemd --user on Linux), so it is up before
    Pi starts and restarts by itself after a crash or a login. No Pi session starts or checks it.
  * Every file it writes carries an ownership marker or a routing record, and uninstall and the reset
    hatch remove only what they own.
"""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import plistlib
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
import models_route  # noqa: E402

MARKER = "# Managed by the context-guru Pi package."
RELEASE_REPO = "rossoctl/context-guru"
MINIMUM_RELEASE = (0, 4, 0)
SERVICE = "context-guru-pi"
LAUNCHD_LABEL = "io.rossoctl.context-guru-pi"
# Pi's built-in providers this package routes, and where each one forwards. `openai-codex` (ChatGPT
# login through chatgpt.com/backend-api) is not here: the proxy has no route for that backend.
PROVIDERS = ("anthropic", "openai")
UPSTREAMS = {"anthropic": "api.anthropic.com", "openai": "api.openai.com"}
NAMES = {"anthropic": "Anthropic", "openai": "OpenAI"}
PRESETS = ("off", "conservative", "medium", "high", "xhigh", "ultrahigh")
DEFAULT_PRESET = "off"
DEFAULT_STRATEGY = "auto"

# The same strategies, keys and values as the Claude Code plugin (context-guru-plugin/scripts/settings.py),
# so one name means one behaviour on both clients. `none` writes no cache keys at all.
#
# The same keys also drive the proxy's OpenAI keep-alive; no OpenAI-only key is needed. On an OpenAI
# Responses request the proxy uses `keepalive`, `keepalive_max_pings`, `keepalive_max_usd_per_ping` and
# `keepalive_min_prefix_tokens`, and replaces the idle time with its OpenAI default (28 minutes, under
# the 30-minute cache of GPT-5.6 and later). The `_1h` keys are Anthropic only. Earlier OpenAI models
# get no pings. The Codex plugin writes `keepalive: true` alone, which gives the same OpenAI behaviour.
STRATEGIES = {
    "none": {"cache": {}, "spends": False,
             "desc": "no keep-alive pings. Pi's own cache warming runs as it would without context-guru."},
    "auto": {"cache": {"keepalive": True, "keepalive_idle_seconds": 240, "keepalive_max_pings": 2,
                       "keepalive_max_usd_per_ping": 0.50, "keepalive_min_prefix_tokens": 80000,
                       "keepalive_idle_seconds_1h": 3540, "keepalive_max_pings_1h": 1},
             "spends": True,
             "desc": "idle pings that follow the cache lifetime of each session. Anthropic 5-minute cache: "
                     "first ping at 240 s, at most 2. Anthropic 1-hour cache: 1 ping at 59 minutes. "
                     "OpenAI (GPT-5.6 and later): a ping at 28 minutes, at most 2. Uses your quota."},
    "1-ping": {"cache": {"keepalive": True, "keepalive_idle_seconds": 240, "keepalive_max_pings": 1,
                         "keepalive_max_usd_per_ping": 0.50, "keepalive_min_prefix_tokens": 80000,
                         "keepalive_idle_seconds_1h": 3540, "keepalive_max_pings_1h": 1},
               "spends": True, "desc": "at most 1 idle ping per idle gap, on Anthropic and on OpenAI. "
                                       "Uses your quota."},
    "2-ping": {"cache": {"keepalive": True, "keepalive_idle_seconds": 240, "keepalive_max_pings": 2,
                         "keepalive_max_usd_per_ping": 0.50, "keepalive_min_prefix_tokens": 80000,
                         "keepalive_idle_seconds_1h": 3540, "keepalive_max_pings_1h": 2},
               "spends": True, "desc": "at most 2 idle pings per idle gap, on both Anthropic cache lifetimes "
                                       "and on OpenAI. Uses your quota."},
}


# ---------------------------------------------------------------------------------------------- paths

def agent_dir():
    return Path(os.environ.get("PI_CODING_AGENT_DIR") or Path.home() / ".pi/agent")


def models_path():
    return agent_dir() / "models.json"


def state_dir():
    base = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state")
    return base / "context-guru-pi"


def record_path():
    return state_dir() / "install.json"


def routing_state():
    return state_dir() / "routing.json"


def proxy_config():
    return state_dir() / "proxy.yaml"


def managed_binary():
    return state_dir() / "bin/context-guru-proxy"


def reset_hatch():
    return state_dir() / "context-guru-reset"


def systemd_unit():
    base = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    return base / f"systemd/user/{SERVICE}.service"


def launchd_plist():
    return Path.home() / f"Library/LaunchAgents/{LAUNCHD_LABEL}.plist"


def service_kind():
    return {"Linux": "systemd", "Darwin": "launchd"}.get(platform.system())


# -------------------------------------------------------------------------------------------- helpers

def facts(**values):
    for key, value in values.items():
        if isinstance(value, (dict, list)):
            value = json.dumps(value, separators=(",", ":"))
        # One fact per line: an error text with a newline must not start a second, false fact.
        print(f"{key}=" + " ".join(str(value).split("\n")).replace("\r", " "))


def healthy(port):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as response:
            return response.status == 200
    except Exception:
        return False


def first_free_port():
    for port in range(8787, 8888):
        with socket.socket() as sock:
            try:
                sock.bind(("127.0.0.1", port))
                return port
            except OSError:
                pass
    raise RuntimeError("no free port in 8787..8887")


def port_free(port):
    with socket.socket() as sock:
        try:
            sock.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def choose_port(record):
    """The recorded port while it is ours to use (our proxy answers there, or nothing holds it).
    Another program on it means a new port, so setup does not fail on every retry."""
    port = int(record.get("port") or 0)
    if port and (healthy(port) or port_free(port)):
        return port
    return first_free_port()


def read_record():
    try:
        return json.loads(record_path().read_text())
    except (OSError, ValueError):
        return {}


def write_record(record):
    path = record_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(record, indent=2) + "\n")
    temporary.chmod(0o600)
    os.replace(temporary, path)


def owned(path):
    try:
        return path.read_text().startswith(MARKER)
    except OSError:
        return False


# --------------------------------------------------------------------------------------------- binary

def find_binary():
    found = shutil.which(os.environ.get("CONTEXT_GURU_BIN", "context-guru-proxy"))
    if found:
        return str(Path(found).resolve())
    if managed_binary().is_file() and os.access(managed_binary(), os.X_OK):
        return str(managed_binary().resolve())
    return None


def release_platform():
    systems = {"Darwin": "darwin", "Linux": "linux"}
    machines = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}
    try:
        return systems[platform.system()], machines[platform.machine()]
    except KeyError as error:
        raise RuntimeError(f"unsupported release platform: {platform.system()}/{platform.machine()}") from error


def latest_release_tag():
    request = urllib.request.Request(f"https://github.com/{RELEASE_REPO}/releases/latest", method="HEAD")
    with urllib.request.urlopen(request, timeout=15) as response:
        tag = response.geturl().rstrip("/").rsplit("/", 1)[-1]
    if not tag or tag == "latest":
        raise RuntimeError("could not resolve the latest context-guru release")
    return tag


def fetch(url):
    with urllib.request.urlopen(url, timeout=60) as response:
        return response.read()


def release_version(tag):
    value = tag.removeprefix("v").split("-", 1)[0]
    try:
        parts = tuple(int(part) for part in value.split("."))
    except ValueError as error:
        raise RuntimeError(f"unrecognized context-guru release tag: {tag}") from error
    if len(parts) != 3:
        raise RuntimeError(f"unrecognized context-guru release tag: {tag}")
    return parts


def install_release_binary(version=None):
    """Download the release for this platform, check it against the release's SHA-256 list, and
    install it into the private state directory. A checksum problem is a hard stop: the binary
    carries all of the user's model traffic."""
    version = version or os.environ.get("CONTEXT_GURU_VERSION") or latest_release_tag()
    if release_version(version) < MINIMUM_RELEASE:
        raise RuntimeError(f"release {version} is too old; v0.4.0 or newer is required")
    os_name, arch = release_platform()
    archive_name = f"context-guru_{version.removeprefix('v')}_{os_name}_{arch}.tar.gz"
    base = f"https://github.com/{RELEASE_REPO}/releases/download/{version}"
    archive = fetch(f"{base}/{archive_name}")
    checksums = fetch(f"{base}/checksums.txt").decode("utf-8")
    expected = None
    for line in checksums.splitlines():
        fields = line.split()
        if len(fields) >= 2 and fields[-1].lstrip("*") == archive_name:
            expected = fields[0].lower()
            break
    if not expected:
        raise RuntimeError(f"checksum_absent: the release checksums do not list {archive_name}")
    if hashlib.sha256(archive).hexdigest() != expected:
        raise RuntimeError(f"checksum_mismatch for {archive_name}")
    destination = managed_binary()
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
        matches = [m for m in bundle.getmembers() if m.isfile() and Path(m.name).name == "context-guru-proxy"]
        if len(matches) != 1:
            raise RuntimeError("release archive does not contain exactly one context-guru-proxy")
        source = bundle.extractfile(matches[0])
        if source is None:
            raise RuntimeError("could not read context-guru-proxy from the release archive")
        fd, temporary_name = tempfile.mkstemp(prefix="context-guru-proxy.", dir=destination.parent)
        temporary = Path(temporary_name)
        try:
            with os.fdopen(fd, "wb") as output:
                shutil.copyfileobj(source, output)
            temporary.chmod(0o755)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)
    return str(destination.resolve()), version


# --------------------------------------------------------------------------------------- proxy config

def render_config(preset, strategy):
    lines = [MARKER, f"# strategy={strategy}. Change it with /context-guru preset or /context-guru cache-strategy.",
             f"preset: {preset}"]
    cache = STRATEGIES[strategy]["cache"]
    if cache:
        lines.append("cache:")
        for key, value in cache.items():
            lines.append(f"  {key}: {str(value).lower() if isinstance(value, bool) else value}")
    return "\n".join(lines) + "\n"


def write_proxy_config(preset, strategy):
    if preset not in PRESETS:
        raise RuntimeError(f"unknown preset: {preset}")
    if strategy not in STRATEGIES:
        raise RuntimeError(f"unknown cache strategy: {strategy}")
    path = proxy_config()
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() and not owned(path):
        raise RuntimeError(f"refusing to overwrite a proxy config we did not write: {path}")
    temporary = path.with_suffix(".tmp")
    temporary.write_text(render_config(preset, strategy))
    temporary.chmod(0o600)
    os.replace(temporary, path)
    return path


def proxy_command(executable, port):
    return [executable, "--listen", f"127.0.0.1:{port}", "--config", str(proxy_config()),
            "--dashboard", "--dashboard-db", str(state_dir() / "dashboard.db")]


# -------------------------------------------------------------------------------------------- service

def _systemd_quote(value):
    return '"' + str(value).replace("\\", "\\\\").replace('"', '\\"') + '"'


def start_service(executable, port):
    """Install and start the user service. Returns True once /healthz answers."""
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    command = proxy_command(executable, port)
    log = root / "proxy.log"
    kind = service_kind()
    if kind == "systemd":
        unit = systemd_unit()
        unit.parent.mkdir(parents=True, exist_ok=True)
        if unit.exists() and not owned(unit):
            return False
        unit.write_text(
            f"{MARKER}\n[Unit]\nDescription=context-guru for Pi\n\n[Service]\nType=simple\n"
            f"ExecStart={' '.join(_systemd_quote(item) for item in command)}\n"
            "Restart=on-failure\nRestartSec=1\n"
            f"StandardOutput={_systemd_quote('append:' + str(log))}\n"
            f"StandardError={_systemd_quote('append:' + str(log))}\n\n"
            "[Install]\nWantedBy=default.target\n")
        unit.chmod(0o600)
        try:
            subprocess.run(["systemctl", "--user", "daemon-reload"], check=True, capture_output=True)
            subprocess.run(["systemctl", "--user", "enable", "--now", unit.name], check=True, capture_output=True)
            subprocess.run(["systemctl", "--user", "restart", unit.name], check=True, capture_output=True)
        except (OSError, subprocess.CalledProcessError):
            return False
    elif kind == "launchd":
        plist = launchd_plist()
        plist.parent.mkdir(parents=True, exist_ok=True)
        if plist.exists():
            try:
                with open(plist, "rb") as handle:
                    if plistlib.load(handle).get("ContextGuruManaged") is not True:
                        return False
            except (OSError, ValueError, plistlib.InvalidFileException):
                return False
        with open(plist, "wb") as handle:
            plistlib.dump({"Label": LAUNCHD_LABEL, "ContextGuruManaged": True, "ProgramArguments": command,
                           "RunAtLoad": True, "KeepAlive": True, "ProcessType": "Background",
                           "StandardOutPath": str(log), "StandardErrorPath": str(log)}, handle)
        plist.chmod(0o600)
        domain = f"gui/{os.getuid()}"
        subprocess.run(["launchctl", "bootout", f"{domain}/{LAUNCHD_LABEL}"], capture_output=True)
        try:
            subprocess.run(["launchctl", "bootstrap", domain, str(plist)], check=True, capture_output=True)
            subprocess.run(["launchctl", "kickstart", "-k", f"{domain}/{LAUNCHD_LABEL}"], check=True,
                           capture_output=True)
        except (OSError, subprocess.CalledProcessError):
            return False
    else:
        return False
    for _ in range(50):
        if healthy(port):
            return True
        time.sleep(0.1)
    return False


def stop_service():
    """Stop and remove the user service. Returns stopped | gone | not_owned."""
    unit, plist = systemd_unit(), launchd_plist()
    if unit.exists():
        if not owned(unit):
            return "not_owned"
        subprocess.run(["systemctl", "--user", "disable", "--now", unit.name], capture_output=True)
        unit.unlink()
        subprocess.run(["systemctl", "--user", "daemon-reload"], capture_output=True)
        return "stopped"
    if plist.exists():
        try:
            with open(plist, "rb") as handle:
                if plistlib.load(handle).get("ContextGuruManaged") is not True:
                    return "not_owned"
        except (OSError, ValueError, plistlib.InvalidFileException):
            return "not_owned"
        subprocess.run(["launchctl", "bootout", f"gui/{os.getuid()}/{LAUNCHD_LABEL}"], capture_output=True)
        plist.unlink()
        return "stopped"
    return "gone"


def install_escape_hatch():
    """Copy the reset script and the route helper OUT of the package directory. If routing breaks,
    Pi cannot run a command, and a package update could move the package itself."""
    here = Path(__file__).resolve().parent
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    for name, target in (("reset.sh", reset_hatch()), ("models_route.py", root / "models_route.py")):
        shutil.copyfile(here / name, target)
        target.chmod(0o700)
    return reset_hatch()


# ------------------------------------------------------------------------------------------- commands

def consent_question(port, strategy):
    routes = "; ".join(f"{NAMES[p]} at {models_route.base_url(port, p)}, forwarding to {UPSTREAMS[p]}"
                       for p in PROVIDERS)
    question = (f"Route Pi's {' and '.join(NAMES[p] for p in PROVIDERS)} models through a local "
                f"context-guru proxy: {routes}. This changes {models_path()} for every Pi session on "
                f"this machine, and installs the proxy as a user service that starts at login.")
    if STRATEGIES[strategy]["spends"]:
        question += (f" Cache strategy {strategy} sends small keep-alive requests while you are idle, "
                     "using your own quota on each provider: Anthropic after 4 minutes idle (59 "
                     "minutes on a 1-hour cache), OpenAI GPT-5.6 and later after 28 minutes idle. "
                     "Pi's own cache warming stops for routed models.")
    return question


def own_urls(record):
    """The base URLs an earlier setup wrote, on any port. A clash with one of them is our own route."""
    urls = record.get("base_urls")
    return [url for url in urls.values() if isinstance(url, str)] if isinstance(urls, dict) else []


def setup(args):
    record = read_record()
    # Read before the record changes: a recorded port someone else now holds moves the route to a new
    # port, and the old URLs must still count as ours, not as a foreign gateway.
    earlier = own_urls(record)
    port = choose_port(record)
    strategy = args.cache_strategy or record.get("cache_strategy") or DEFAULT_STRATEGY
    preset = record.get("preset") or DEFAULT_PRESET
    if strategy not in STRATEGIES:
        facts(result="refused", reason="unknown_strategy", note=",".join(STRATEGIES))
        return 2
    try:
        clash = models_route.conflicts(models_path(), port, PROVIDERS, earlier)
    except Exception as error:  # an unreadable models.json
        facts(result="error", reason="models_unreadable", models=models_path(), detail=error)
        return 2
    # Only a hatch that exists is named: it undoes a route an earlier install left behind.
    hatch = {"reset_hatch": reset_hatch()} if reset_hatch().exists() else {}
    if args.plan:
        if clash:
            facts(result="needs_decision", reason="base_url_already_set", models=models_path(),
                  existing_base_url=clash, **hatch)
            return 0
        facts(result="planned", models=models_path(), port=port, preset=preset, cache_strategy=strategy,
              service=service_kind() or "unsupported", already_routed=str(
                  all(models_route.is_routed(models_path(), port, p) for p in PROVIDERS)).lower(),
              consent_question=consent_question(port, strategy))
        return 0
    if not args.i_consent_to_traffic_interception:
        facts(result="refused", reason="consent_required")
        return 2
    if clash:
        facts(result="refused", reason="base_url_already_set", existing_base_url=clash, **hatch)
        return 2
    if service_kind() is None:
        facts(result="error", reason="unsupported_platform", detail=platform.system())
        return 2
    try:
        write_proxy_config(preset, strategy)
    except Exception as error:
        facts(result="error", reason="config_conflict", config=proxy_config(), detail=error)
        return 2
    executable = record.get("binary") if record.get("binary") and os.access(record["binary"], os.X_OK) else None
    executable = executable or find_binary()
    if not executable:
        try:
            executable, version = install_release_binary()
            facts(binary="installed", binary_version=version, binary_path=executable)
        except Exception as error:
            facts(result="error", reason="binary_install_failed", detail=error)
            return 2
    # Proxy first, routing second: a route written while nothing answers makes every request fail.
    if not start_service(executable, port):
        facts(result="error", reason="health_check_failed", log=state_dir() / "proxy.log",
              routing_written="false")
        return 1
    record.update(port=port, binary=executable, service=service_kind(), preset=preset, cache_strategy=strategy,
                  base_urls={p: models_route.base_url(port, p) for p in PROVIDERS})
    write_record(record)
    try:
        models_route.install(models_path(), routing_state(), port, PROVIDERS, earlier)
        hatch = install_escape_hatch()
    except Exception as error:
        facts(result="error", reason="routing_write_failed", detail=error, proxy_running="true")
        return 1
    facts(result="routed", port=port, models=models_path(), preset=preset, cache_strategy=strategy,
          dashboard=f"http://127.0.0.1:{port}/dashboard/", reset_hatch=hatch)
    return 0


def status(args):
    record = read_record()
    port = int(record.get("port") or 0)
    if args.stats:
        if not port:
            print("no installed proxy", file=sys.stderr)
            return 1
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/stats", timeout=2) as response:
                body = response.read()
            handle, path = tempfile.mkstemp(prefix=f"context-guru-pi-stats-{port}-", suffix=".json")
            with os.fdopen(handle, "wb") as file:
                file.write(body)
        except Exception as error:
            print(f"status --stats: {error}", file=sys.stderr)
            return 1
        print(path)
        return 0
    if not port:
        facts(result="not_installed")
        return 1
    up = healthy(port)
    routed = {p: models_route.is_routed(models_path(), port, p) for p in PROVIDERS}
    values = {"result": "ok" if up and all(routed.values()) else "not_ready", "port": port,
              "proxy_up": str(up).lower(), "routed": routed, "models": models_path(),
              "preset": record.get("preset", DEFAULT_PRESET),
              "cache_strategy": record.get("cache_strategy", DEFAULT_STRATEGY),
              "dashboard": f"http://127.0.0.1:{port}/dashboard/", "reset_hatch": reset_hatch()}
    if up:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/stats", timeout=2) as response:
                stats = json.load(response)
            values["running_preset"] = stats.get("preset", "unknown")
            values["requests"] = stats.get("requests", 0)
        except Exception as error:
            values["stats_error"] = error
    facts(**values)
    return 0 if values["result"] == "ok" else 1


def configure(args):
    record = read_record()
    if not record.get("port"):
        facts(result="not_installed")
        return 2
    preset = record.get("preset", DEFAULT_PRESET)
    strategy = record.get("cache_strategy", DEFAULT_STRATEGY)
    if args.list:
        facts(presets=",".join(PRESETS), strategies=",".join(STRATEGIES),
              **{f"strategy_{name}": spec["desc"] for name, spec in STRATEGIES.items()})
        return 0
    if args.show:
        facts(result="ok", preset=preset, cache_strategy=strategy)
        return 0
    preset = args.preset or preset
    strategy = args.cache_strategy or strategy
    try:
        write_proxy_config(preset, strategy)
    except Exception as error:
        facts(result="refused", detail=error)
        return 2
    record.update(preset=preset, cache_strategy=strategy)
    write_record(record)
    restarted = start_service(record["binary"], int(record["port"]))
    extra = {}
    if restarted and preset == "ultrahigh":
        extra["probe"] = ask_defer_probe(int(record["port"]))
    facts(result="configured" if restarted else "not_applied", preset=preset, cache_strategy=strategy,
          proxy_restarted=str(restarted).lower(), **extra)
    return 0 if restarted else 1


def ask_defer_probe(port):
    """For ultrahigh: ask the restarted proxy to run tool deferral's provider check at the next
    request, the same way the Claude Code and Codex plugins do. Never raises."""
    request = urllib.request.Request(f"http://127.0.0.1:{port}/tool-defer/probe", data=b"{}", method="POST",
                                     headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=1) as response:
            return "asked" if response.status == 202 else f"status_{response.status}"
    except Exception:
        return "unavailable"


def update(args):
    try:
        latest = latest_release_tag()
    except Exception as error:
        facts(result="error", reason="release_check_failed", detail=error)
        return 1
    record = read_record()
    executable = record.get("binary") or find_binary()
    installed = "unknown"
    if executable:
        try:
            out = subprocess.run([executable, "--version"], text=True, capture_output=True).stdout.split()
            installed = out[1] if len(out) >= 2 else installed
        except OSError:
            pass
    available = installed.removeprefix("v") != latest.removeprefix("v")
    if args.check:
        facts(result="checked", installed_version=installed, latest_version=latest,
              update_available=str(available).lower())
        return 0
    if executable and Path(executable).resolve() != managed_binary().resolve():
        facts(result="refused", reason="binary_not_managed", binary=executable)
        return 2
    try:
        executable, version = install_release_binary(latest)
    except Exception as error:
        facts(result="error", reason="binary_install_failed", detail=error)
        return 1
    if not record.get("port"):
        facts(result="updated", version=version, restart="skipped")
        return 0
    record["binary"] = executable
    write_record(record)
    restarted = start_service(executable, int(record["port"]))
    facts(result="updated", version=version, restart="completed" if restarted else "failed")
    return 0 if restarted else 1


def uninstall(args):
    record = read_record()
    facts(result="planned" if args.dry_run else "removing", models=models_path(),
          port=record.get("port", "(none)"))
    if args.dry_run:
        return 0
    # Routing first, proxy second: the reverse of setup, so no moment has a route with no proxy.
    facts(models_restore=models_route.restore(routing_state()))
    stopped = stop_service()
    facts(service=stopped)
    if owned(proxy_config()):
        proxy_config().unlink()
    if stopped != "not_owned":
        record_path().unlink(missing_ok=True)
        if managed_binary().exists():
            managed_binary().unlink()
            facts(binary_removed="true")
    facts(result="removed", kept=state_dir())
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    commands = parser.add_subparsers(dest="command", required=True)
    install = commands.add_parser("setup")
    install.add_argument("--plan", action="store_true")
    install.add_argument("--cache-strategy", choices=tuple(STRATEGIES))
    install.add_argument("--i-consent-to-traffic-interception", action="store_true")
    status_parser = commands.add_parser("status")
    status_parser.add_argument("--stats", action="store_true")
    settings = commands.add_parser("configure")
    group = settings.add_mutually_exclusive_group()
    group.add_argument("--show", action="store_true")
    group.add_argument("--list", action="store_true")
    settings.add_argument("--preset", choices=PRESETS)
    settings.add_argument("--cache-strategy", choices=tuple(STRATEGIES))
    upgrade = commands.add_parser("update")
    mode = upgrade.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--install", action="store_true")
    remove = commands.add_parser("uninstall")
    remove.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    return {"setup": setup, "status": status, "configure": configure, "update": update,
            "uninstall": uninstall}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
