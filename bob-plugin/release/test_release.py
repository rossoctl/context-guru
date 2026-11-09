"""Tests for the Bob release package: the installer, the uninstaller and package.sh.

Run: python3 -m unittest discover -s bob-plugin/release -p 'test_*.py'

Nothing here touches the real HOME, the real launchd or the real IBM Bob. Each test builds a
temp root with a temp HOME, a fake Bob.app, a release folder with fake proxy binaries, and fakes
first on PATH: launchctl (it really starts the plist's ProgramArguments, so /healthz answers),
bobide, pgrep, osascript (for the Bob quit step only) and uname. The installer and uninstaller
run as subprocesses with an environment built from scratch, so no real path leaks in.
"""

import functools
import hashlib
import http.server
import os
from pathlib import Path
import plistlib
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.request
import zipfile

HERE = Path(__file__).resolve().parent
PLUGIN_SRC = HERE.parent / "context-guru"
INSTALLER_NAME = "Install context-guru for Bob.command"
UNINSTALLER_NAME = "Uninstall context-guru for Bob.command"
PACKAGER = HERE / "package.sh"
BOOTSTRAP = HERE / "install-bob.sh"
PY = sys.executable
CANARY = "CANARY-7f3a9c-user-content-do-not-log"
LOG_REL = "Library/Logs/context-guru-bob-install.log"
SUCCESS_LINE = "Open IBM Bob from the Dock. Bob asks you to log in once — this is expected."

# A context-guru-proxy stand-in. --help prints the flag list; else it serves /healthz on --listen.
FAKE_PROXY = """#!{py}
import http.server, sys
args = sys.argv[1:]
if "--help" in args:
    print("Usage of context-guru-proxy:")
    print("  {flag} string")
    print("fake-arch={arch}")
    sys.exit(0)
if {dies}:
    sys.exit(1)
import time
time.sleep({slow})
host, port = args[args.index("--listen") + 1].rsplit(":", 1)
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        if self.path == "/healthz":
            body = b"ok"
        elif self.path.startswith("/api/requests"):
            body = b'{{"requests": []}}'
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)
http.server.HTTPServer((host, int(port)), Handler).serve_forever()
"""

# launchctl stand-in. bootstrap starts ProgramArguments in the background; bootout stops it.
FAKE_LAUNCHCTL = """#!{py}
import os, plistlib, signal, subprocess, sys, time
root = os.environ["CG_TEST_ROOT"]
with open(os.path.join(root, "launchctl.log"), "a") as log:
    log.write(" ".join(sys.argv[1:]) + "\\n")
pidfile = os.path.join(root, "launchd.pid")
argsfile = os.path.join(root, "launchd.args")

def pid():
    try:
        value = int(open(pidfile).read())
        os.kill(value, 0)
        return value
    except (OSError, ValueError):
        return None

def start():
    arguments = open(argsfile).read().split("\\0")
    out = open(os.path.join(root, "launchd-child.log"), "a")
    child = subprocess.Popen(arguments, stdout=out, stderr=out, stdin=subprocess.DEVNULL,
                             start_new_session=True)
    open(pidfile, "w").write(str(child.pid))

def stop():
    value = pid()
    if value is None:
        return
    os.kill(value, signal.SIGTERM)
    for _ in range(50):
        if pid() is None:
            break
        time.sleep(0.1)
    os.unlink(pidfile)

command = sys.argv[1]
if command == "bootstrap":
    with open(sys.argv[3], "rb") as handle:
        open(argsfile, "w").write("\\0".join(plistlib.load(handle)["ProgramArguments"]))
    if pid() is None:
        start()
elif command == "bootout":
    stop()
elif command == "print":
    sys.exit(0 if pid() is not None else 113)
elif command == "kickstart":
    if "-k" in sys.argv:
        stop()
    if pid() is None and os.path.exists(argsfile):
        start()
sys.exit(0)
"""

FAKE_BOBIDE = """#!/bin/sh
db="$CG_TEST_ROOT/bobide.db"
case "$1" in
  --install-extension) echo rossoctl.context-guru-bob > "$db" ;;
  --list-extensions) [ -f "$db" ] && cat "$db" ;;
  --uninstall-extension) rm -f "$db" ;;
esac
exit 0
"""

# Bob "runs" while the file bob-running exists. The quit removes it, unless bob-stuck exists.
FAKE_PGREP = """#!/bin/sh
printf '%s\\n' "$*" >> "$CG_TEST_ROOT/pgrep.log"
if [ -f "$CG_TEST_ROOT/bob-running" ]; then echo 4242; exit 0; fi
exit 1
"""

