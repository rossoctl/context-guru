#!/usr/bin/env python3
"""Install and operate context-guru's opt-in Codex routing (profile + launcher)."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import plistlib
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parent))
from config_route import install as install_default_route
from config_route import is_default_routed, is_routed, restore as restore_default_route

MARKER = "# Managed by the context-guru Codex plugin."
CONFIG_MARKER = "# Managed by the context-guru Codex plugin."
RELEASE_REPO = "rossoctl/context-guru"
MINIMUM_CODEX_RELEASE = (0, 4, 0)


def codex_home():
    return Path(os.environ.get("CODEX_HOME", Path.home() / ".codex"))


def state_dir():
    base = Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local/state"))
    return base / "context-guru-codex"


def profile():
    return codex_home() / "context-guru.config.toml"


def main_config():
    return codex_home() / "config.toml"


def routing_state():
    return state_dir() / "routing.json"


def proxy_config():
    return state_dir() / "proxy.yaml"


def managed_binary():
    return state_dir() / "bin/context-guru-proxy"


def remove_managed_binary():
    binary = managed_binary()
    if not binary.exists():
        return False
    binary.unlink()
    try:
        binary.parent.rmdir()
    except OSError:
        pass
    return True


def systemd_unit():
    base = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    return base / "systemd/user/context-guru-codex.service"


def launchd_plist():
    return Path.home() / "Library/LaunchAgents/io.rossoctl.context-guru-codex.plist"


def facts(**values):
    for key, value in values.items():
        if isinstance(value, (dict, list)):
            value = json.dumps(value, separators=(",", ":"))
        print(f"{key}={value}")


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


def read_record():
    try:
        return json.loads((state_dir() / "install.json").read_text())
    except (OSError, ValueError):
        return {}


def find_binary():
    found = shutil.which(os.environ.get("CONTEXT_GURU_BIN", "context-guru-proxy"))
    if found:
        return str(Path(found).resolve())
    if managed_binary().is_file() and os.access(managed_binary(), os.X_OK):
        return str(managed_binary().resolve())
    candidate = Path(__file__).resolve().parents[4] / "bin/context-guru-proxy"
    if candidate.is_file() and os.access(candidate, os.X_OK):
        return str(candidate)
    return None


def release_platform():
    systems = {"Darwin": "darwin", "Linux": "linux"}
    machines = {"x86_64": "amd64", "AMD64": "amd64",
                "arm64": "arm64", "aarch64": "arm64"}
    try:
        return systems[platform.system()], machines[platform.machine()]
    except KeyError as error:
        raise RuntimeError(f"unsupported release platform: {platform.system()}/{platform.machine()}") from error


def latest_release_tag():
    request = urllib.request.Request(
        f"https://github.com/{RELEASE_REPO}/releases/latest", method="HEAD")
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
    version = version or os.environ.get("CONTEXT_GURU_VERSION") or latest_release_tag()
    if release_version(version) < MINIMUM_CODEX_RELEASE:
        raise RuntimeError(
            f"release {version} predates Codex Responses support; v0.4.0 or newer is required")
    os_name, arch = release_platform()
    number = version.removeprefix("v")
    archive_name = f"context-guru_{number}_{os_name}_{arch}.tar.gz"
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
        raise RuntimeError(f"release checksum does not list {archive_name}")
    actual = hashlib.sha256(archive).hexdigest()
    if actual != expected:
        raise RuntimeError(f"release checksum mismatch for {archive_name}")

    destination = managed_binary()
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as bundle:
        matches = [member for member in bundle.getmembers()
                   if member.isfile() and Path(member.name).name == "context-guru-proxy"]
        if len(matches) != 1:
            raise RuntimeError("release archive does not contain exactly one context-guru-proxy")
        source = bundle.extractfile(matches[0])
        if source is None:
            raise RuntimeError("could not read context-guru-proxy from release archive")
        fd, temporary_name = tempfile.mkstemp(prefix="context-guru-proxy.",
                                               dir=destination.parent)
        temporary = Path(temporary_name)
        try:
            with os.fdopen(fd, "wb") as output:
                shutil.copyfileobj(source, output)
            temporary.chmod(0o755)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)
    return str(destination.resolve()), version


def install_escape_hatch():
    source = Path(__file__).with_name("reset.sh")
    target = state_dir() / "context-guru-reset"
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(0o700)
    helper = state_dir() / "config_route.py"
    shutil.copyfile(Path(__file__).with_name("config_route.py"), helper)
    helper.chmod(0o700)
    controller = state_dir() / "codex_plugin.py"
    shutil.copyfile(Path(__file__), controller)
    controller.chmod(0o700)
    launcher = state_dir() / "context-guru-codex"
    launcher.write_text(f"#!/bin/sh\n{MARKER}\n"
                        f'exec python3 "{controller}" launch -- "$@"\n')
    launcher.chmod(0o700)
    updater = state_dir() / "context-guru-update"
    shutil.copyfile(Path(__file__).with_name("update.sh"), updater)
    updater.chmod(0o700)
    return target


def write_proxy_config():
    path = proxy_config()
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() and not path.read_text().startswith(CONFIG_MARKER):
        raise RuntimeError(f"refusing to overwrite unmanaged proxy config: {path}")
    temporary = path.with_suffix(".tmp")
    temporary.write_text(
        f"{CONFIG_MARKER}\n"
        "preset: off\n"
        "cache:\n"
        "  keepalive: true\n"
    )
    temporary.chmod(0o600)
    os.replace(temporary, path)
    return path


def read_proxy_options():
    preset, keepalive = "off", True
    try:
        lines = proxy_config().read_text().splitlines()
    except OSError:
        return preset, keepalive
    in_cache = False
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("preset:"):
            preset = stripped.split(":", 1)[1].strip()
        elif stripped == "cache:":
            in_cache = True
        elif line and not line.startswith((" ", "\t", "#")):
            in_cache = False
        elif in_cache and stripped.startswith("keepalive:"):
            keepalive = stripped.split(":", 1)[1].strip().lower() == "true"
    return preset, keepalive


def save_proxy_options(preset, keepalive):
    if preset not in {"off", "conservative", "medium", "high", "xhigh"}:
        raise RuntimeError(f"unknown preset: {preset}")
    path = proxy_config()
    if path.exists() and not path.read_text().startswith(CONFIG_MARKER):
        raise RuntimeError(f"refusing to overwrite unmanaged proxy config: {path}")
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(f"{CONFIG_MARKER}\npreset: {preset}\ncache:\n"
                         f"  keepalive: {str(keepalive).lower()}\n")
    temporary.chmod(0o600)
    os.replace(temporary, path)


def restart_proxy(record):
    port, executable = int(record.get("port", 0)), record.get("binary")
    if not port or not executable:
        return False
    stopped = stop_owned(record)
    if stopped == "not_owned":
        return False
    for _ in range(20):
        if not healthy(port):
            break
        time.sleep(0.1)
    process = start_proxy(executable, port, record.get("upstream"))
    if process is None:
        return False
    record["pid"] = process.pid
    record["service"] = "systemd" if platform.system() == "Linux" else "launchd"
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    return True


def configure(args):
    if not routing_state().exists():
        facts(result="not_installed")
        return 2
    preset, keepalive = read_proxy_options()
    if args.show:
        facts(result="ok", preset=preset,
              cache_strategy="30-min-ping" if keepalive else "none")
        return 0
    if args.preset:
        preset = args.preset
    if args.cache_strategy:
        keepalive = args.cache_strategy == "30-min-ping"
    try:
        save_proxy_options(preset, keepalive)
    except Exception as error:
        facts(result="refused", detail=error)
        return 2
    restarted = restart_proxy(read_record())
    facts(result="configured", preset=preset,
          cache_strategy="30-min-ping" if keepalive else "none",
          proxy_restarted=str(restarted).lower())
    return 0 if restarted else 1


def service_command(executable, port, upstream=None):
    return proxy_command(executable, port, upstream)


def _systemd_quote(value):
    return '"' + str(value).replace("\\", "\\\\").replace('"', '\\"') + '"'


def start_proxy(executable, port, upstream=None):
    """Start through the host user service manager, never as a sandbox child."""
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    command = service_command(executable, port, upstream)
    system = platform.system()
    if system == "Linux":
        unit = systemd_unit()
        unit.parent.mkdir(parents=True, exist_ok=True)
        if unit.exists() and not unit.read_text().startswith(CONFIG_MARKER):
            return None
        unit.write_text(
            f"{CONFIG_MARKER}\n[Unit]\nDescription=context-guru for Codex\n\n"
            "[Service]\nType=simple\n"
            f"ExecStart={' '.join(_systemd_quote(item) for item in command)}\n"
            "Restart=on-failure\nRestartSec=1\n"
            f"StandardOutput={_systemd_quote('append:' + str(root / 'proxy.log'))}\n"
            f"StandardError={_systemd_quote('append:' + str(root / 'proxy.log'))}\n\n"
            "[Install]\nWantedBy=default.target\n")
        unit.chmod(0o600)
        try:
            subprocess.run(["systemctl", "--user", "daemon-reload"], check=True,
                           capture_output=True, text=True)
            subprocess.run(["systemctl", "--user", "enable", "--now", str(unit)], check=True,
                           capture_output=True, text=True)
        except (OSError, subprocess.CalledProcessError):
            return None
        pid_command = ["systemctl", "--user", "show", "--property", "MainPID", "--value",
                       unit.name]
    elif system == "Darwin":
        plist = launchd_plist()
        plist.parent.mkdir(parents=True, exist_ok=True)
        if plist.exists():
            try:
                with open(plist, "rb") as handle:
                    if plistlib.load(handle).get("ContextGuruManaged") is not True:
                        return None
            except (OSError, ValueError, plistlib.InvalidFileException):
                return None
        with open(plist, "wb") as handle:
            plistlib.dump({"Label": "io.rossoctl.context-guru-codex",
                           "ContextGuruManaged": True,
                           "ProgramArguments": command, "RunAtLoad": True,
                           "KeepAlive": True, "ProcessType": "Background",
                           "StandardOutPath": str(root / "proxy.log"),
                           "StandardErrorPath": str(root / "proxy.log")}, handle)
        plist.chmod(0o600)
        domain = f"gui/{os.getuid()}"
        subprocess.run(["launchctl", "bootout", domain + "/io.rossoctl.context-guru-codex"],
                       capture_output=True)
        try:
            subprocess.run(["launchctl", "bootstrap", domain, str(plist)], check=True,
                           capture_output=True, text=True)
            subprocess.run(["launchctl", "kickstart", "-k",
                            domain + "/io.rossoctl.context-guru-codex"], check=True,
                           capture_output=True, text=True)
        except (OSError, subprocess.CalledProcessError):
            return None
        pid_command = ["launchctl", "print", domain + "/io.rossoctl.context-guru-codex"]
    else:
        return None
    for _ in range(40):
        if healthy(port):
            pid = 0
            if system == "Linux":
                try:
                    pid = int(subprocess.check_output(pid_command, text=True).strip())
                except (OSError, ValueError, subprocess.CalledProcessError):
                    pass
            return SimpleNamespace(pid=pid)
        time.sleep(0.1)
    return None


def proxy_command(executable, port, upstream=None):
    root = state_dir()
    command = [executable, "--listen", f"127.0.0.1:{port}", "--config", str(proxy_config()),
               "--dashboard", "--dashboard-db", str(root / "dashboard.db")]
    if upstream:
        command.extend(["--openai-upstream", upstream.rstrip("/").removesuffix("/v1")])
    return command


def base_provider():
    path = codex_home() / "config.toml"
    try:
        lines = path.read_text().splitlines()
    except OSError:
        return {}
    name = None
    for line in lines:
        key, separator, value = line.partition("=")
        if separator and key.strip() == "model_provider":
            try:
                name = json.loads(value.strip())
            except (ValueError, TypeError):
                return {}
            break
    if not isinstance(name, str):
        return {}
    section = f'[model_providers.{name}]'
    provider = {}
    active = False
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("["):
            active = stripped == section
            continue
        if not active:
            continue
        key, separator, value = line.partition("=")
        key = key.strip()
        if not separator or key not in {"base_url", "experimental_bearer_token",
                                        "requires_openai_auth"}:
            continue
        value = value.strip()
        if value in {"true", "false"}:
            provider[key] = value == "true"
        else:
            try:
                provider[key] = json.loads(value)
            except (ValueError, TypeError):
                pass
    return provider


def setup(args):
    if args.plan:
        facts(result="planned", scope="user", config=main_config(),
              reset=state_dir() / "context-guru-reset",
              launch=state_dir() / "context-guru-codex",
              consent_question="Add a context-guru profile to your Codex config and run a local "
                               "context-guru proxy (cache keep-alive uses your own quota)? Plain "
                               "`codex` is left unchanged; start routed sessions with "
                               "context-guru-codex.")
        return 0
    if not args.i_consent_to_traffic_interception:
        facts(result="refused", reason="consent_required")
        return 2
    try:
        write_proxy_config()
    except Exception as error:
        facts(result="config_conflict", config=proxy_config(), detail=error)
        return 2
    executable = find_binary()
    if not executable:
        try:
            executable, version = install_release_binary()
            facts(binary="installed", binary_version=version, binary_path=executable)
        except Exception as error:
            facts(result="binary_install_failed", detail=error)
            return 2
    existing = read_record()
    provider = existing.get("provider") if is_routed(main_config()) or is_default_routed(main_config()) else None
    if not isinstance(provider, dict):
        provider = base_provider()
    port = int(existing.get("port", 0))
    if port and healthy(port):
        try:
            install_default_route(main_config(), routing_state(), port, provider)
            reset = install_escape_hatch()
        except Exception as error:
            facts(result="setup_failed", detail=error)
            return 1
        existing.update(config=str(main_config()), provider=provider,
                        upstream=provider.get("base_url"))
        (state_dir() / "install.json").write_text(json.dumps(existing, indent=2) + "\n")
        facts(result="already_running", port=port, config=main_config(), reset=reset,
              launch=state_dir() / "context-guru-codex")
        return 0
    port = args.port or first_free_port()
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    upstream = provider.get("base_url")
    process = start_proxy(executable, port, upstream)
    if process is None:
        facts(result="start_failed", log=root / "proxy.log")
        return 1
    try:
        install_default_route(main_config(), routing_state(), port, provider)
        reset = install_escape_hatch()
    except Exception as error:
        stop_owned({"service": "systemd" if platform.system() == "Linux" else "launchd",
                    "pid": process.pid, "binary": executable})
        facts(result="setup_failed", detail=error)
        return 1
    record = {"port": port, "pid": process.pid, "binary": executable,
              "config": str(main_config()), "upstream": upstream, "provider": provider,
              "service": "systemd" if platform.system() == "Linux" else "launchd"}
    (root / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    facts(result="installed", port=port, config=main_config(), reset=reset, launch=state_dir() / "context-guru-codex")
    return 0


def revive(record):
    """True when the recorded proxy answers, restarting its user service first if it is down."""
    port, executable = int(record.get("port", 0)), record.get("binary")
    if not port or not executable:
        return False
    if healthy(port):
        return True
    process = start_proxy(executable, port, record.get("upstream"))
    if process is None:
        return False
    record["pid"] = process.pid
    record["service"] = "systemd" if platform.system() == "Linux" else "launchd"
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    return True


def ensure(_args):
    record = read_record()
    port = int(record.get("port", 0))
    if not port or not is_routed(main_config()) or healthy(port):
        return 0
    if revive(record):
        facts(result="restarted", port=port)
    else:
        facts(result="start_failed", log=state_dir() / "proxy.log")
    return 0


def launch(args):
    """Start Codex through the profile; if the proxy cannot be brought up, start plain Codex.

    Plain `codex` never depends on the proxy, so a dead proxy cannot lock the user out.
    """
    codex = os.environ.get("CODEX_BIN") or shutil.which("codex")
    if not codex:
        print("context-guru-codex: `codex` not found on PATH", file=sys.stderr)
        return 127
    rest = args.codex_args[1:] if args.codex_args[:1] == ["--"] else args.codex_args
    if is_routed(main_config()) and revive(read_record()):
        os.environ["CONTEXT_GURU_ROUTED"] = "1"
        argv = [codex, "--profile", "context-guru", *rest]
    else:
        print("context-guru-codex: the proxy is not reachable and could not be restarted; "
              "starting Codex directly. This session is NOT routed through context-guru "
              f"(log: {state_dir() / 'proxy.log'}).", file=sys.stderr)
        argv = [codex, *rest]
    os.execv(codex, argv)


def session_start_hook(_args):
    """Advisory only: a hook runs after provider selection, so it cannot change transport."""
    record = read_record()
    if not int(record.get("port", 0)) or not is_routed(main_config()):
        return 0
    message = None
    if os.environ.get("CONTEXT_GURU_ROUTED") != "1":
        message = ("context-guru is installed but this session is not routed through it. "
                   f"Start Codex with {state_dir() / 'context-guru-codex'} to route it.")
    elif not revive(record):
        message = ("context-guru proxy is down and could not be restarted: requests from this "
                   "session will fail. Quit and start plain `codex`, or run $context-guru-doctor.")
    if message:
        print(json.dumps({"systemMessage": message}))
    return 0


def doctor(_args):
    """Read-only checks; each line is name=ok, or name=FAIL fix=<command>."""
    record = read_record()
    port = int(record.get("port", 0))
    launcher = state_dir() / "context-guru-codex"
    checks = [
        ("codex_on_path", bool(os.environ.get("CODEX_BIN") or shutil.which("codex")),
         "install the Codex CLI"),
        ("installed", bool(port), "run $context-guru-setup"),
        ("profile_present", is_routed(main_config()), "run $context-guru-setup"),
        ("default_provider_untouched", not is_default_routed(main_config()),
         f"run $context-guru-setup again (it migrates the legacy install)"),
        ("binary_present", bool(record.get("binary")) and Path(record["binary"]).is_file(),
         "run $context-guru-setup"),
        ("launcher_present", launcher.is_file(), "run $context-guru-setup"),
        ("proxy_healthy", bool(port) and healthy(port),
         f"start Codex with {launcher} (it restarts the proxy); log: {state_dir() / 'proxy.log'}"),
        ("session_routed", os.environ.get("CONTEXT_GURU_ROUTED") == "1",
         f"start Codex with {launcher}"),
    ]
    for name, ok, fix in checks:
        print(f"{name}=ok" if ok else f"{name}=FAIL fix={fix}")
    return 0 if all(ok for _, ok, _ in checks[:-1]) else 1


def serve(_args):
    """Keep the proxy attached to Codex's asynchronous SessionStart hook."""
    record = read_record()
    port = int(record.get("port", 0))
    executable = record.get("binary")
    if not port or not executable or not is_routed(main_config()) or healthy(port):
        return 0
    record["pid"] = os.getpid()
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    log_fd = os.open(state_dir() / "proxy.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    os.dup2(log_fd, 1)
    os.dup2(log_fd, 2)
    if log_fd > 2:
        os.close(log_fd)
    os.execv(executable, proxy_command(executable, port, record.get("upstream")))


def status(args):
    record = read_record()
    port = int(record.get("port", 0))
    if getattr(args, "stats", False):
        if not port:
            print("context-guru status --stats: no installed proxy", file=sys.stderr)
            return 1
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/stats", timeout=2) as response:
                sys.stdout.write(response.read().decode("utf-8"))
            return 0
        except Exception as error:
            print(f"context-guru status --stats: {error}", file=sys.stderr)
            return 1
    up = bool(port and healthy(port))
    configured = is_routed(main_config())
    values = {"result": "ok" if up and configured else "not_ready", "config": main_config(),
              "profile_configured": str(configured).lower(), "default_provider_untouched": str(not is_default_routed(main_config())).lower(), "port": port or "(none)",
              "proxy_up": str(up).lower(), "session_routed": "unknown"}
    if up:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/stats", timeout=2) as response:
                values["stats_json"] = json.load(response)
        except Exception as error:
            values["stats_error"] = str(error)
    facts(**values)
    return 0 if up and configured else 1


def stop_owned(record):
    service = record.get("service")
    if service == "systemd" or (service is None and systemd_unit().exists()):
        unit = systemd_unit()
        try:
            subprocess.run(["systemctl", "--user", "disable", "--now", unit.name], check=True,
                           capture_output=True, text=True)
            if unit.exists() and unit.read_text().startswith(CONFIG_MARKER):
                unit.unlink()
                subprocess.run(["systemctl", "--user", "daemon-reload"], check=False,
                               capture_output=True)
            return "stopped"
        except (OSError, subprocess.CalledProcessError):
            return "not_owned"
    if service == "launchd" or (service is None and launchd_plist().exists()):
        plist = launchd_plist()
        try:
            with open(plist, "rb") as handle:
                if plistlib.load(handle).get("ContextGuruManaged") is not True:
                    return "not_owned"
            subprocess.run(["launchctl", "bootout",
                            f"gui/{os.getuid()}/io.rossoctl.context-guru-codex"], check=False,
                           capture_output=True)
            if plist.exists():
                plist.unlink()
            return "stopped"
        except OSError:
            return "not_owned"
    pid = record.get("pid")
    if not isinstance(pid, int):
        return "gone"
    try:
        command = subprocess.check_output(
            ["ps", "-p", str(pid), "-o", "command="], text=True).strip()
    except subprocess.CalledProcessError:
        return "gone"
    except OSError:
        return "not_owned"
    expected = record.get("binary", "")
    if not expected or not command.startswith(expected + " "):
        return "not_owned"
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return "gone"
    return "stopped"


def update(args):
    try:
        latest = latest_release_tag()
    except Exception as error:
        facts(result="error", reason="release_check_failed", detail=error)
        return 1
    executable = find_binary()
    installed = "unknown"
    if executable:
        completed = subprocess.run([executable, "--version"], text=True, capture_output=True)
        if completed.returncode == 0:
            fields = completed.stdout.split()
            if len(fields) >= 2:
                installed = fields[1]
    available = installed.removeprefix("v") != latest.removeprefix("v")
    if args.check:
        facts(result="checked", installed_version=installed, latest_version=latest,
              update_available=str(available).lower())
        return 0
    try:
        executable, version = install_release_binary(latest)
    except Exception as error:
        facts(result="error", reason="binary_install_failed", detail=error)
        return 1
    record = read_record()
    port = int(record.get("port", 0))
    stopped = stop_owned(record)
    if stopped == "not_owned" or not port or not executable:
        facts(restart="skipped", reason=stopped if stopped == "not_owned" else "incomplete_record")
        return 1
    process = start_proxy(executable, port, record.get("upstream"))
    if process is None:
        facts(restart="failed", port=port)
        return 1
    record.update(pid=process.pid, binary=executable)
    record["service"] = "systemd" if platform.system() == "Linux" else "launchd"
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    facts(result="updated", version=version, restart="completed", port=port)
    return 0


def uninstall(args):
    record = read_record()
    routed = is_routed(main_config())
    facts(result="planned" if args.dry_run else "removing", config=main_config(),
          profile_configured=str(routed).lower(), pid=record.get("pid", "(none)"))
    if args.dry_run:
        return 0
    process_result = stop_owned(record)
    if process_result == "not_owned":
        facts(process="not_owned")
    restore_result = restore_default_route(routing_state())
    facts(config_restore=restore_result)
    path = profile()
    if path.exists() and path.read_text().startswith(MARKER):
        path.unlink()
    config = proxy_config()
    if config.exists() and config.read_text().startswith(CONFIG_MARKER):
        config.unlink()
    install = state_dir() / "install.json"
    if install.exists() and process_result != "not_owned":
        install.unlink()
    if process_result != "not_owned":
        facts(binary_removed=str(remove_managed_binary()).lower())
    facts(result="removed")
    return 0


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    install = commands.add_parser("setup")
    install.add_argument("--plan", action="store_true")
    install.add_argument("--port", type=int, default=0)
    install.add_argument("--i-consent-to-traffic-interception", action="store_true")
    status_parser = commands.add_parser("status")
    status_parser.add_argument("--stats", action="store_true")
    commands.add_parser("ensure")
    commands.add_parser("serve")
    commands.add_parser("doctor")
    commands.add_parser("session-start-hook")
    starter = commands.add_parser("launch")
    starter.add_argument("codex_args", nargs=argparse.REMAINDER)
    upgrade = commands.add_parser("update")
    mode = upgrade.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--install", action="store_true")
    remove = commands.add_parser("uninstall")
    remove.add_argument("--dry-run", action="store_true")
    settings = commands.add_parser("configure")
    settings.add_argument("--show", action="store_true")
    settings.add_argument("--preset", choices=("off", "conservative", "medium", "high", "xhigh"))
    settings.add_argument("--cache-strategy", choices=("none", "30-min-ping"))
    args = parser.parse_args()
    return {"setup": setup, "status": status, "ensure": ensure, "serve": serve,
            "update": update, "uninstall": uninstall, "configure": configure,
            "doctor": doctor, "launch": launch, "session-start-hook": session_start_hook}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
