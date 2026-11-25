#!/usr/bin/env python3
"""Install or remove OpenCode's context-guru plugin, and OpenCode itself when it is missing, in one run.

install-opencode.sh downloads the release's plugin, checks its SHA-256, and runs this file from it, with the
user's answers on the terminal. See docs/superpowers/specs/2026-11-23-opencode-one-command-installer-design.md
and docs/superpowers/specs/2026-11-25-opencode-installer-provider-setup-design.md.

  * OpenCode changes only after a yes. The config files change only after the consent yes.
  * The installer never reads, asks for, or stores a key: OpenCode's own login (`opencode auth login`) does.
  * A record lists what this file added, so uninstall removes that and nothing else.
"""
import argparse
from dataclasses import dataclass
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from typing import Optional
from urllib.parse import urlsplit, urlunsplit

OPENCODE_NPM = "opencode-ai"
TESTED_OPENCODE = "1.19.5"  # only named in a note: the installer installs the latest, as the Pi installer does
MIN_OPENCODE = (1, 18, 29)  # the first release that calls the plugin's v1 server() export
PUBLIC_REGISTRY = "https://registry.npmjs.org/"
INSTALL_PAGE = "https://opencode.ai/docs"
# index.js and tui.js both carry it. A local plugin entry whose file has it is a copy of this plugin.
MARKER = "opencode-context-guru"
# (config file, plugin file in the copy, $schema for a file the installer creates)
CONFIGS = (("opencode.json", "index.js", "https://opencode.ai/config.json"),
           ("tui.json", "tui.js", "https://opencode.ai/tui.json"))
DOWNLOAD_STEP = "Download context-guru for OpenCode"
INSTALL_STEPS = ("Check this computer", "Your models", "OpenCode", "Your consent", "The context-guru plugin",
                 "Login", "Check that it works")
DIRECT = ("anthropic", "openai")  # the built-in provider ids that the plugin routes
# API format -> (provider id, AI SDK package, display name). A gateway gets its own id, so a direct login's key
# never goes to it. The plugin routes any id that contains "anthropic" or "openai" (index.js providerDialect).
GATEWAYS = {"anthropic": ("anthropic-gateway", "@ai-sdk/anthropic", "Anthropic gateway"),
            "openai": ("openai-gateway", "@ai-sdk/openai", "OpenAI gateway")}
MODELS_TEXT = """context-guru works with Anthropic and OpenAI models only.
It can reach them directly, or through a gateway that
speaks the Anthropic or OpenAI API (for example LiteLLM).
Other providers (Google, Groq, local models, ...) are not routed.

How do you reach your models?
  1) Anthropic or OpenAI directly
  2) A custom base URL (a company gateway)
  3) Another provider: exit, change nothing"""
NOT_SUPPORTED = "Nothing was changed. context-guru supports Anthropic and OpenAI models."
ABSENT = {"absent": True}  # in the record: the key was not there before the install
UNINSTALL_STEPS = ("Stop the proxies", "Remove the plugin", "Local data", "OpenCode itself")


class Stop(Exception):
    """A step cannot go on. The message says why, and what to do."""


@dataclass(frozen=True)
class Connection:
    """How OpenCode reaches the models. Directly: all None. A gateway: its provider id, `<url>/v1` and 2 models."""
    provider: Optional[str]
    base_url: Optional[str]
    model: Optional[str]
    small_model: Optional[str]


def config_dir():
    return Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config") / "opencode"


def plugin_dir():
    return Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share") / "context-guru-opencode/plugin"


def state_root():
    """The plugin's own state root (index.js stateRoot)."""
    if os.environ.get("CONTEXT_GURU_STATE"):
        return Path(os.environ["CONTEXT_GURU_STATE"])
    return Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local/state") / "context-guru-opencode"


def record_path():
    return state_root() / "installer.json"


def backup_dir():
    return state_root() / "installer-backup"


