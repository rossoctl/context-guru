#!/usr/bin/env python3
"""Install and operate context-guru's isolated Codex profile."""

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.request

MARKER = "# Managed by the context-guru Codex plugin."


def codex_home():
    return Path(os.environ.get("CODEX_HOME", Path.home() / ".codex"))


def state_dir():
    base = Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local/state"))
    return base / "context-guru-codex"


def profile():
    return codex_home() / "context-guru.config.toml"


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
    candidate = Path(__file__).resolve().parents[4] / "bin/context-guru-proxy"
    if candidate.is_file() and os.access(candidate, os.X_OK):
        return str(candidate)
    return None


def shared_installer():
    return Path(__file__).resolve().parents[4] / "context-guru-plugin/scripts/install.sh"


def install_escape_hatch():
    source = Path(__file__).with_name("reset.sh")
    target = state_dir() / "context-guru-reset"
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(0o700)
    return target


def start_proxy(executable, port, upstream=None):
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    log = open(root / "proxy.log", "ab", buffering=0)
    process = subprocess.Popen(
        proxy_command(executable, port, upstream),
        stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True,
    )
    for _ in range(40):
        if healthy(port):
            return process
        if process.poll() is not None:
            return None
        time.sleep(0.1)
    process.terminate()
    return None


def proxy_command(executable, port, upstream=None):
    root = state_dir()
    command = [executable, "--listen", f"127.0.0.1:{port}", "--preset", "off", "--dashboard",
               "--dashboard-db", str(root / "dashboard.db")]
    if upstream:
        command.extend(["--openai-upstream", upstream.rstrip("/")])
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


def write_profile(port, provider=None):
    path = profile()
    path.parent.mkdir(parents=True, exist_ok=True)
    provider = provider or {}
    auth = provider.get("experimental_bearer_token")
    auth_line = f"experimental_bearer_token = {json.dumps(auth)}\n" if auth else ""
    body = f'''{MARKER}
model_provider = "context-guru"

[model_providers.context-guru]
name = "context-guru (local)"
base_url = "http://127.0.0.1:{port}/openai/v1"
wire_api = "responses"
requires_openai_auth = {str(bool(provider.get("requires_openai_auth", True))).lower()}
{auth_line}'''
    if path.exists() and not path.read_text().startswith(MARKER):
        raise RuntimeError(f"refusing to overwrite unmanaged profile: {path}")
    temporary = path.with_suffix(".tmp")
    temporary.write_text(body)
    temporary.chmod(0o600)
    os.replace(temporary, path)


