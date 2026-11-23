#!/usr/bin/env python3
"""Install or remove Pi, the context-guru Pi package and Pi's Anthropic connection, in one run.

install-pi.sh downloads the release's Pi package, checks its SHA-256, and runs this file from it, with the
user's answers on the terminal. See docs/superpowers/specs/2026-11-23-pi-one-command-installer-design.md.

  * Nothing changes before the key check passes, and nothing changes without a yes.
  * The key goes only into the OS secret store (and into the one key-check request). models.json holds a
    command that prints it, which Pi runs at request time.
  * A record lists what this file wrote, so uninstall removes that and nothing else.
"""
import argparse
import getpass
import json
import os
from pathlib import Path
import platform as platform_module
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import models_route  # noqa: E402
import pi_plugin  # noqa: E402

PLUGIN_SCRIPT = str(HERE / "pi_plugin.py")
PI_NPM = "@earendil-works/pi-coding-agent"
PACKAGE_NAME = "@rossoctl/context-guru-pi"
PUBLIC_REGISTRY = "https://registry.npmjs.org/"
MIN_NODE = (22, 19)
ANTHROPIC_API = "https://api.anthropic.com"
DEFAULT_MODEL = "claude-haiku-5-5"
KEY_SERVICE = "context-guru-pi-anthropic"
KEY_TRIES = 3
RETRY_WAIT_SECONDS = 10


class Stop(Exception):
    """A step cannot go on. The message says why, and what to do."""


def package_dir():
    return Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share") / "context-guru-pi/package"


def installer_record_path():
    return pi_plugin.state_dir() / "installer.json"


def backup_dir():
    return pi_plugin.state_dir() / "installer-backup"


def settings_path():
    return pi_plugin.agent_dir() / "settings.json"


def version_tuple(text):
    """(major, minor, patch) from `v24.2.0` or `1.6.0`; None when it is not a version."""
    try:
        return pi_plugin.release_version(text.strip())
    except RuntimeError:
        return None


def load_json(path):
    if not path.exists() or not path.read_text().strip():
        return {}
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise Stop(f"{path} is not a JSON object. Fix or move it, then run this again.")
    return value


def write_json(path, value):
    models_route._atomic(path, json.dumps(value, indent=2) + "\n")


class Terminal:
    """Questions and messages on the user's terminal. The key is read without echo."""

    def __init__(self, stream_in=sys.stdin, stream_out=sys.stdout):
        self.stream_in, self.stream_out = stream_in, stream_out

    def say(self, text):
        print(text, file=self.stream_out, flush=True)

    def ask(self, question, default=""):
        self.stream_out.write(question + " ")
        self.stream_out.flush()
        line = self.stream_in.readline()
        if not line:
            raise Stop("no answer (the input ended). Nothing more was changed.")
        return line.strip() or default

    def secret(self, question):
        return getpass.getpass(question + " ", stream=self.stream_out)


class Shell:
    """Runs a program. The arguments never hold a secret: a secret goes on stdin."""

    def which(self, name):
        return shutil.which(name)

    def run(self, cmd, stdin=None):
        try:
            return subprocess.run(cmd, input=stdin, capture_output=True, text=True, timeout=600)
        except FileNotFoundError:
            return subprocess.CompletedProcess(cmd, 127, "", f"{cmd[0]}: not found")