# Only `-e` scripts are faked (and never passed on). reset.sh's `-l JavaScript FILE` runs for real.
FAKE_OSASCRIPT = """#!/bin/sh
if [ "$1" = -e ]; then
  printf '%s\\n' "$2" >> "$CG_TEST_ROOT/osascript.log"
  case "$2" in
    *'quit app "IBM Bob"'*) [ -f "$CG_TEST_ROOT/bob-stuck" ] || rm -f "$CG_TEST_ROOT/bob-running" ;;
  esac
  exit 0
fi
exec /usr/bin/osascript "$@"
"""

FAKE_UNAME = """#!/bin/sh
if [ "$1" = -m ] && [ -n "${CG_TEST_ARCH:-}" ]; then echo "$CG_TEST_ARCH"; exit 0; fi
exec /usr/bin/uname "$@"
"""

# The strings doctor's bundle check needs, so a fake Bob passes it.
FAKE_BUNDLE_JS = ('VITE_GATEWAY_BASE_URL region_domain "/inference/v1/chat/completions" X-Task-Id '
                  'SessionStart UserPromptSubmit PreCompact PostCompact PreToolUse PostToolUse '
                  'PermissionRequest PermissionResolved Stop "Tool does not exist"\n')


def write_exec(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o755)


def fake_proxy(arch, has_bob=True, dies=False, slow=0):
    return FAKE_PROXY.format(py=PY, arch=arch, dies=dies, slow=slow,
                             flag="-bob-upstream" if has_bob else "-openai-upstream")


def write_sums(folder):
    lines = []
    for path in sorted(p for p in folder.rglob("*") if p.is_file() and p.name != "SHA256SUMS"):
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        lines.append(f"{digest}  {path.relative_to(folder)}\n")
    (folder / "SHA256SUMS").write_text("".join(lines))


def healthz(port):
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as reply:
            return reply.read() == b"ok"
    except Exception:
        return False


@unittest.skipUnless(sys.platform == "darwin",
                     "the .command files are macOS only: they need xattr, plutil and osascript")
class Release(unittest.TestCase):
    """A temp root: home/, a release folder bundle/, fakes in fakebin/, a fake Bob.app."""

    HOME_NAME = "home"

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.home = self.root / self.HOME_NAME
        self.home.mkdir()
        self.fakebin = self.root / "fakebin"
        write_exec(self.fakebin / "launchctl", FAKE_LAUNCHCTL.format(py=PY))
        write_exec(self.fakebin / "pgrep", FAKE_PGREP)
        write_exec(self.fakebin / "osascript", FAKE_OSASCRIPT)
        write_exec(self.fakebin / "uname", FAKE_UNAME)
        self.bob_app = self.home / "Applications/IBM Bob.app"
        self.bobide = self.bob_app / "Contents/Resources/app/bin/bobide"
        write_exec(self.bobide, FAKE_BOBIDE)
        with open(self.bob_app / "Contents/Info.plist", "wb") as handle:
            plistlib.dump({"CFBundleShortVersionString": "2.3.0"}, handle)
        bundle_js = self.bob_app / "Contents/Resources/app/extensions/bob-code/dist/extension.js"
        bundle_js.parent.mkdir(parents=True)
        bundle_js.write_text(FAKE_BUNDLE_JS)
        # The user's own files, with a canary that must never reach the log.
        self.rc = self.home / ".bash_profile"
        self.rc.write_bytes(f"# my profile\nexport CG_TEST_NOTE={CANARY}\n".encode())
        self.settings = self.home / ".bob/settings/settings.json"
        self.settings.parent.mkdir(parents=True)
        self.settings.write_bytes(f'{{\n  "approval": "{CANARY}"\n}}\n'.encode())
        self.bundle = self.root / "bundle"
        self.make_bundle()
        self.env = {
            "HOME": str(self.home),
            "SHELL": "/bin/bash",
            "PATH": f"{self.fakebin}:{Path(PY).parent}:/usr/bin:/bin:/usr/sbin:/sbin",
            "TMPDIR": str(self.root),
            "LANG": "en_US.UTF-8",
            "USER": os.environ.get("USER", "test"),
            "CG_TEST_ROOT": str(self.root),
            "CG_TEST_ARCH": "arm64",
            "CG_BOB_QUIT_WAIT_S": "2",
            "CONTEXT_GURU_BOB_APP": str(self.bob_app),
            "CONTEXT_GURU_BOBIDE": str(self.bobide),
        }

    def tearDown(self):
        # Stop a proxy the fake launchctl started, whatever the test did.
        try:
            os.kill(int((self.root / "launchd.pid").read_text()), signal.SIGTERM)
        except (OSError, ValueError):
            pass
        self.temp.cleanup()

    def make_bundle(self, arm64=None, amd64=None, vsix=True):
        if self.bundle.exists():
            shutil.rmtree(self.bundle)
        self.bundle.mkdir()
        for name in (INSTALLER_NAME, UNINSTALLER_NAME):
            shutil.copy2(HERE / name, self.bundle / name)
        shutil.copytree(PLUGIN_SRC, self.bundle / "plugin",
                        ignore=shutil.ignore_patterns("__pycache__", "test_*.py", "*.vsix"))
        if vsix:
            (self.bundle / "plugin/statusbar/context-guru-bob.vsix").write_bytes(b"PK fake vsix")
        write_exec(self.bundle / "bin/darwin-arm64/context-guru-proxy", arm64 or fake_proxy("arm64"))
        write_exec(self.bundle / "bin/darwin-amd64/context-guru-proxy", amd64 or fake_proxy("amd64"))
        write_sums(self.bundle)

    def run_script(self, name, answers="", *args, folder=None):
        script = (folder or self.bundle) / name
        return subprocess.run(["/bin/bash", str(script), *args], input=answers, text=True,
                              capture_output=True, env=self.env, cwd=str(self.root), timeout=180)

    def install(self, answers):
        return self.run_script(INSTALLER_NAME, answers)

    def uninstall(self, answers, folder=None):
        return self.run_script(UNINSTALLER_NAME, answers, folder=folder)

    def files(self):
        """Every file and link under HOME with its bytes, except our own install log."""
        result = {}
        for path in sorted(self.home.rglob("*")):
            rel = str(path.relative_to(self.home))
            if rel == LOG_REL or (path.is_dir() and not path.is_symlink()):
                continue
            result[rel] = os.readlink(path) if path.is_symlink() else path.read_bytes()
        return result

    def state(self):
        return self.home / ".local/state/context-guru-bob"

    def plugin_dest(self):
        return self.home / ".bob/plugins/context-guru"

    def plist(self):
        return self.home / "Library/LaunchAgents/com.rossoctl.context-guru.bob.plist"

    def launchctl_calls(self):
        log = self.root / "launchctl.log"
        return log.read_text().splitlines() if log.exists() else []

    def log_text(self):
        log = self.home / LOG_REL
        return log.read_text() if log.exists() else ""

    def port(self):
        pid_args = (self.root / "launchd.args").read_text().split("\0")
        return int(pid_args[pid_args.index("--listen") + 1].rsplit(":", 1)[1])

    def plant_history(self):
        """A dashboard database, as the real proxy writes one. It is the user's data."""
        history = self.state() / f"dashboard-{self.port()}.db"
        history.write_bytes(b"history")
        return history

    def assert_clean(self, before):
        self.assertEqual(self.files(), before)
        self.assertFalse(self.plugin_dest().exists())
        self.assertFalse(self.state().exists())
        self.assertFalse(self.plist().exists())
        self.assertFalse((self.root / "bobide.db").exists())

    def assert_installed(self, done):
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertNotIn("✗", done.stdout)
        self.assertIn(SUCCESS_LINE, done.stdout)


