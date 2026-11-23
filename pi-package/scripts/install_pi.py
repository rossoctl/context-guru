#!/usr/bin/env python3
"""Install or remove Pi, the context-guru Pi package and Pi's Anthropic connection, in one run.

install-pi.sh downloads the release's Pi package, checks its SHA-256, and runs this file from it, with the
user's answers on the terminal. See docs/superpowers/specs/2026-11-23-pi-one-command-installer-design.md.

  * Pi changes only after a yes. The package is installed before the connection questions. Pi's connection
    (models.json, settings.json, the stored key) changes only after the key check passes.
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
import shlex
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
# The Anthropic entry's keys that describe the connection. A new connection replaces all of them: an old
# baseUrl or authHeader left in place would send the new key to the old endpoint.
CONNECTION_KEYS = ("apiKey", "baseUrl", "authHeader", "headers", "models")
# The phases the user sees, as "Step N of M · <name>". install-pi.sh does the download before this file runs.
DOWNLOAD_STEP = "Download context-guru for Pi"
INSTALL_STEPS = ("Check this computer", "Pi", "The context-guru package for Pi", "Pi's connection to Anthropic",
                 "Check the key", "Save the key and the connection", "Turn context-guru on", "Check that it works")
UNINSTALL_STEPS = ("Turn context-guru off", "Remove the context-guru package", "Delete the stored key",
                   "Put Pi's settings back", "Local data", "Pi itself")


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
    try:
        value = json.loads(path.read_text())
    except ValueError:
        raise Stop(f"{path} is not valid JSON. Fix or move it, then run this again.") from None
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
    def __init__(self, terminal, shell, http, source, plugin=PLUGIN_SCRIPT, platform=None, sleep=time.sleep,
                 downloaded=False):
        self.terminal, self.shell, self.http, self.source = terminal, shell, http, Path(source)
        self.downloaded, self.steps = downloaded, ()
        self.plugin, self.platform, self.sleep = plugin, platform or platform_module.system(), sleep
        self.registry = os.environ.get("CG_PI_NPM_REGISTRY") or PUBLIC_REGISTRY
        self.record = {}

    # ------------------------------------------------------------------------------- install
    def install(self):
        try:
            self.begin(INSTALL_STEPS)
            self.record = load_json(installer_record_path())
            self.step("Check this computer", "Pi needs Node.js 22.19 or later. Nothing changes in this step.")
            self.check_node()
            self.step("Pi", "Pi is the AI coding agent that context-guru works in. This step installs or updates Pi, "
                            "after your yes.")
            self.ensure_pi()
            self.step("The context-guru package for Pi", f"It adds the /context-guru command to Pi. It goes to "
                                                         f"{package_dir()}.")
            self.install_package()
            self.save_record()
            self.connection()
            self.save_record()
            if not self.setup():
                self.skip("Check that it works", "context-guru is off.")
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
        self.step("Pi's connection to Anthropic", "Where Pi sends its requests: the Anthropic API, or your company's "
                                                  "gateway. Nothing is saved before the key check.")
        models = load_json(pi_plugin.models_path())
        current = models.get("providers", {}).get("anthropic", {})
        if isinstance(current, dict) and (current.get("baseUrl") or current.get("apiKey")):
            if self.yes(f"Pi already connects to Anthropic at {self.shown_endpoint(current)}. Keep this connection?",
                        default=True):
                self.skip("Check the key", "you kept your connection.")
                self.skip("Save the key and the connection", "you kept your connection.")
                return
        gateway = self.terminal.ask("Gateway URL (press Enter for the Anthropic API):").strip().rstrip("/")
        gateway = gateway.removesuffix("/v1") if gateway else ""
        bearer = bool(gateway) and self.yes("Does the gateway want the key as Authorization: Bearer?", default=True)
        model = self.terminal.ask(f"Model id{' at the gateway' if gateway else ''} [{DEFAULT_MODEL}]:", DEFAULT_MODEL)
        self.step("Check the key", "One very short request (1 token) checks the key and the model, before anything "
                                   "is saved. The key is not shown on the screen.")
        key = self.checked_key(gateway or ANTHROPIC_API, bearer, model)
        where = {"Darwin": "your Keychain"}.get(self.platform, "your secret store (or a file only you can read)")
        self.step("Save the key and the connection", f"The key goes into {where}. Pi's models.json gets only a "
                                                     "command that reads it. Your files are backed up first.")
        command = self.store_key(key)
        del key
        self.write_connection(models, gateway, bearer, model, command)

    def shown_endpoint(self, entry):
        """The host Pi's Anthropic requests reach. When context-guru routes Pi, models.json names the local
        proxy; the endpoint behind it is in context-guru's own record."""
        url = entry.get("baseUrl") or ANTHROPIC_API
        if url.startswith("http://127.0.0.1:"):
            url = pi_plugin.read_record().get("upstreams", {}).get("anthropic") or ANTHROPIC_API
        return self.host(url)

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
            literal = lambda text: '"' + text.replace("\\", "\\\\").replace('"', '\\"') + '"'
            out = self.shell.run(["security", "-i"], stdin=f"add-generic-password -U -s {KEY_SERVICE} "
                                                           f"-a {literal(user)} -w {literal(key)}\n")
            # `security -i` can exit 0 when its command failed, so read the item back (status only, no -w).
            found = self.shell.run(["security", "find-generic-password", "-s", KEY_SERVICE, "-a", user])
            if out.returncode != 0 or found.returncode != 0:
                raise Stop("the Keychain did not store the key. Pi's connection was not changed.")
            self.record["secret"] = {"kind": "keychain", "service": KEY_SERVICE, "user": user}
            self.terminal.say("✓ The key is in your Keychain.")
            return f"!security find-generic-password -s {KEY_SERVICE} -a {shlex.quote(user)} -w"
        if self.shell.which("secret-tool"):
            out = self.shell.run(["secret-tool", "store", "--label=context-guru for Pi: Anthropic key",
                                  "service", KEY_SERVICE, "user", user], stdin=key)
            if out.returncode != 0:
                raise Stop("the secret store did not store the key. Pi's connection was not changed.")
            self.record["secret"] = {"kind": "secret-tool", "service": KEY_SERVICE, "user": user}
            self.terminal.say("✓ The key is in your secret store.")
            return f"!secret-tool lookup service {KEY_SERVICE} user {shlex.quote(user)}"
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
        return f"!cat {shlex.quote(str(path))}"

    def write_connection(self, models, gateway, bearer, model, command):
        settings = load_json(settings_path())
        self.record.setdefault("models_created", not pi_plugin.models_path().exists())
        self.backup(pi_plugin.models_path())
        self.backup(settings_path())
        new = {"apiKey": command}
        if gateway:
            new["baseUrl"] = gateway
            new["models"] = [{"id": model, "name": model, "reasoning": True, "input": ["text"],
                              "contextWindow": 200000, "maxTokens": 8192}]
        if bearer:
            new["authHeader"] = True
        entry = models.setdefault("providers", {}).setdefault("anthropic", {})
        changes = self.change(entry, {key: new.get(key) for key in CONNECTION_KEYS}, "models")
        write_json(pi_plugin.models_path(), models)
        if not settings.get("defaultModel"):
            self.change(settings, {"defaultProvider": "anthropic", "defaultModel": model}, "settings")
            write_json(settings_path(), settings)
        self.terminal.say(f"✓ Pi connects to Anthropic at {self.host(gateway or ANTHROPIC_API)}, model {model}"
                          + (f" (replaced: {', '.join(sorted(k for k in changes if 'old' in changes[k]))})"
                             if any("old" in c for c in changes.values()) else "") + ".")

    def change(self, document, values, name):
        """Set each key to its value (None: remove it), and record its old and new value, so uninstall can put
        the old one back. A key changed by an earlier install keeps that install's old value."""
        changes = self.record.setdefault("changed", {}).setdefault(name, {})
        for key, value in values.items():
            if key not in changes:
                changes[key] = {"old": document[key]} if key in document else {}
            if value is None:
                document.pop(key, None)
                changes[key].pop("new", None)
            else:
                document[key] = value
                changes[key]["new"] = value
            if changes[key] == {}:
                del changes[key]  # absent before and after: nothing to undo
        return changes

    def backup(self, path):
        if path.exists():
            backup_dir().mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, backup_dir() / path.name)

    # -------------------------------------------------------------------------------- setup
    def plugin_run(self, *args):
        return facts(self.shell.run(["python3", self.plugin, *args]).stdout)

    def setup(self):
        self.step("Turn context-guru on", "It starts the context-guru proxy as a background service, and sends Pi's "
                                          "requests through it. It shows what changes, and asks you first.")
        plan = self.plugin_run("setup", "--plan")
        if plan.get("result") not in ("planned", "needs_decision"):
            raise Stop(f"setup refuses ({plan.get('reason', plan.get('result', 'no answer'))}: "
                       f"{plan.get('detail', '')}). context-guru is off. Pi works without it.")
        chain = plan.get("result") == "needs_decision"
        text = plan.get("consent_question_chain") if chain else plan.get("consent_question")
        if chain and not text:
            raise Stop(f"setup cannot chain behind this gateway ({plan.get('chain_refused', 'no reason given')}).")
        self.terminal.say(text or "")
        if not self.yes("Turn context-guru on?", default=False):
            self.terminal.say("context-guru stays off. Pi works without it. To turn it on later, type "
                              "/context-guru setup in Pi.")
            return False
        # The settings the consent named, on every call: a retry after an uninstall has no record left to read.
        args = ["setup", "--i-consent-to-traffic-interception"] + (["--on-conflict", "chain"] if chain else []) + (
            ["--cache-strategy", plan["cache_strategy"]] if plan.get("cache_strategy") else [])
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
        if plan.get("preset") and result.get("preset") and plan["preset"] != result["preset"]:
            result = {**result, **self.plugin_run("configure", "--preset", plan["preset"])}
        self.terminal.say(f"✓ context-guru is on (port {result.get('port', '?')}).")
        return True

    def finish(self):
        self.step("Check that it works", "It shows the status, and can send one short test prompt through Pi.")
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
            self.begin(UNINSTALL_STEPS)
            record = load_json(installer_record_path())
            plugin = self.plugin if Path(self.plugin).exists() else str(self.source / "scripts/pi_plugin.py")
            self.step("Turn context-guru off", "It puts Pi's models.json back to Pi's own endpoints, and stops and "
                                               "removes the background service.")
            # context-guru off first: this puts the route back in models.json and stops the service.
            off = facts(self.shell.run(["python3", plugin, "uninstall"]).stdout)
            details = ", ".join(f"{name}: {off[fact]}" for name, fact in (("models.json", "models_restore"),
                                                                          ("service", "service")) if fact in off)
            if off.get("models_restore") not in (None, "restored", "no_record"):
                raise Stop(f"the route in models.json was not put back ({off['models_restore']}). Nothing else was "
                           f"removed. To put Pi back, run: sh {pi_plugin.reset_hatch()}")
            self.terminal.say(f"✓ context-guru is off{f' ({details})' if details else ''}.")
            self.step("Remove the context-guru package", "It removes the /context-guru command from Pi.")
            ours = Path(record["package"]) if record.get("package") else None
            for entry in self.package_entries():
                if entry == ours or self.is_our_package(entry):
                    self.pi(["remove", str(entry)], "remove the context-guru package")
            if ours and ours.exists():
                shutil.rmtree(ours)
            self.terminal.say("✓ context-guru package removed from Pi.")
            if record.get("secret"):
                self.step("Delete the stored key", "It deletes the key that the installer saved.")
                self.delete_key(record["secret"])
            else:
                self.skip("Delete the stored key", "the installer saved no key.")
            self.step("Put Pi's settings back", "It puts back each value the installer changed in models.json and "
                                                "settings.json. A value you changed yourself stays.")
            changed = record.get("changed") or {}
            self.restore(pi_plugin.models_path(), ["providers", "anthropic"], changed.get("models") or {})
            if record.get("models_created") and load_json(pi_plugin.models_path()) in ({}, {"providers": {}}):
                pi_plugin.models_path().unlink(missing_ok=True)  # it was absent before the install
            self.restore(settings_path(), [], changed.get("settings") or {})
            self.step("Local data", f"context-guru's log and dashboard database, in {pi_plugin.state_dir()}.")
            if self.yes("Delete context-guru's local data too (log, dashboard database)?", default=True):
                shutil.rmtree(pi_plugin.state_dir(), ignore_errors=True)
            else:
                installer_record_path().unlink(missing_ok=True)
                shutil.rmtree(backup_dir(), ignore_errors=True)
            if record.get("pi_installed"):
                self.step("Pi itself", "The installer installed Pi, so it can remove it too. Your Pi settings stay.")
                if self.yes("Uninstall Pi too?", default=False):
                    out = self.shell.run(["npm", "uninstall", "-g", PI_NPM])
                    self.terminal.say("✓ Pi uninstalled." if out.returncode == 0 else f"npm: {out.stderr.strip()}")
            else:
                self.skip("Pi itself", "the installer did not install Pi, so Pi stays.")
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
    def restore(path, keys, changes):
        """Put back the old value of each key the installer changed, while the key still has the value the
        installer wrote (a value the user changed since stays). An entry left empty goes too."""
        if not changes or not path.exists():
            return
        document = load_json(path)
        parent = document
        for key in keys:
            parent = parent.setdefault(key, {}) if isinstance(parent, dict) else None
        if not isinstance(parent, dict):
            return
        missing = object()
        for key, change in changes.items():
            if parent.get(key, missing) != change.get("new", missing):
                continue
            if "old" in change:
                parent[key] = change["old"]
            else:
                parent.pop(key, None)
        if keys and not parent:
            holder = document
            for key in keys[:-1]:
                holder = holder[key]
            del holder[keys[-1]]
        write_json(path, document)

    # ------------------------------------------------------------------------------- helpers
    def begin(self, names):
        self.steps = ((DOWNLOAD_STEP,) if self.downloaded else ()) + tuple(names)
        if self.downloaded:
            self.step(DOWNLOAD_STEP, "Done: the package matches the release's SHA-256, so it is the file the "
                                     "release published.")

    def header(self, name):
        return f"Step {self.steps.index(name) + 1} of {len(self.steps)} · {name}"

    def step(self, name, explanation):
        self.terminal.say("")  # a blank line before each step, so the phases stand apart
        self.terminal.say(f"{self.header(name)}\n   {explanation}")

    def skip(self, name, why):
        self.terminal.say("")
        self.terminal.say(f"{self.header(name)} — skipped: {why}")

    def yes(self, question, default):
        answer = self.terminal.ask(f"{question} [{'Y/n' if default else 'y/N'}]", "y" if default else "n")
        return answer.lower() in ("y", "yes")

    def save_record(self):
        write_json(installer_record_path(), self.record)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Install or remove context-guru for Pi.")
    parser.add_argument("action", choices=["install", "uninstall"], nargs="?", default="install")
    parser.add_argument("--source", default=str(HERE.parent), help="the unpacked Pi package to install")
    parser.add_argument("--downloaded", action="store_true", help="install-pi.sh downloaded and checked it (step 1)")
    args = parser.parse_args(argv)
    # setup runs from the installed copy, not from the download: it copies its reset command from there.
    installer = Installer(Terminal(), Shell(), http_post, args.source, plugin=str(package_dir() / "scripts/pi_plugin.py"),
                          downloaded=args.downloaded)
    return installer.install() if args.action == "install" else installer.uninstall()


if __name__ == "__main__":
    sys.exit(main())