def setup(_args):
    executable = find_binary()
    if not executable:
        facts(result="missing_binary", detail="install context-guru-proxy on PATH or run make build")
        return 2
    path = profile()
    if path.exists() and not path.read_text().startswith(MARKER):
        facts(result="profile_conflict", profile=path, detail="refusing to overwrite unmanaged profile")
        return 2
    provider = base_provider()
    existing = read_record()
    port = int(existing.get("port", 0))
    if port and healthy(port):
        write_profile(port, provider)
        reset = install_escape_hatch()
        facts(result="already_running", port=port, profile=profile(), reset=reset,
              launch="codex -p context-guru")
        return 0
    port = first_free_port()
    root = state_dir()
    root.mkdir(parents=True, exist_ok=True)
    upstream = provider.get("base_url")
    process = start_proxy(executable, port, upstream)
    if process is None:
        facts(result="start_failed", log=root / "proxy.log")
        return 1
    try:
        write_profile(port, provider)
        reset = install_escape_hatch()
    except Exception as error:
        process.terminate()
        facts(result="setup_failed", detail=error)
        return 1
    record = {"port": port, "pid": process.pid, "binary": executable,
              "profile": str(profile()), "upstream": upstream}
    (root / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    facts(result="installed", port=port, profile=profile(), reset=reset,
          launch="codex -p context-guru")
    return 0


def ensure(_args):
    record = read_record()
    port = int(record.get("port", 0))
    executable = record.get("binary")
    if not port or not executable or not profile().exists():
        return 0
    if healthy(port):
        return 0
    process = start_proxy(executable, port, record.get("upstream"))
    if process is None:
        facts(result="start_failed", log=state_dir() / "proxy.log")
        return 0
    record["pid"] = process.pid
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    facts(result="restarted", port=port)
    return 0


def serve(_args):
    """Keep the proxy attached to Codex's asynchronous SessionStart hook."""
    record = read_record()
    port = int(record.get("port", 0))
    executable = record.get("binary")
    if not port or not executable or not profile().exists() or healthy(port):
        return 0
    record["pid"] = os.getpid()
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    log_fd = os.open(state_dir() / "proxy.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    os.dup2(log_fd, 1)
    os.dup2(log_fd, 2)
    if log_fd > 2:
        os.close(log_fd)
    os.execv(executable, proxy_command(executable, port, record.get("upstream")))


def status(_args):
    record = read_record()
    port = int(record.get("port", 0))
    up = bool(port and healthy(port))
    configured = profile().exists() and profile().read_text().startswith(MARKER)
    values = {"result": "ok" if up and configured else "not_ready", "profile": profile(),
              "profile_configured": str(configured).lower(), "port": port or "(none)",
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
    pid = record.get("pid")
    if not isinstance(pid, int):
        return "gone"
    try:
        command = subprocess.check_output(
            ["ps", "-p", str(pid), "-o", "command="], text=True).strip()
    except subprocess.CalledProcessError:
        return "gone"
    expected = record.get("binary", "")
    if not expected or not command.startswith(expected + " "):
        return "not_owned"
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return "gone"
    return "stopped"


def update(args):
    installer = shared_installer()
    if not installer.is_file():
        facts(result="error", reason="shared_installer_missing", path=installer)
        return 1
    command = [str(installer), "--check-latest"] if args.check else [str(installer)]
    env = os.environ.copy()
    if args.install:
        env["CONTEXT_GURU_UPGRADE"] = "1"
    else:
        env.pop("CONTEXT_GURU_UPGRADE", None)
    completed = subprocess.run(command, env=env, text=True, capture_output=True)
    if completed.stdout:
        print(completed.stdout, end="")
    if completed.stderr:
        print(completed.stderr, end="", file=sys.stderr)
    if completed.returncode or args.check:
        return completed.returncode
    record = read_record()
    port = int(record.get("port", 0))
    stopped = stop_owned(record)
    executable = find_binary()
    if stopped == "not_owned" or not port or not executable:
        facts(restart="skipped", reason=stopped if stopped == "not_owned" else "incomplete_record")
        return 1
    process = start_proxy(executable, port, record.get("upstream"))
    if process is None:
        facts(restart="failed", port=port)
        return 1
    record.update(pid=process.pid, binary=executable)
    (state_dir() / "install.json").write_text(json.dumps(record, indent=2) + "\n")
    facts(restart="completed", port=port)
    return 0


def uninstall(args):
    record = read_record()
    path = profile()
    owned = path.exists() and path.read_text().startswith(MARKER)
    facts(result="planned" if args.dry_run else "removing", profile=path,
          profile_owned=str(owned).lower(), pid=record.get("pid", "(none)"))
    if args.dry_run:
        return 0
    process_result = stop_owned(record)
    if process_result == "not_owned":
        facts(process="not_owned")
    if owned:
        path.unlink()
    install = state_dir() / "install.json"
    if install.exists() and process_result != "not_owned":
        install.unlink()
    facts(result="removed")
    return 0


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("setup")
    commands.add_parser("status")
    commands.add_parser("ensure")
    commands.add_parser("serve")
    upgrade = commands.add_parser("update")
    mode = upgrade.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--install", action="store_true")
    remove = commands.add_parser("uninstall")
    remove.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    return {"setup": setup, "status": status, "ensure": ensure, "serve": serve,
            "update": update, "uninstall": uninstall}[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
