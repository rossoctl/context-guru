#!/usr/bin/env python3
"""Install or remove OpenCode's context-guru plugin, and OpenCode itself when it is missing, in one run.

install-opencode.sh downloads the release's plugin, checks its SHA-256, and runs this file from it, with the
user's answers on the terminal. See docs/superpowers/specs/2026-11-23-opencode-one-command-installer-design.md.

  * OpenCode changes only after a yes. The config files change only after the consent yes.
  * The installer never reads, asks for, or stores a key: the plugin uses OpenCode's own login.
  * A record lists what this file added, so uninstall removes that and nothing else.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

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
INSTALL_STEPS = ("Check this computer", "OpenCode", "Your consent", "The context-guru plugin", "Check that it works")
UNINSTALL_STEPS = ("Stop the proxies", "Remove the plugin", "Local data", "OpenCode itself")


class Stop(Exception):
    """A step cannot go on. The message says why, and what to do."""


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
    """Atomic: a temp file in the same folder, then a rename. The file keeps its mode (mkstemp makes 0600)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o644
    descriptor, temp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    with os.fdopen(descriptor, "w") as handle:
        handle.write(json.dumps(value, indent=2) + "\n")
    os.chmod(temp, mode)
    os.replace(temp, path)


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
            self.finish()
            return 0
        except Stop as stop:
            self.terminal.say(f"context-guru: {stop}")
            return 1

    def read_configs(self):
        jsonc = config_dir() / "opencode.jsonc"
        if jsonc.exists():
            raise Stop(f"OpenCode reads {jsonc}, and the installer does not change JSONC files.\n{manual_steps()}")
        return {name: load_json(config_dir() / name) for name, _, _ in CONFIGS}

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

    def finish(self):
        self.step("Check that it works", "One short test prompt through OpenCode starts the proxy and checks it.")
        if self.yes("Send one test prompt now? It costs one short request.", default=False):
            with tempfile.TemporaryDirectory(prefix="cg-opencode-check.") as folder:
                folder = os.path.realpath(folder)
                out = self.shell.run(["opencode", "run", "Reply with the single word: ok"], cwd=folder)
                self.terminal.say(f"OpenCode answered: {out.stdout.strip()[-200:] or out.stderr.strip()[-200:]}")
                state = project_state(folder)
                status, reset = state / "context-guru-status", state / "context-guru-reset"
                proxy = facts(self.shell.run(["sh", str(status)]).stdout).get("proxy") if status.exists() else None
                # The check's own proxy would idle for 24 h (and send keep-alive) for a folder that is gone.
                if reset.exists():
                    self.shell.run(["sh", str(reset)])
                shutil.rmtree(state, ignore_errors=True)
            # opencode run can print a provider error and still exit 0, so its text counts too.
            answered = out.returncode == 0 and "Error:" not in out.stdout + out.stderr
            if proxy == "up" and answered:
                self.terminal.say("✓ The proxy started, and OpenCode answered through it.")
            elif proxy == "up":
                self.terminal.say("The proxy started, but OpenCode could not answer (see above). If OpenCode has "
                                  "no provider login yet, run: opencode auth login")
            else:
                self.terminal.say("The proxy did not answer. If OpenCode has no provider login yet, run: "
                                  f"opencode auth login. The proxy log is under {state_root()}.")
        self.terminal.say("Done. Restart any OpenCode that is open now. Then start OpenCode with: opencode")

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
            files = record.get("files") or {}
            known = {str(Path(record["plugin"]) / file) for _, file, _ in CONFIGS} if record.get("plugin") else set()
            by_hand = []
            for name, file, schema in CONFIGS:
                path = config_dir() / name
                try:
                    self.remove_entries(path, load_json(path), files.get(name) or {}, schema, known)
                except Stop:
                    by_hand.append(f"{path}: remove this entry by hand: \"{plugin_dir() / file}\"")
            jsonc = config_dir() / "opencode.jsonc"
            if jsonc.exists():
                by_hand.append(f"{jsonc}: remove a context-guru entry by hand, if it has one")
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

    @staticmethod
    def remove_entries(path, document, record, schema, known):
        entries = document.get("plugin")
        if not isinstance(entries, list) or not any(is_ours(entry, known) for entry in entries):
            return
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