def version_tuple(text):
    """(major, minor, patch) from `1.19.5`, `opencode v1.19.5` or `1.20.0-beta.1` (a suffix is ignored, so a
    pre-release counts as its release); None when there is no version."""
    for word in text.split():
        found = re.match(r"v?(\d+)\.(\d+)\.(\d+)(?![.\d])", word)
        if found:
            return tuple(int(part) for part in found.groups())
    return None


def load_json(path):
    if not path.exists() or not path.read_text().strip():
        return {}
    try:
        value = json.loads(path.read_text())
    except ValueError:
        raise Stop(f"{path} is not plain JSON (it may have comments). The installer does not change it.\n"
                   f"{manual_steps()}") from None
    if not isinstance(value, dict):
        raise Stop(f"{path} is not a JSON object. Fix or move it, then run this again.")
    return value


def write_json(path, value):
    """Atomic: a temp file in the same folder, then a rename. The file keeps its mode (mkstemp makes 0600). A
    symlink (a dotfiles repo) stays: the write goes to the file it points to."""
    path = Path(os.path.realpath(path))
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o644
    descriptor, temp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    with os.fdopen(descriptor, "w") as handle:
        handle.write(json.dumps(value, indent=2) + "\n")
    os.chmod(temp, mode)
    os.replace(temp, path)


def gateway_url(text):
    """`<url>/v1` for a gateway base URL: https, or http on this computer only."""
    url = urlsplit(text.strip())
    local = url.hostname in ("127.0.0.1", "localhost")
    if not url.hostname or not (url.scheme == "https" or (url.scheme == "http" and local)):
        raise Stop(f"the base URL must start with https:// (http:// only on 127.0.0.1 or localhost): {text!r}. "
                   "Nothing was changed.")
    path = url.path.rstrip("/").removesuffix("/v1")
    return urlunsplit((url.scheme, url.netloc, path + "/v1", "", ""))


def dialect(provider_id):
    """The API format the plugin routes a provider id as (index.js providerDialect), or None."""
    lower = str(provider_id).lower()
    return "anthropic" if "anthropic" in lower else "openai" if "openai" in lower else None


def slot(document, key):
    """(the object that holds `key`, the name in it). `provider.<id>` is nested; the other keys are top level."""
    head, _, rest = key.partition(".")
    return (document.get(head), rest) if rest else (document, head)


def logged_in(text):
    """The provider ids with a login, from `opencode auth list`: the first word of each `●` line, lower case. A key
    in the environment (the Environment block) counts too."""
    plain = re.sub(r"\x1b\[[0-9;]*m", "", text)
    return {line.split("●", 1)[1].split()[0].lower() for line in plain.splitlines()
            if "●" in line and line.split("●", 1)[1].split()}


def manual_steps():
    lines = ["To install it by hand, add these entries to the \"plugin\" list:"]
    for name, file, _ in CONFIGS:
        lines.append(f"  {config_dir() / name}: \"{plugin_dir() / file}\"")
    return "\n".join(lines)


def project_state(folder):
    """The plugin's state folder for an OpenCode project folder (index.js stateDirectory)."""
    return state_root() / hashlib.sha256(folder.encode()).hexdigest()[:16]


def entry_path(entry):
    return entry[0] if isinstance(entry, list) and entry and isinstance(entry[0], str) else entry


def is_ours(entry, known=()):
    """A local path to a file that is a copy of this plugin, or a path the installer wrote (`known`, for a copy
    that is gone). npm names and URLs are never ours."""
    path = entry_path(entry)
    if not isinstance(path, str) or not path.startswith(("/", "~", "file://")):
        return False
    if path in known:
        return True
    try:
        return MARKER in Path(path.removeprefix("file://")).expanduser().read_text(errors="replace")
    except OSError:
        return False


def facts(text):
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


class Terminal:
    """Questions and messages on the user's terminal."""

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