class InstallTest(Release):
    def test_creates_bobs_missing_playground_folder_and_uninstall_keeps_it(self):
        # Bob runs a chat with no folder open in ~/.bob/playground but never creates it, so every
        # shell command in that chat fails. The folder is Bob's, so the uninstaller keeps it.
        playground = self.home / ".bob/playground"
        self.assertFalse(playground.exists())

        self.assert_installed(self.install("y\n"))
        self.assertTrue(playground.is_dir())
        self.assertIn("playground=created", self.log_text())

        self.assertEqual(self.uninstall("y\ny\n").returncode, 0)
        self.assertTrue(playground.is_dir())

    def test_leaves_an_existing_playground_folder_as_it_is(self):
        playground = self.home / ".bob/playground"
        playground.mkdir(parents=True)
        (playground / "work.txt").write_text("mine")

        self.assert_installed(self.install("y\n"))

        self.assertEqual((playground / "work.txt").read_text(), "mine")
        self.assertIn("playground=present", self.log_text())

    def test_happy_path_installs_then_uninstall_restores_every_byte(self):
        before = self.files()
        profile, settings = self.rc.read_bytes(), self.settings.read_bytes()

        done = self.install("y\n")

        self.assert_installed(done)
        for row in ("✓ setup", "✓ proxy /healthz", "✓ doctor", "✓ status", "✓ status bar"):
            self.assertIn(row, done.stdout)
        self.assertTrue(healthz(self.port()))
        self.assertTrue((self.plugin_dest() / "scripts/bob_plugin.py").is_file())
        self.assertTrue((self.state() / "install.json").is_file())
        self.assertIn(b"# >>> context-guru (bob) >>>", self.rc.read_bytes())
        self.assertEqual((self.root / "bobide.db").read_text(), "rossoctl.context-guru-bob\n")
        port = self.port()

        removed = self.uninstall("y\n")

        self.assertEqual(removed.returncode, 0, removed.stdout + removed.stderr)
        self.assertNotIn("✗", removed.stdout)
        self.assertIn("Open Bob from the Dock and log in once", removed.stdout)
        self.assertEqual(self.rc.read_bytes(), profile)
        self.assertEqual(self.settings.read_bytes(), settings)
        self.assert_clean(before)
        self.assertFalse(healthz(port))

    def test_installed_copies_are_0755_and_not_quarantined(self):
        # A zip downloaded from GitHub gives every file this attribute.
        for path in (self.bundle / "bin/darwin-arm64/context-guru-proxy",
                     self.bundle / "plugin/scripts/bob_plugin.py"):
            subprocess.run(["/usr/bin/xattr", "-w", "com.apple.quarantine",
                            "0081;00000000;Safari;", str(path)], check=True)

        done = self.install("y\n")

        self.assert_installed(done)
        binary = self.state() / "bin/context-guru-proxy"
        self.assertEqual(binary.stat().st_mode & 0o777, 0o755)
        for path in (binary, self.plugin_dest() / "scripts/bob_plugin.py"):
            attrs = subprocess.run(["/usr/bin/xattr", str(path)], capture_output=True, text=True)
            self.assertNotIn("com.apple.quarantine", attrs.stdout, path)

    def test_consent_no_changes_nothing(self):
        before = self.files()

        done = self.install("n\n")

        self.assertEqual(done.returncode, 0)
        self.assertIn("Nothing changed.", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_consent_defaults_to_no_on_empty_input(self):
        before = self.files()

        done = self.install("")

        self.assertEqual(done.returncode, 0)
        self.assertIn("Nothing changed.", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_plan_is_shown_before_the_question(self):
        done = self.install("n\n")

        self.assertIn("result=planned", done.stdout)
        self.assertLess(done.stdout.index("result=planned"), done.stdout.index("[y/N]"))

    def test_bob_not_installed_stops_and_changes_nothing(self):
        self.env["CONTEXT_GURU_BOB_APP"] = str(self.root / "no-bob/IBM Bob.app")
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("Install IBM Bob first", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_python_missing_stops_with_command_line_tools_message(self):
        write_exec(self.fakebin / "xcode-select", "#!/bin/sh\nexit 2\n")
        write_exec(self.fakebin / "python3", "#!/bin/sh\nexit 1\n")
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("xcode-select --install", done.stdout)
        self.assertIn("run this file again", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_damaged_package_stops_and_changes_nothing(self):
        (self.bundle / "plugin/scripts/bob_plugin.py").write_text("tampered\n")
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("SHA256SUMS", done.stdout)
        self.assertEqual(self.files(), before)

    def test_binary_without_bob_gateway_is_rolled_back(self):
        self.make_bundle(arm64=fake_proxy("arm64", has_bob=False))
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("✗", done.stdout)
        self.assertIn("Bob support", done.stdout)
        self.assertIn(str(self.home / LOG_REL), done.stdout)
        self.assert_clean(before)

    def test_setup_failure_is_rolled_back(self):
        self.make_bundle(arm64=fake_proxy("arm64", dies=True))
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("✗ setup", done.stdout)
        self.assertIn("Undone", done.stdout)
        self.assert_clean(before)

    def test_already_installed_quit_changes_nothing(self):
        self.assert_installed(self.install("y\n"))
        before = self.files()
        calls = len(self.launchctl_calls())

        done = self.install("q\n")

        self.assertEqual(done.returncode, 0)
        self.assertIn("already installed", done.stdout)
        self.assertIn("Nothing changed.", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(len(self.launchctl_calls()), calls)

    def test_already_installed_reinstall_removes_then_installs(self):
        original = self.rc.read_bytes()
        self.assert_installed(self.install("y\n"))

        done = self.install("r\ny\n")

        self.assert_installed(done)
        self.assertIn("Removing the old install", done.stdout)
        # One block, after the user's own bytes. The port may differ from the first install.
        self.assertTrue(self.rc.read_bytes().startswith(original))
        self.assertEqual(self.rc.read_bytes().count(b"# >>> context-guru (bob) >>>"), 1)
        self.assertTrue(healthz(self.port()))

    def test_package_with_an_unlisted_file_is_refused(self):
        (self.bundle / "plugin/scripts/planted.py").write_text("print('planted')\n")
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("not listed in SHA256SUMS", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_package_with_a_symlink_is_refused(self):
        (self.bundle / "plugin/scripts/link.py").symlink_to("/etc/hosts")
        write_sums(self.bundle)  # even when the link is listed
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("link", done.stdout)
        self.assertEqual(self.files(), before)

    def test_finder_ds_store_does_not_block_the_install(self):
        (self.bundle / ".DS_Store").write_bytes(b"finder")

        self.assert_installed(self.install("y\n"))

    def test_plugin_folder_that_is_not_ours_stops_and_changes_nothing(self):
        foreign = self.plugin_dest()
        foreign.mkdir(parents=True)
        (foreign / "plugin.json").write_text('{"id": "someone-else"}\n')
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("is not ours", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_already_installed_is_seen_from_the_agent_or_the_rc_block(self):
        self.assert_installed(self.install("y\n"))
        shutil.rmtree(self.plugin_dest())
        (self.state() / "install.json").unlink()
        before = self.files()

        done = self.install("q\n")

        self.assertEqual(done.returncode, 0)
        self.assertIn("already installed", done.stdout)
        self.assertEqual(self.files(), before)

    def test_interrupt_during_install_undoes_everything(self):
        self.make_bundle(arm64=fake_proxy("arm64", slow=4))
        before = self.files()
        script = self.bundle / INSTALLER_NAME
        run = subprocess.Popen(["/bin/bash", str(script)], stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                               env=self.env, cwd=str(self.root), start_new_session=True)
        run.stdin.write("y\n")
        run.stdin.close()
        deadline = time.monotonic() + 30
        while not self.plist().exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(self.plist().exists(), "setup never started the agent")

        os.killpg(run.pid, signal.SIGINT)  # what Ctrl-C in the Terminal window sends
        out, _ = run.communicate(timeout=120)

        self.assertNotEqual(run.returncode, 0, out)
        self.assertIn("interrupted", out)
        self.assert_clean(before)

    def test_running_bob_is_quit_before_any_change(self):
        (self.root / "bob-running").touch()

        done = self.install("y\n")

        self.assert_installed(done)
        self.assertEqual((self.root / "osascript.log").read_text(), 'quit app "IBM Bob"\n')
        self.assertIn("IBM Bob.app/Contents/MacOS/IBM Bob",
                      (self.root / "pgrep.log").read_text())

    def test_bob_that_will_not_quit_stops_before_any_change(self):
        (self.root / "bob-running").touch()
        (self.root / "bob-stuck").touch()
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("IBM Bob is still running", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_arm64_mac_gets_the_arm64_binary(self):
        self.env["CG_TEST_ARCH"] = "arm64"

        self.assert_installed(self.install("y\n"))

        self.assertEqual((self.state() / "bin/context-guru-proxy").read_bytes(),
                         (self.bundle / "bin/darwin-arm64/context-guru-proxy").read_bytes())

    def test_intel_mac_gets_the_amd64_binary(self):
        self.env["CG_TEST_ARCH"] = "x86_64"

        self.assert_installed(self.install("y\n"))

        self.assertEqual((self.state() / "bin/context-guru-proxy").read_bytes(),
                         (self.bundle / "bin/darwin-amd64/context-guru-proxy").read_bytes())

    def test_unknown_processor_stops_and_changes_nothing(self):
        self.env["CG_TEST_ARCH"] = "ppc"
        before = self.files()

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("not supported", done.stdout)
        self.assertEqual(self.files(), before)

    def test_missing_vsix_installs_with_a_note(self):
        self.make_bundle(vsix=False)

        done = self.install("y\n")

        self.assert_installed(done)
        self.assertIn("– status bar: not in this package", done.stdout)

    def test_log_holds_steps_but_no_prompt_text_or_file_contents(self):
        self.install(f"{CANARY}\n")  # a canary answer at the consent question means No
        self.assert_installed(self.install("y\n"))
        self.uninstall("y\n")

        log = self.log_text()

        self.assertIn("install: consent=no", log)
        self.assertIn("install: consent=yes", log)
        self.assertIn("install: setup result=installed", log)
        self.assertIn("uninstall: remove result=removed", log)
        self.assertNotIn(CANARY, log)
        self.assertNotIn("[y/N]", log)
        self.assertNotIn("consent_question", log)
        self.assertNotIn("dashboard history?", log)
        self.assertNotIn("Send IBM Bob's model traffic", log)


class UninstallTest(Release):
    def test_consent_no_changes_nothing(self):
        self.assert_installed(self.install("y\n"))
        before = self.files()

        done = self.uninstall("n\n")

        self.assertEqual(done.returncode, 0)
        self.assertIn("Nothing changed.", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertTrue(healthz(self.port()))

    def test_dry_run_is_shown_before_the_question(self):
        self.assert_installed(self.install("y\n"))

        done = self.uninstall("n\n")

        self.assertIn("result=planned", done.stdout)
        self.assertLess(done.stdout.index("result=planned"), done.stdout.index("[y/N]"))

    def test_quits_bob_first(self):
        self.assert_installed(self.install("y\n"))
        (self.root / "bob-running").touch()

        done = self.uninstall("y\n")

        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertEqual((self.root / "osascript.log").read_text(), 'quit app "IBM Bob"\n')

    def test_bob_that_will_not_quit_stops_before_any_change(self):
        self.assert_installed(self.install("y\n"))
        (self.root / "bob-running").touch()
        (self.root / "bob-stuck").touch()
        before = self.files()

        done = self.uninstall("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("IBM Bob is still running", done.stdout)
        self.assertEqual(self.files(), before)

    def test_keeps_the_dashboard_history_by_default(self):
        self.assert_installed(self.install("y\n"))
        history = self.plant_history()

        done = self.uninstall("y\nn\n")

        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertIn("dashboard history? [y/N]", done.stdout)
        self.assertEqual(history.read_bytes(), b"history")
        self.assertEqual(sorted(p.name for p in self.state().iterdir()), [history.name])
        self.assertFalse(self.plugin_dest().exists())

    def test_deletes_the_history_only_on_a_second_yes(self):
        before = self.files()
        self.assert_installed(self.install("y\n"))
        self.plant_history()

        done = self.uninstall("y\ny\n")

        self.assertEqual(done.returncode, 0, done.stdout)
        self.assert_clean(before)

    def test_yes_flag_never_deletes_the_history(self):
        self.assert_installed(self.install("y\n"))
        history = self.plant_history()

        done = self.run_script(UNINSTALLER_NAME, "", "--yes")

        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertEqual(history.read_bytes(), b"history")

    def test_reinstall_keeps_the_history(self):
        self.assert_installed(self.install("y\n"))
        history = self.plant_history()

        self.assert_installed(self.install("r\ny\n"))

        self.assertEqual(history.read_bytes(), b"history")

    def test_a_failed_reinstall_says_the_old_install_is_gone(self):
        # The old install is removed before the new one starts. A rollback then undoes only the new
        # one, so "as they were before" would be false.
        self.assert_installed(self.install("y\n"))
        self.make_bundle(arm64=fake_proxy("arm64", dies=True))

        done = self.install("r\ny\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertNotIn("as they were before", done.stdout)
        self.assertIn("The old install was removed", done.stdout)

    def test_failed_install_keeps_a_state_folder_that_was_there_before(self):
        self.state().mkdir(parents=True)
        old = self.state() / "dashboard-8891.db"
        old.write_bytes(b"old history")
        self.make_bundle(arm64=fake_proxy("arm64", dies=True))

        done = self.install("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertEqual(old.read_bytes(), b"old history")

    def test_partly_removed_keeps_the_record_and_says_run_again(self):
        self.assert_installed(self.install("y\n"))
        # A hand edit inside our block: uninstall must leave it alone, so the undo is partial.
        self.rc.write_bytes(self.rc.read_bytes().replace(
            b"# <<< context-guru (bob) <<<", b"echo edited\n# <<< context-guru (bob) <<<"))

        done = self.uninstall("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("run this file again", done.stdout)
        self.assertTrue((self.state() / "install.json").is_file())
        self.assertTrue((self.state() / "context-guru-reset").is_file())
        self.assertTrue(self.plugin_dest().is_dir())

    def test_no_tool_left_changes_nothing_and_fails(self):
        self.assert_installed(self.install("y\n"))
        shutil.rmtree(self.plugin_dest())
        (self.state() / "context-guru-reset").unlink()
        alone = self.root / "alone"
        alone.mkdir()
        shutil.copy2(self.bundle / UNINSTALLER_NAME, alone / UNINSTALLER_NAME)
        before = self.files()

        done = self.uninstall("y\n", folder=alone)

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("run this file again", done.stdout)
        self.assertEqual(self.files(), before)
        self.assertTrue(healthz(self.port()))

    def test_tampered_package_plugin_is_not_used(self):
        self.assert_installed(self.install("y\n"))
        shutil.rmtree(self.plugin_dest())
        (self.state() / "context-guru-reset").unlink()
        marker = self.root / "tampered-ran"
        with open(self.bundle / "plugin/scripts/bob_plugin.py", "a") as handle:
            handle.write(f"\nopen({str(marker)!r}, 'w').close()\n")

        done = self.uninstall("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertFalse(marker.exists(), "the uninstaller ran a plugin that fails SHA256SUMS")

    def test_plugin_folder_that_is_not_ours_stops_and_changes_nothing(self):
        self.assert_installed(self.install("y\n"))
        (self.plugin_dest() / "plugin.json").write_text('{"id": "someone-else"}\n')
        before = self.files()

        done = self.uninstall("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("is not ours", done.stdout)
        self.assertEqual(self.files(), before)

    def test_a_foreign_listener_in_our_port_range_is_not_a_failure(self):
        before = self.files()
        self.assert_installed(self.install("y\n"))
        ours = self.port()
        foreign = socket.socket()
        self.addCleanup(foreign.close)
        for port in range(8990, 8890, -1):
            if port == ours:
                continue
            try:
                foreign.bind(("127.0.0.1", port))
                break
            except OSError:
                pass
        foreign.listen()

        done = self.uninstall("y\n")

        self.assertEqual(done.returncode, 0, done.stdout)
        self.assertNotIn("✗", done.stdout)
        self.assert_clean(before)

    def test_log_file_is_private(self):
        self.install("n\n")

        self.assertEqual((self.home / LOG_REL).stat().st_mode & 0o777, 0o600)

    def test_works_when_the_installed_plugin_folder_is_gone(self):
        before = self.files()
        self.assert_installed(self.install("y\n"))
        shutil.rmtree(self.plugin_dest())

        done = self.uninstall("y\n")

        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertNotIn("✗", done.stdout)
        self.assert_clean(before)

    def test_works_with_only_the_reset_script_left(self):
        before = self.files()
        self.assert_installed(self.install("y\n"))
        shutil.rmtree(self.plugin_dest())
        # The uninstaller alone, with no plugin next to it: only the state copy of reset.sh is left.
        alone = self.root / "alone"
        alone.mkdir()
        shutil.copy2(self.bundle / UNINSTALLER_NAME, alone / UNINSTALLER_NAME)

        done = self.uninstall("y\n", folder=alone)

        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("rc_block=removed", done.stdout)
        self.assertNotIn("✗", done.stdout)
        self.assert_clean(before)


class QuietFiles(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


class BootstrapTest(Release):
    """install-bob.sh, piped into bash the way `curl -fsSL …/install-bob.sh | bash` runs it."""

    def setUp(self):
        super().setUp()
        self.release = self.root / "release"
        self.release.mkdir()
        # The real packager builds the zip from the test bundle, as the release workflow does.
        zipped = subprocess.run(
            ["/bin/sh", str(PACKAGER), "v9.9.9-test", str(self.release),
             str(self.bundle / "bin/darwin-arm64/context-guru-proxy"),
             str(self.bundle / "bin/darwin-amd64/context-guru-proxy"), str(self.bundle / "plugin")],
            capture_output=True, text=True, timeout=60)
        self.assertEqual(zipped.returncode, 0, zipped.stderr)
        self.zip = self.release / "context-guru-bob-v9.9.9-test.zip"
        self.write_release_sums()
        handler = functools.partial(QuietFiles, directory=str(self.release))
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_address[1]}"
        self.tty = self.root / "tty"

    def write_release_sums(self, digest=None):
        digest = digest or hashlib.sha256(self.zip.read_bytes()).hexdigest()
        (self.release / "context-guru-bob-SHA256SUMS").write_text(f"{digest}  {self.zip.name}\n")

    def pipe(self, answers, *args):
        """bash reads the script on stdin, and the user's answers come from the terminal."""
        self.tty.write_text(answers)
        env = dict(self.env, CG_BOB_RELEASE_URL=self.url, CG_BOB_TTY=str(self.tty))
        argv = ["/bin/bash", "-s", *args] if args else ["/bin/bash"]
        return subprocess.run(argv, input=BOOTSTRAP.read_text(), text=True,
                              capture_output=True, env=env, cwd=str(self.root), timeout=180)

    def test_piped_uninstall_restores_every_byte(self):
        before = self.files()
        self.assert_installed(self.pipe("y\n"))

        done = self.pipe("y\ny\n", "uninstall")

        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assert_clean(before)

    def test_an_unknown_command_runs_nothing(self):
        before = self.files()

        done = self.pipe("y\n", "remove-everything")

        self.assertEqual(done.returncode, 2)
        self.assertIn("usage:", done.stderr)
        self.assertEqual(self.files(), before)

    def test_piped_install_downloads_checks_and_installs(self):
        done = self.pipe("y\n")

        self.assert_installed(done)
        self.assertIn("✓ download matches SHA-256", done.stdout)
        self.assertTrue((self.plugin_dest() / "scripts/bob_plugin.py").is_file())
        self.assertTrue(healthz(self.port()))

    def test_piped_install_reads_the_answer_from_the_terminal_not_the_pipe(self):
        # The answer file says No. The script on stdin must not be read as an answer.
        done = self.pipe("n\n")

        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Nothing changed.", done.stdout)
        self.assertFalse(self.plugin_dest().exists())

    def test_a_zip_that_does_not_match_its_sum_runs_nothing(self):
        before = self.files()
        self.write_release_sums("0" * 64)

        done = self.pipe("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("does not match", done.stdout + done.stderr)
        self.assertEqual(self.files(), before)

    def test_a_missing_sums_file_runs_nothing(self):
        before = self.files()
        (self.release / "context-guru-bob-SHA256SUMS").unlink()

        done = self.pipe("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertEqual(self.files(), before)

    def test_the_download_folder_is_removed_after_a_no(self):
        done = self.pipe("n\n")

        # TMPDIR is self.root, so the folder would be self.root/cg-bob-install.*.
        self.assertEqual(list(self.root.glob("cg-bob-install.*")), [])
        self.assertNotIn("unbound variable", done.stderr)

    def test_the_download_folder_is_removed_after_a_full_install(self):
        done = self.pipe("y\n")

        self.assert_installed(done)
        self.assertEqual(list(self.root.glob("cg-bob-install.*")), [])
        self.assertNotIn("unbound variable", done.stderr)

    def test_the_installer_exit_code_comes_through(self):
        # A failed install (here: Bob is not installed) must still fail the one-command run.
        shutil.rmtree(self.bob_app)

        done = self.pipe("y\n")

        self.assertNotEqual(done.returncode, 0)
        self.assertEqual(list(self.root.glob("cg-bob-install.*")), [])

    def test_a_truncated_script_runs_nothing(self):
        # A download cut short must not run half the script: everything sits in one function
        # that is called on the last line.
        before = self.files()
        script = BOOTSTRAP.read_text()
        self.tty.write_text("y\n")
        env = dict(self.env, CG_BOB_RELEASE_URL=self.url, CG_BOB_TTY=str(self.tty))
        done = subprocess.run(["/bin/bash"], input=script[: len(script) // 2], text=True,
                              capture_output=True, env=env, cwd=str(self.root), timeout=60)

        self.assertEqual(self.files(), before)
        self.assertNotIn("Downloading", done.stdout)


class SpacedHomeTest(Release):
    HOME_NAME = "my home"

    def test_install_and_uninstall_with_a_space_in_home(self):
        before = self.files()

        self.assert_installed(self.install("y\n"))
        done = self.uninstall("y\n")

        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assert_clean(before)


@unittest.skipUnless(shutil.which("zip") and shutil.which("shasum"), "package.sh needs zip, shasum")
class PackageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.plugin = self.root / "plugin-src"
        shutil.copytree(PLUGIN_SRC, self.plugin, ignore=shutil.ignore_patterns("*.vsix"))
        (self.plugin / "scripts/__pycache__").mkdir(exist_ok=True)
        (self.plugin / "scripts/__pycache__/x.pyc").write_bytes(b"cache")
        (self.plugin / "statusbar/context-guru-bob.vsix").write_bytes(b"PK vsix")
        self.arm64 = self.root / "arm64-proxy"
        self.amd64 = self.root / "amd64-proxy"
        self.arm64.write_bytes(b"arm64 binary")
        self.amd64.write_bytes(b"amd64 binary")
        self.out = self.root / "dist"

    def tearDown(self):
        self.temp.cleanup()

    def package(self, plugin=None):
        return subprocess.run(["/bin/sh", str(PACKAGER), "v9.9.9-test", str(self.out),
                               str(self.arm64), str(self.amd64), str(plugin or self.plugin)],
                              capture_output=True, text=True, timeout=60)

    def test_zip_has_the_listed_members_and_correct_sums(self):
        done = self.package()

        self.assertEqual(done.returncode, 0, done.stderr)
        archive = self.out / "context-guru-bob-v9.9.9-test.zip"
        self.assertEqual(done.stdout.strip(), str(archive))
        top = "context-guru-bob-v9.9.9-test/"
        with zipfile.ZipFile(archive) as bundle:
            names = {info.filename: info for info in bundle.infolist() if not info.is_dir()}
            files = {name.removeprefix(top): bundle.read(name) for name in names}
        for member in (INSTALLER_NAME, UNINSTALLER_NAME, "README.md", "SHA256SUMS",
                       "bin/darwin-arm64/context-guru-proxy", "bin/darwin-amd64/context-guru-proxy",
                       "plugin/plugin.json", "plugin/scripts/bob_plugin.py",
                       "plugin/scripts/reset.sh", "plugin/statusbar/context-guru-bob.vsix"):
            self.assertIn(member, files)
        self.assertEqual(files["bin/darwin-arm64/context-guru-proxy"], b"arm64 binary")
        self.assertEqual(files["bin/darwin-amd64/context-guru-proxy"], b"amd64 binary")
        self.assertFalse([n for n in files if "__pycache__" in n or "/test_" in n
                          or n.endswith(".gitignore")], "tests and caches must not ship")
        listed = {}
        for line in files["SHA256SUMS"].decode().splitlines():
            digest, name = line.split("  ", 1)
            listed[name] = digest
        self.assertEqual(set(listed), set(files) - {"SHA256SUMS"})
        for name, digest in listed.items():
            self.assertEqual(hashlib.sha256(files[name]).hexdigest(), digest, name)
        for name in (INSTALLER_NAME, UNINSTALLER_NAME, "bin/darwin-arm64/context-guru-proxy",
                     "bin/darwin-amd64/context-guru-proxy"):
            mode = names[top + name].external_attr >> 16
            self.assertEqual(mode & 0o755, 0o755, f"{name} is not executable in the zip")

    def test_refuses_a_plugin_without_the_vsix(self):
        (self.plugin / "statusbar/context-guru-bob.vsix").unlink()

        done = self.package()

        self.assertNotEqual(done.returncode, 0)
        self.assertIn("context-guru-bob.vsix", done.stderr)
        self.assertFalse((self.out / "context-guru-bob-v9.9.9-test.zip").exists())


if __name__ == "__main__":
    unittest.main()