def http_post(url, headers, body):
    """The HTTP status of one POST. A network error is status 0."""
    request = urllib.request.Request(url, data=body.encode(), headers=headers, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code
    except (urllib.error.URLError, OSError):
        return 0


def facts(text):
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


class Installer:
    def __init__(self, terminal, shell, http, source, plugin=PLUGIN_SCRIPT, platform=None, sleep=time.sleep):
        self.terminal, self.shell, self.http, self.source = terminal, shell, http, Path(source)
        self.plugin, self.platform, self.sleep = plugin, platform or platform_module.system(), sleep
        self.registry = os.environ.get("CG_PI_NPM_REGISTRY") or PUBLIC_REGISTRY
        self.record = {}

    # ------------------------------------------------------------------------------- install
    def install(self):
        try:
            self.record = load_json(installer_record_path())
            self.check_node()
            self.ensure_pi()
            self.install_package()
            self.save_record()
            if not self.connection():
                return 0
            self.save_record()
            if not self.setup():
                return 0
            self.finish()
            return 0
        except Stop as stop:
            self.terminal.say(f"context-guru: {stop}")
            return 1

    def check_node(self):
        out = self.shell.run(["node", "--version"])
        version = version_tuple(out.stdout) if out.returncode == 0 else None
        if version is None or version[:2] < MIN_NODE:
            raise Stop("Pi needs Node.js 22.19 or later. Install it from https://nodejs.org, then run this again. "
                       "Nothing was changed.")

    def latest_pi(self):
        out = self.shell.run(["npm", "view", PI_NPM, "version", f"--registry={self.registry}"])
        if out.returncode != 0 or not version_tuple(out.stdout):
            raise Stop(f"npm cannot read the latest Pi version from {self.registry}:\n{out.stderr.strip()}\n"
                       "To use another registry, set CG_PI_NPM_REGISTRY. Nothing was changed.")
        return out.stdout.strip()

    def ensure_pi(self):
        installed = None
        if self.shell.which("pi"):
            installed = self.shell.run(["pi", "--version"]).stdout.strip()
        latest = self.latest_pi()
        if installed and version_tuple(installed) and version_tuple(installed) >= version_tuple(latest):
            self.terminal.say(f"Pi {installed} is installed.")
            return
        if installed:
            if not self.yes(f"Update Pi {installed} to {latest}?", default=True):
                self.terminal.say(f"Pi {installed} stays.")
                return
        elif not self.yes(f"Install Pi {latest}?", default=True):
            raise Stop("context-guru needs Pi. Nothing was changed.")
        out = self.shell.run(["npm", "install", "-g", f"--registry={self.registry}", f"{PI_NPM}@{latest}"])
        if out.returncode != 0:
            raise Stop(f"npm could not install Pi:\n{out.stderr.strip()}\n"
                       "If npm cannot write its global folder, fix npm's prefix or install Pi yourself. "
                       "This installer never uses sudo.")
        if not installed:
            self.record["pi_installed"] = True
        self.terminal.say(f"✓ Pi {latest} {'updated' if installed else 'installed'}.")

    def install_package(self):
        target = Path(os.path.abspath(package_dir()))
        if target.exists():
            shutil.rmtree(target)
        shutil.copytree(self.source, target, ignore=shutil.ignore_patterns("__pycache__", "*.pyc"))
        self.record["package"] = str(target)
        for entry in self.package_entries():
            if self.is_our_package(entry) and entry != target:
                self.pi(["remove", str(entry)], "remove the other copy of the context-guru package")
                self.terminal.say(f"Removed another copy of the package: {entry}")
        if target not in self.package_entries():
            self.pi(["install", str(target)], "install the context-guru package")
        self.terminal.say("✓ context-guru package installed in Pi.")

    def package_entries(self):
        entries = load_json(settings_path()).get("packages", [])
        paths = []
        for entry in entries if isinstance(entries, list) else []:
            source = entry.get("source") if isinstance(entry, dict) else entry
            if isinstance(source, str) and not source.startswith(("npm:", "git:", "http")):
                path = Path(source).expanduser()
                # abspath resolves `..` (Pi stores paths relative to its folder) but not symlinks: `pi remove`
                # matches the path as Pi stored it.
                paths.append(Path(os.path.abspath(path if path.is_absolute() else pi_plugin.agent_dir() / path)))
        return paths

    @staticmethod
    def is_our_package(path):
        try:
            return json.loads((path / "package.json").read_text()).get("name") == PACKAGE_NAME
        except (OSError, ValueError):
            return False

    def pi(self, args, what):
        out = self.shell.run(["pi", *args])
        if out.returncode != 0:
            raise Stop(f"Pi could not {what}:\n{out.stderr.strip() or out.stdout.strip()}")
        return out

    # ---------------------------------------------------------------------------- connection
    def connection(self):
        models = load_json(pi_plugin.models_path())
        current = models.get("providers", {}).get("anthropic", {})
        if isinstance(current, dict) and (current.get("baseUrl") or current.get("apiKey")):
            where = current.get("baseUrl") or ANTHROPIC_API
            if self.yes(f"Pi already connects to Anthropic at {self.host(where)}. Keep this connection?", default=True):
                return True
        gateway = self.terminal.ask("Gateway URL (press Enter for the Anthropic API):").strip().rstrip("/")
        gateway = gateway.removesuffix("/v1") if gateway else ""
        bearer = bool(gateway) and self.yes("Does the gateway want the key as Authorization: Bearer?", default=True)
        model = self.terminal.ask(f"Model id{' at the gateway' if gateway else ''} [{DEFAULT_MODEL}]:", DEFAULT_MODEL)
        key = self.checked_key(gateway or ANTHROPIC_API, bearer, model)
        command = self.store_key(key)
        del key
        self.write_connection(models, gateway, bearer, model, command)
        return True

    @staticmethod
    def host(url):
        return url.split("//", 1)[-1].split("/", 1)[0]

    def checked_key(self, base, bearer, model):
        for _ in range(KEY_TRIES):
            key = self.terminal.secret("Key (it is not shown):").strip()
            if not key:
                self.terminal.say("No key given.")
                continue
            auth = {"authorization": f"Bearer {key}"} if bearer else {"x-api-key": key}
            headers = {**auth, "anthropic-version": "2023-06-01", "content-type": "application/json"}
            body = json.dumps({"model": model, "max_tokens": 1, "messages": [{"role": "user", "content": "hi"}]})
            status = self.http(f"{base}/v1/messages", headers, body)
            if status == 200:
                self.terminal.say(f"✓ {self.host(base)} accepts the key and the model {model}.")
                return key
            if status in (401, 403):
                self.terminal.say(f"The key is not accepted at {self.host(base)} (HTTP {status}).")
            elif status == 0:
                self.terminal.say(f"Cannot reach {self.host(base)}. Check the URL and the network.")
            else:
                self.terminal.say(f"{self.host(base)} answered HTTP {status}. Check the URL and the model id.")
        raise Stop("no working key. Pi's connection was not changed.")

    def store_key(self, key):
        user = os.environ.get("USER") or getpass.getuser()
        if self.platform == "Darwin":
            quoted = '"' + key.replace("\\", "\\\\").replace('"', '\\"') + '"'
            out = self.shell.run(["security", "-i"],
                                 stdin=f"add-generic-password -U -s {KEY_SERVICE} -a {user} -w {quoted}\n")
            if out.returncode != 0:
                raise Stop("the Keychain did not store the key. Pi's connection was not changed.")
            self.record["secret"] = {"kind": "keychain", "service": KEY_SERVICE, "user": user}
            self.terminal.say("✓ The key is in your Keychain.")
            return f"!security find-generic-password -s {KEY_SERVICE} -a {user} -w"
        if self.shell.which("secret-tool"):
            out = self.shell.run(["secret-tool", "store", "--label=context-guru for Pi: Anthropic key",
                                  "service", KEY_SERVICE, "user", user], stdin=key)
            if out.returncode != 0:
                raise Stop("the secret store did not store the key. Pi's connection was not changed.")
            self.record["secret"] = {"kind": "secret-tool", "service": KEY_SERVICE, "user": user}
            self.terminal.say("✓ The key is in your secret store.")
            return f"!secret-tool lookup service {KEY_SERVICE} user {user}"
        folder = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "context-guru-pi"
        folder.mkdir(parents=True, exist_ok=True)
        folder.chmod(0o700)
        path = folder / "anthropic-key"
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "w") as handle:
            handle.write(key)
        path.chmod(0o600)
        self.record["secret"] = {"kind": "file", "path": str(path)}
        self.terminal.say(f"✓ The key is in {path}, which only you can read (no secret store found).")
        return f"!cat {path}"

    def write_connection(self, models, gateway, bearer, model, command):
        settings = load_json(settings_path())
        self.record["models_created"] = not pi_plugin.models_path().exists()
        self.backup(pi_plugin.models_path())
        self.backup(settings_path())
        anthropic = {"apiKey": command}
        if gateway:
            anthropic["baseUrl"] = gateway
            anthropic["models"] = [{"id": model, "name": model, "reasoning": True, "input": ["text"],
                                    "contextWindow": 200000, "maxTokens": 8192}]
        if bearer:
            anthropic["authHeader"] = True
        entry = models.setdefault("providers", {}).setdefault("anthropic", {})
        entry.update(anthropic)
        write_json(pi_plugin.models_path(), models)
        wrote = {"models": anthropic, "settings": {}}
        if not settings.get("defaultModel"):
            wrote["settings"] = {"defaultProvider": "anthropic", "defaultModel": model}
            settings.update(wrote["settings"])
            write_json(settings_path(), settings)
        self.record["wrote"] = wrote
        self.terminal.say(f"✓ Pi connects to Anthropic at {self.host(gateway or ANTHROPIC_API)}, model {model}.")

    def backup(self, path):
        if path.exists():
            backup_dir().mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, backup_dir() / path.name)

    # -------------------------------------------------------------------------------- setup
    def plugin_run(self, *args):
        return facts(self.shell.run(["python3", self.plugin, *args]).stdout)

    def setup(self):
        plan = self.plugin_run("setup", "--plan")
        chain = plan.get("result") == "needs_decision"
        text = plan.get("consent_question_chain") if chain else plan.get("consent_question")
        if chain and not text:
            raise Stop(f"setup cannot chain behind this gateway ({plan.get('chain_refused', 'no reason given')}).")
        self.terminal.say(text or "")
        if not self.yes("Turn context-guru on?", default=False):
            self.terminal.say("context-guru stays off. Pi works without it. To turn it on later, type "
                              "/context-guru setup in Pi.")
            return False
        args = ["setup", "--i-consent-to-traffic-interception"] + (["--on-conflict", "chain"] if chain else [])
        result = self.plugin_run(*args)
        if result.get("result") != "routed" and result.get("reason") == "health_check_failed":
            # #1288: a cold first start of a new proxy can miss setup's check. Stop the half service, then once more.
            # setup says health_check_failed for any failed start, so the cause is not known here.
            self.terminal.say("The proxy did not answer in time. Trying once more in 10 seconds…")
            self.plugin_run("uninstall")
            self.sleep(RETRY_WAIT_SECONDS)
            result = self.plugin_run(*args)
        if result.get("result") != "routed":
            raise Stop(f"setup did not finish ({result.get('reason', result.get('result', 'unknown'))}). "
                       f"context-guru is off. Pi works without it. Log: {result.get('log', 'none')}")
        self.terminal.say(f"✓ context-guru is on (port {result.get('port', '?')}).")
        return True

    def finish(self):
        status = self.plugin_run("status")
        self.terminal.say(f"Status: proxy {'up' if status.get('proxy_up') == 'true' else 'DOWN'} on port "
                          f"{status.get('port', '?')} · preset {status.get('preset', '?')} · "
                          f"cache strategy {status.get('cache_strategy', '?')}")
        if self.yes("Send one short test prompt through Pi now?", default=False):
            out = self.shell.run(["pi", "-p", "Reply with the single word: ok"], stdin="")
            self.terminal.say(f"Pi answered: {out.stdout.strip()[-200:] or out.stderr.strip()[-200:]}")
        self.terminal.say("Done. Start Pi with: pi")

    # ----------------------------------------------------------------------------- uninstall
    def uninstall(self):
        try:
            record = load_json(installer_record_path())
            plugin = self.plugin if Path(self.plugin).exists() else str(self.source / "scripts/pi_plugin.py")
            # context-guru off first: this puts the route back in models.json and stops the service.
            off = facts(self.shell.run(["python3", plugin, "uninstall"]).stdout)
            details = ", ".join(f"{name}: {off[fact]}" for name, fact in (("models.json", "models_restore"),
                                                                          ("service", "service")) if fact in off)
            self.terminal.say(f"✓ context-guru is off{f' ({details})' if details else ''}.")
            ours = Path(record["package"]) if record.get("package") else None
            for entry in self.package_entries():
                if entry == ours or self.is_our_package(entry):
                    self.pi(["remove", str(entry)], "remove the context-guru package")
            if ours and ours.exists():
                shutil.rmtree(ours)
            self.terminal.say("✓ context-guru package removed from Pi.")
            self.delete_key(record.get("secret") or {})
            wrote = record.get("wrote") or {}
            self.remove_written(pi_plugin.models_path(), ["providers", "anthropic"], wrote.get("models") or {})
            if record.get("models_created") and load_json(pi_plugin.models_path()) in ({}, {"providers": {}}):
                pi_plugin.models_path().unlink(missing_ok=True)  # it was absent before the install
            self.remove_written(settings_path(), [], wrote.get("settings") or {})
            if self.yes("Delete context-guru's local data too (log, dashboard database)?", default=True):
                shutil.rmtree(pi_plugin.state_dir(), ignore_errors=True)
            else:
                installer_record_path().unlink(missing_ok=True)
                shutil.rmtree(backup_dir(), ignore_errors=True)
            if record.get("pi_installed") and self.yes("Uninstall Pi too?", default=False):
                out = self.shell.run(["npm", "uninstall", "-g", PI_NPM])
                self.terminal.say("✓ Pi uninstalled." if out.returncode == 0 else f"npm: {out.stderr.strip()}")
            self.terminal.say("Done. context-guru is removed.")
            return 0
        except Stop as stop:
            self.terminal.say(f"context-guru: {stop}")
            return 1

    def delete_key(self, secret):
        kind = secret.get("kind")
        if kind == "keychain":
            self.shell.run(["security", "delete-generic-password", "-s", secret["service"], "-a", secret["user"]])
        elif kind == "secret-tool":
            self.shell.run(["secret-tool", "clear", "service", secret["service"], "user", secret["user"]])
        elif kind == "file":
            Path(secret["path"]).unlink(missing_ok=True)
        if kind:
            self.terminal.say("✓ The stored key is deleted.")

    @staticmethod
    def remove_written(path, keys, written):
        """Remove each key the installer wrote, while it still has the written value. An entry left
        empty goes too, so a file that had no Anthropic entry gets none back."""
        if not written or not path.exists():
            return
        document = load_json(path)
        parent = document
        for key in keys:
            parent = parent.get(key) if isinstance(parent, dict) else None
        if not isinstance(parent, dict):
            return
        for key, value in written.items():
            if parent.get(key) == value:
                del parent[key]
        if keys and not parent:
            holder = document
            for key in keys[:-1]:
                holder = holder[key]
            del holder[keys[-1]]
        write_json(path, document)

    # ------------------------------------------------------------------------------- helpers
    def yes(self, question, default):
        answer = self.terminal.ask(f"{question} [{'Y/n' if default else 'y/N'}]", "y" if default else "n")
        return answer.lower() in ("y", "yes")

    def save_record(self):
        write_json(installer_record_path(), self.record)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Install or remove context-guru for Pi.")
    parser.add_argument("action", choices=["install", "uninstall"], nargs="?", default="install")
    parser.add_argument("--source", default=str(HERE.parent), help="the unpacked Pi package to install")
    args = parser.parse_args(argv)
    # setup runs from the installed copy, not from the download: it copies its reset command from there.
    installer = Installer(Terminal(), Shell(), http_post, args.source, plugin=str(package_dir() / "scripts/pi_plugin.py"))
    return installer.install() if args.action == "install" else installer.uninstall()


if __name__ == "__main__":
    sys.exit(main())