class Shell:
    """Runs a program."""

    def which(self, name):
        return shutil.which(name)

    def interactive(self, cmd):
        """Runs a program on the user's terminal (a login), and returns its exit code."""
        try:
            return subprocess.run(cmd).returncode
        except FileNotFoundError:
            return 127

    def run(self, cmd, cwd=None):
        try:
            # OpenCode takes its project folder from PWD as well, so PWD must name the same folder as cwd.
            env = dict(os.environ, PWD=cwd) if cwd else None
            return subprocess.run(cmd, capture_output=True, text=True, timeout=600, cwd=cwd, env=env,
                                  stdin=subprocess.DEVNULL)
        except FileNotFoundError:
            return subprocess.CompletedProcess(cmd, 127, "", f"{cmd[0]}: not found")
        except subprocess.TimeoutExpired:
            return subprocess.CompletedProcess(cmd, 124, "", f"{cmd[0]}: no answer in 10 minutes")


class Installer:
    def __init__(self, terminal, shell, source, downloaded=False):
        self.terminal, self.shell, self.source, self.downloaded = terminal, shell, Path(source), downloaded
        self.registry = os.environ.get("CG_OC_NPM_REGISTRY") or PUBLIC_REGISTRY
        self.steps, self.record = (), {}

    # ------------------------------------------------------------------------------- install
    def install(self):
        try:
            self.begin(INSTALL_STEPS)
            self.step("Check this computer", "It reads OpenCode's config files. Nothing changes in this step.")
            documents = self.read_configs()
            self.record = load_json(record_path())
            self.step("Your models", "How OpenCode reaches the models. Nothing changes in this step.")
            self.connection = self.ask_connection(documents)
            if self.connection is None:
                self.terminal.say(NOT_SUPPORTED)
                return 0
            self.step("OpenCode", "OpenCode is the AI coding agent that context-guru works in. This step installs "
                                  "or updates OpenCode, after your yes.")
            self.ensure_opencode()
            self.save_record()
            self.step("Your consent", "What changes when context-guru is on.")
            if not self.consent(self.carried_options(documents["opencode.json"]).get("preset", "off")):
                self.terminal.say("context-guru stays off. Nothing else was changed.")
                return 0
            self.step("The context-guru plugin", f"It goes to {plugin_dir()}, and into OpenCode's config for every "
                                                 "project. Your config files are backed up first.")
            self.install_plugin(documents)
            self.save_record()
            self.login()
            return self.finish()
        except Stop as stop:
            self.terminal.say(f"context-guru: {stop}")
            return 1

    def read_configs(self):
        jsonc = config_dir() / "opencode.jsonc"
        if jsonc.exists():
            raise Stop(f"OpenCode reads {jsonc}, and the installer does not change JSONC files.\n{manual_steps()}")
        return {name: load_json(config_dir() / name) for name, _, _ in CONFIGS}

    def ask_connection(self, documents):
        """The connection from the user's answers (or the CG_OC_* variables); None for another provider."""
        env = os.environ.get
        if not env("CG_OC_GATEWAY_URL"):
            self.terminal.say(MODELS_TEXT)
            choice = None
            while choice not in ("1", "2", "3"):
                try:
                    choice = self.terminal.ask("Your choice [1]:", "1")
                except Stop:  # the input ended: the same as "another provider"
                    return None
            if choice == "3":
                return None
            if choice == "1":
                return Connection(None, None, None, None)
        url = gateway_url(env("CG_OC_GATEWAY_URL") or self.terminal.ask("The gateway's base URL:"))
        api = env("CG_OC_API")
        if api and api not in GATEWAYS:
            raise Stop(f"CG_OC_API must be anthropic or openai, not {api!r}. Nothing was changed.")
        while api not in GATEWAYS:
            api = self.terminal.ask("The gateway's API format, anthropic or openai [anthropic]:", "anthropic").lower()
        model = env("CG_OC_MODEL") or ""
        while not model:
            model = self.terminal.ask("The model, as the gateway names it (for example aws/claude-sonnet-5-5):")
        small = env("CG_OC_SMALL_MODEL") or self.terminal.ask(
            f"The small model, for session titles [{model}]:", model)
        provider = GATEWAYS[api][0]
        # The plugin refuses two upstreams of one API format, and then routes nothing.
        for other in (documents["opencode.json"].get("provider") or {}):
            if other != provider and dialect(other) == api:
                base = ((documents["opencode.json"]["provider"][other] or {}).get("options") or {}).get("baseURL")
                raise Stop(f"OpenCode's config already has the {api} provider \"{other}\""
                           f"{f' at {base}' if base else ''}. context-guru routes one {api} upstream only, so it "
                           f"cannot add {url} too. Remove \"{other}\" from {config_dir() / 'opencode.json'}, "
                           "then run this again. Nothing was changed.")
        old = (((documents["opencode.json"].get("provider") or {}).get(provider) or {}).get("options") or {}).get("baseURL")
        if old and old != url:
            self.terminal.say(f"OpenCode's config already has \"{provider}\" at {old}.")
            if not self.yes(f"Replace it with {url}?", default=False):
                raise Stop("Nothing was changed.")
        return Connection(provider, url, model.strip(), small.strip())

    @staticmethod
    def tested_note(shown):
        return "" if shown == TESTED_OPENCODE else f" The plugin is tested on OpenCode {TESTED_OPENCODE} only."

    def opencode_version(self):
        """(path of `opencode` on PATH, its version text); (None, None) when it is not on PATH."""
        path = self.shell.which("opencode")
        if not path:
            return None, None
        version = version_tuple(self.shell.run(["opencode", "--version"]).stdout)
        return path, ".".join(map(str, version)) if version else "of an unknown version"

    def latest_opencode(self):
        out = self.shell.run(["npm", "view", OPENCODE_NPM, "version", f"--registry={self.registry}"])
        if out.returncode != 0 or not version_tuple(out.stdout):
            raise Stop(f"npm cannot read the latest OpenCode version from {self.registry}:\n{out.stderr.strip()}\n"
                       "To use another registry, set CG_OC_NPM_REGISTRY.")
        return out.stdout.strip()

    def ensure_opencode(self):
        path, shown = self.opencode_version()
        version = version_tuple(shown) if path else None
        if path and not version:
            # A dev build: never offer npm for it, which could replace a newer OpenCode with an older one.
            self.terminal.say(f"The installer cannot read the version of {path}. The plugin needs OpenCode 1.18.29 "
                              "or later.")
            if not self.yes("Go on with this OpenCode?", default=True):
                raise Stop("Nothing was changed.")
            return
        if not self.shell.which("npm"):
            if version and version >= MIN_OPENCODE:
                self.terminal.say(f"OpenCode {shown} is installed.{self.tested_note(shown)}")
                return
            raise Stop(f"OpenCode {'is too old' if path else 'is not installed'}, and npm is not here to install "
                       f"it. Install the latest OpenCode from {INSTALL_PAGE}, then run this again.")
        try:
            latest = self.latest_opencode()
        except Stop as stop:
            if not (version and version >= MIN_OPENCODE):
                raise Stop(f"{stop} Nothing was changed.") from None
            # Nothing needs npm here: a broken registry must not block a supported OpenCode.
            self.terminal.say(f"The installer cannot check for a newer OpenCode: {stop}")
            self.terminal.say(f"OpenCode {shown} stays.{self.tested_note(shown)}")
            return
        target = latest  # as npm prints it: a pre-release tag keeps its suffix
        if version and version >= version_tuple(latest):
            self.terminal.say(f"OpenCode {shown} is installed.{self.tested_note(shown)}")
            return
        if path:
            if not self.yes(f"Update OpenCode {shown} to {target}?", default=True):
                if version >= MIN_OPENCODE:
                    self.terminal.say(f"OpenCode {shown} stays.")
                    return
                raise Stop("context-guru needs OpenCode 1.18.29 or later. Nothing was changed.")
        elif not self.yes(f"Install OpenCode {target}?", default=True):
            raise Stop("context-guru needs OpenCode. Nothing was changed.")
        self.target = target
        out = self.shell.run(["npm", "install", "-g", f"--registry={self.registry}", f"{OPENCODE_NPM}@{self.target}"])
        if out.returncode != 0:
            raise Stop(f"npm could not install OpenCode:\n{out.stderr.strip()}\n"
                       "If npm cannot write its global folder, fix npm's prefix or install OpenCode yourself. "
                       "To use another registry, set CG_OC_NPM_REGISTRY. This installer never uses sudo.")
        if not path:
            self.record["opencode_installed"] = True
            self.save_record()
        # npm's global bin can be missing from PATH, or come after an older OpenCode (Homebrew, ~/.opencode/bin).
        now, now_shown = self.opencode_version()
        if not now:
            raise Stop(f"npm installed OpenCode {self.target}, but `opencode` is not on your PATH. Add npm's global "
                       "bin folder (`npm prefix -g`, then /bin) to PATH, then run this again.")
        if not version_tuple(now_shown) or version_tuple(now_shown) < MIN_OPENCODE:
            raise Stop(f"npm installed OpenCode {self.target}, but `opencode` still runs OpenCode {now_shown} "
                       f"({now}). Remove that one, or put npm's global bin folder first on PATH, then run this again.")
        if version_tuple(now_shown) < version_tuple(target):
            self.terminal.say(f"npm installed OpenCode {target}, but `opencode` still runs OpenCode {now_shown} ({now}). "
                              "That one works with the plugin. To use the new one, put npm's global bin folder first "
                              "on PATH.")
            return
        self.terminal.say(f"✓ OpenCode {now_shown} {'updated' if path else 'installed'}.")

    @staticmethod
    def carried_options(document):
        """The options of an older entry of this plugin, which the new entry keeps."""
        entries = document.get("plugin") if isinstance(document.get("plugin"), list) else []
        return next((entry[1] for entry in entries if is_ours(entry) and isinstance(entry, list) and len(entry) == 2
                     and isinstance(entry[1], dict)), {})

    def consent(self, preset):
        self.terminal.say(
            "   • In every project, OpenCode's Anthropic and OpenAI requests go through a local context-guru proxy\n"
            "     on 127.0.0.1. Your requests and their answers pass through it.\n"
            + (f"   • OpenCode reaches the models through the gateway at {urlsplit(self.connection.base_url).hostname}.\n"
               if self.connection.provider else "   • OpenCode reaches Anthropic or OpenAI directly.\n")
            +
            "   • While you are away, the proxy sends short keep-alive requests with your own credential, to keep\n"
            "     the prompt cache warm. They cost a little. /context-guru-strategy none turns them off.\n"
            + ("   • The preset is off: request bodies go to the provider unchanged." if preset == "off" else
               f"   • The preset is {preset} (from your old plugin entry): context-guru trims what it sends.")
            + " /context-guru-preset changes it.\n"
            "   • If the proxy is down, requests go straight to the provider.")
        return self.yes("Turn context-guru on for OpenCode?", default=False)

    def install_plugin(self, documents):
        target = Path(os.path.abspath(plugin_dir()))
        if target.exists():
            shutil.rmtree(target)
        shutil.copytree(self.source, target, ignore=shutil.ignore_patterns(
            "test", "release", "scripts", "__pycache__", "*.pyc", ".DS_Store"))
        self.record["plugin"] = str(target)
        files = self.record.setdefault("files", {})
        for name, file, schema in CONFIGS:
            path = config_dir() / name
            document = documents[name]
            files.setdefault(name, {"created": not path.exists(), "had_plugin": "plugin" in document})
            if path.exists():
                backup_dir().mkdir(parents=True, exist_ok=True)
                shutil.copy2(path, backup_dir() / name)
            elif not document:
                document["$schema"] = schema
            if name == "opencode.json":
                self.write_connection(document)
            entries = document.get("plugin") if isinstance(document.get("plugin"), list) else []
            ours = [entry for entry in entries if is_ours(entry)]
            options = self.carried_options(document)
            new = str(target / file)
            document["plugin"] = [entry for entry in entries if not is_ours(entry)]
            document["plugin"].append([new, options] if options else new)
            write_json(path, document)
            for entry in ours:
                if entry_path(entry) != new:
                    self.terminal.say(f"Replaced another copy of the plugin in {name}: {entry_path(entry)}")
        self.terminal.say(f"✓ context-guru plugin installed for every project ({config_dir()}).")

    def logins(self):
        return logged_in(self.shell.run(["opencode", "auth", "list"]).stdout)

    def login(self):
        self.step("Login", "OpenCode keeps your key in its own login store. The installer never sees the key.")
        gateway = self.connection.provider
        wanted = {gateway} if gateway else set(DIRECT)
        if wanted & self.logins():
            self.terminal.say(f"✓ OpenCode already has a login for {gateway or 'Anthropic or OpenAI'}.")
            return
        if gateway:
            self.terminal.say("OpenCode asks for the provider next. In the list, pick Other (the last choice), then "
                              f"type: {gateway}\nThen paste your gateway key.")
        else:
            self.terminal.say("OpenCode asks for the provider next. Pick Anthropic or OpenAI, then log in.")
        before = self.logins()
        self.shell.interactive(["opencode", "auth", "login"])
        after = self.logins()
        if wanted & after:
            self.record["login_started"] = sorted(wanted & after)[0]
            self.save_record()
            self.terminal.say("✓ OpenCode has the login.")
            return
        other = sorted(after - before)
        self.terminal.say(f"context-guru cannot route {', '.join(other)}. It routes Anthropic and OpenAI only." if other
                          else f"OpenCode has no login for {gateway or 'Anthropic or OpenAI'}.")
        if self.yes("Undo the changes of this install?", default=True):
            self.undo()
            raise Stop("the install is undone.")
        raise Stop(f"to finish, run: opencode auth login{f' (pick Other, then type: {gateway})' if gateway else ''}. "
                   "Then run this installer again.")

    def undo(self):
        """Uninstall without its questions: config entries and keys, the plugin copy, a login the installer started.
        An OpenCode that the installer installed stays, and the record keeps that fact for uninstall."""
        self.revert(self.record)
        if self.record.get("plugin"):
            shutil.rmtree(self.record["plugin"], ignore_errors=True)
        if self.record.get("login_started"):
            self.shell.run(["opencode", "auth", "logout", self.record["login_started"]])
        shutil.rmtree(backup_dir(), ignore_errors=True)
        if self.record.get("opencode_installed"):
            self.record = {"opencode_installed": True}
            self.save_record()
            self.terminal.say("✓ The changes are undone. OpenCode stays installed. The uninstall removes it.")
        else:
            record_path().unlink(missing_ok=True)
            self.terminal.say("✓ The changes are undone.")

    def write_connection(self, document):
        """A gateway's provider and models into opencode.json. The record keeps each key's value from before the
        first install, so uninstall can put it back."""
        connection = self.connection
        if not connection.provider:
            return
        provider, npm, name = GATEWAYS[dialect(connection.provider)]
        models = dict.fromkeys((connection.model, connection.small_model))
        wrote = {f"provider.{provider}": {"npm": npm, "name": name, "options": {"baseURL": connection.base_url},
                                          "models": {model: {"name": model} for model in models}},
                 "model": f"{provider}/{connection.model}", "small_model": f"{provider}/{connection.small_model}"}
        made_provider = "provider" not in document
        if not isinstance(document.setdefault("provider", {}), dict):
            raise Stop(f"\"provider\" in {config_dir() / 'opencode.json'} is not a JSON object. Fix it, then run "
                       "this again.")
        old = self.record.get("connection") or {}
        before = {}
        for key in wrote:
            holder, field = slot(document, key)
            before[key] = holder[field] if field in holder else ABSENT
        for key, value in wrote.items():
            holder, field = slot(document, key)
            holder[field] = value
        # A second run keeps the first run's "before", and what each run wrote.
        self.record["connection"] = {"wrote": {**old.get("wrote", {}), **wrote},
                                     "before": {**before, **old.get("before", {})},
                                     "made_provider": old.get("made_provider", made_provider)}

    @staticmethod
    def restore_connection(document, connection):
        """Put back each key the installer wrote, when it still has the installer's value. True when it changed."""
        changed = False
        for key, wrote in (connection or {}).get("wrote", {}).items():
            holder, field = slot(document, key)
            if not isinstance(holder, dict) or holder.get(field) != wrote:
                continue
            before = connection["before"].get(key, ABSENT)
            if before == ABSENT:
                del holder[field]
            else:
                holder[field] = before
            changed = True
        if changed and document.get("provider") == {} and connection.get("made_provider"):
            del document["provider"]
        return changed

    def finish(self):
        """Exit code 0 only when a real answer came through the proxy, or the user said no to the check."""
        self.step("Check that it works", "One short test prompt through OpenCode starts the proxy and checks it.")
        if not self.yes("Send one test prompt now? It costs one short request.", default=True):
            self.terminal.say("Not checked. Restart any OpenCode that is open now. Then start OpenCode with: opencode")
            return 0
        with tempfile.TemporaryDirectory(prefix="cg-opencode-check.") as folder:
            folder = os.path.realpath(folder)
            out = self.shell.run(["opencode", "run", "Reply with the single word: ok"], cwd=folder)
            self.terminal.say(f"OpenCode answered: {out.stdout.strip()[-200:] or out.stderr.strip()[-200:]}")
            state = project_state(folder)
            status, reset = state / "context-guru-status", state / "context-guru-reset"
            found = facts(self.shell.run(["sh", str(status)]).stdout) if status.exists() else {}
            # The check's own proxy would idle for 24 h (and send keep-alive) for a folder that is gone.
            if reset.exists():
                self.shell.run(["sh", str(reset)])
            shutil.rmtree(state, ignore_errors=True)
        # opencode run can print a provider error and still exit 0, so its text counts too.
        answered = out.returncode == 0 and "Error:" not in out.stdout + out.stderr
        requests = int(float(found.get("stats_requests") or 0))
        if found.get("proxy") != "up":
            self.terminal.say(f"The proxy did not start. Its log is under {state_root()}.")
        elif not answered:
            self.terminal.say("The proxy started, but OpenCode could not answer (see above). Check the key "
                              "(opencode auth login), the base URL and the model.")
        elif requests < 1:
            self.terminal.say("OpenCode answered, but not through the proxy: the proxy counted 0 requests. The "
                              "provider of OpenCode's model is not Anthropic or OpenAI.")
        else:
            self.terminal.say("✓ The proxy started, and OpenCode answered through it.")
            self.terminal.say("Done. Restart any OpenCode that is open now. Then start OpenCode with: opencode")
            return 0
        return 1

    # ----------------------------------------------------------------------------- uninstall
    def uninstall(self):
        try:
            self.begin(UNINSTALL_STEPS)
            record = load_json(record_path())
            self.step("Stop the proxies", "It stops each project's context-guru proxy.")
            resets = sorted(state_root().glob("*/context-guru-reset")) if state_root().exists() else []
            for reset in resets:
                self.shell.run(["sh", str(reset)])
            self.terminal.say(f"✓ Proxies stopped ({len(resets)} projects).")
            self.step("Remove the plugin", "It removes each context-guru entry from OpenCode's config, also one you "
                                           "added by hand. Your other entries stay.")
            by_hand = self.revert(record)
            if record.get("login_started"):
                self.shell.run(["opencode", "auth", "logout", record["login_started"]])
            if record.get("plugin"):
                shutil.rmtree(record["plugin"], ignore_errors=True)
                try:
                    Path(record["plugin"]).parent.rmdir()  # only when it is empty
                except OSError:
                    pass
            self.terminal.say("✓ context-guru plugin removed. Restart any OpenCode that is open now.")
            self.step("Local data", f"context-guru's logs, dashboard databases and proxy binaries, in {state_root()}.")
            if self.yes("Delete context-guru's local data too?", default=True):
                shutil.rmtree(state_root(), ignore_errors=True)
            else:
                record_path().unlink(missing_ok=True)
                shutil.rmtree(backup_dir(), ignore_errors=True)
            if record.get("opencode_installed"):
                self.step("OpenCode itself", "The installer installed OpenCode, so it can remove it too. Your "
                                             "OpenCode settings stay.")
                if self.yes("Uninstall OpenCode too?", default=False):
                    out = self.shell.run(["npm", "uninstall", "-g", OPENCODE_NPM])
                    self.terminal.say("✓ OpenCode uninstalled." if out.returncode == 0 else f"npm: {out.stderr.strip()}")
            else:
                self.skip("OpenCode itself", "the installer did not install OpenCode, so it stays.")
            if by_hand:
                self.terminal.say("context-guru is stopped and its files are gone, but the installer does not change "
                                  "a file with comments or a JSONC file:\n  " + "\n  ".join(by_hand))
                return 1
            self.terminal.say("Done. context-guru is removed.")
            return 0
        except Stop as stop:
            self.terminal.say(f"context-guru: {stop}")
            return 1

    def revert(self, record):
        """Remove the plugin entries and put back the connection keys. Returns the lines to change by hand."""
        files = record.get("files") or {}
        known = {str(Path(record["plugin"]) / file) for _, file, _ in CONFIGS} if record.get("plugin") else set()
        by_hand = []
        for name, file, schema in CONFIGS:
            path = config_dir() / name
            try:
                document = load_json(path)
                changed = name == "opencode.json" and self.restore_connection(document, record.get("connection"))
                self.remove_entries(path, document, files.get(name) or {}, schema, known, changed)
            except Stop:
                by_hand.append(f"{path}: remove this entry by hand: \"{plugin_dir() / file}\"")
        jsonc = config_dir() / "opencode.jsonc"
        if jsonc.exists():
            by_hand.append(f"{jsonc}: remove a context-guru entry by hand, if it has one")
        return by_hand

    @staticmethod
    def remove_entries(path, document, record, schema, known, changed=False):
        entries = document.get("plugin")
        ours = isinstance(entries, list) and any(is_ours(entry, known) for entry in entries)
        if not ours and not changed:
            return
        if ours:
            document["plugin"] = [entry for entry in entries if not is_ours(entry, known)]
            if not document["plugin"] and record.get("had_plugin") is False:
                del document["plugin"]
        if record.get("created") and set(document) <= {"$schema"} and document.get("$schema", schema) == schema:
            path.unlink()
        else:
            write_json(path, document)

    # ------------------------------------------------------------------------------- helpers
    def begin(self, names):
        self.steps = ((DOWNLOAD_STEP,) if self.downloaded else ()) + tuple(names)
        if self.downloaded:
            self.step(DOWNLOAD_STEP, "Done: the plugin matches the release's SHA-256, so it is the file the "
                                     "release published.")

    def header(self, name):
        return f"Step {self.steps.index(name) + 1} of {len(self.steps)} · {name}"

    def step(self, name, explanation):
        self.terminal.say("")
        self.terminal.say(f"{self.header(name)}\n   {explanation}")

    def skip(self, name, why):
        self.terminal.say("")
        self.terminal.say(f"{self.header(name)} — skipped: {why}")

    def yes(self, question, default):
        answer = self.terminal.ask(f"{question} [{'Y/n' if default else 'y/N'}]", "y" if default else "n")
        return answer.lower() in ("y", "yes")

    def save_record(self):
        if self.record:
            write_json(record_path(), self.record)


def main(argv=None):
    parser = argparse.ArgumentParser(description="Install or remove context-guru for OpenCode.")
    parser.add_argument("action", choices=["install", "uninstall"], nargs="?", default="install")
    parser.add_argument("--source", default=str(Path(__file__).resolve().parent.parent),
                        help="the unpacked opencode-plugin folder to install")
    parser.add_argument("--downloaded", action="store_true",
                        help="install-opencode.sh downloaded and checked it (step 1)")
    args = parser.parse_args(argv)
    installer = Installer(Terminal(), Shell(), args.source, downloaded=args.downloaded)
    return installer.install() if args.action == "install" else installer.uninstall()


if __name__ == "__main__":
    sys.exit(main())
