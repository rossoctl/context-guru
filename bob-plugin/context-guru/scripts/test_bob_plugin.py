"""Tests for the context-guru Bob plugin. Every test uses a temp HOME and fakes.

Run: python3 -m unittest discover -s bob-plugin/context-guru/scripts -p 'test_*.py'

Nothing here touches the real HOME, the real launchd or a real Bob. A fake `launchctl` on PATH
records its arguments. A fake proxy (a local HTTP server) answers /healthz, /api/requests and
/signal.
"""

import contextlib
import hashlib
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
PLUGIN_ROOT = HERE.parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


sys.path.insert(0, str(HERE))
ROUTE = load("bob_route")
HOOKS = load("bob_hooks")
PLUGIN = load("bob_plugin")

CANARY = "CANARY-prompt-7f3a9c-do-not-store"
# `print` says "not loaded" (113), as launchctl does after a bootout. Knobs, each a file holding N:
# CG_TEST_FAIL_BOOTSTRAP fails the next N bootstraps; CG_TEST_PRINT_LOADED makes the next N prints
# say "loaded". CG_TEST_SLOW_KICKSTART makes kickstart sleep that many seconds.
FAKE_LAUNCHCTL = """#!/bin/sh
printf '%s\\n' "$*" >> "$CG_TEST_LAUNCHCTL_LOG"
case "$1" in
  print)
    if [ -n "${CG_TEST_PRINT_LOADED:-}" ] && [ -f "$CG_TEST_PRINT_LOADED" ]; then
      n=$(cat "$CG_TEST_PRINT_LOADED")
      if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$CG_TEST_PRINT_LOADED"; exit 0; fi
    fi
    exit 113 ;;
  kickstart)
    if [ -n "${CG_TEST_SLOW_KICKSTART:-}" ]; then sleep "$CG_TEST_SLOW_KICKSTART"; fi ;;
  bootstrap)
    if [ -n "${CG_TEST_FAIL_BOOTSTRAP:-}" ] && [ -f "$CG_TEST_FAIL_BOOTSTRAP" ]; then
      n=$(cat "$CG_TEST_FAIL_BOOTSTRAP")
      if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$CG_TEST_FAIL_BOOTSTRAP"; exit 5; fi
    fi ;;
esac
exit 0
"""


class FakeProxy:
    """A local HTTP server in a thread: /healthz, /api/requests, /signal."""

    def __init__(self, last_bob_ts_ms=None):
        self.signals = []
        self.paths = []
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                owner.paths.append(self.path)
                if self.path == "/healthz":
                    body = b"ok"
                elif self.path.startswith("/api/requests"):
                    rows = [] if last_bob_ts_ms is None else [{"id": 1, "ts": last_bob_ts_ms,
                                                              "agent": "bob"}]
                    body = json.dumps({"requests": rows, "next_cursor": 0,
                                       "total": len(rows)}).encode()
                else:
                    self.send_response(404)
                    self.end_headers()
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                owner.signals.append((self.path, self.headers.get("Content-Type"),
                                      self.rfile.read(length)))
                self.send_response(204)
                self.end_headers()

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.server.shutdown()
        self.server.server_close()


def dead_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class TempHome(unittest.TestCase):
    """A temp root with home/ and state/, a fake launchctl first on PATH, SHELL=/bin/bash.

    bash, not zsh, so the suite runs on the Linux CI runner too. self.rc is the file bash reads.
    """

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.home = self.root / "home"
        self.home.mkdir()
        fakebin = self.root / "fakebin"
        fakebin.mkdir()
        launchctl = fakebin / "launchctl"
        launchctl.write_text(FAKE_LAUNCHCTL)
        launchctl.chmod(0o755)
        self.launchctl_log = self.root / "launchctl.log"
        self.env = mock.patch.dict(os.environ, {
            "HOME": str(self.home),
            "XDG_STATE_HOME": str(self.root / "state"),
            "SHELL": "/bin/bash",
            "PATH": f"{fakebin}:{os.environ.get('PATH', '/usr/bin:/bin')}",
            "CG_TEST_LAUNCHCTL_LOG": str(self.launchctl_log),
            "CONTEXT_GURU_BIN": "/nonexistent/context-guru-proxy",
            # Never the real bobide: a test that needs one points this at a fake.
            "CONTEXT_GURU_BOBIDE": "/nonexistent/bobide",
            # Never the real Bob app: a doctor test that needs one sets its own fake.
            "CONTEXT_GURU_BOB_APP": str(self.root / "no-bob/IBM Bob.app"),
        }, clear=False)
        self.env.start()
        self.rc = self.home / ".bash_profile"

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def launchctl_calls(self):
        if not self.launchctl_log.exists():
            return []
        return self.launchctl_log.read_text().splitlines()

    def tree(self):
        """Every path under the temp root except the fake launchctl and its log, with bytes."""
        result = {}
        for path in sorted(self.root.rglob("*")):
            if path.is_relative_to(self.root / "fakebin") or path == self.launchctl_log:
                continue
            rel = str(path.relative_to(self.root))
            result[rel] = path.read_bytes() if path.is_file() and not path.is_symlink() else None
        return result

    def run_main(self, *argv, stdin=""):
        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["bob_plugin.py", *argv]), \
             mock.patch.object(sys, "stdin", io.StringIO(stdin)), \
             contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = PLUGIN.main()
        # doctor prints the Bob app it read. It must be a fake under the temp root, never the
        # real ~/Applications/IBM Bob.app.
        for line in out.getvalue().splitlines():
            if line.startswith("bob_app="):
                self.assertTrue(Path(line.removeprefix("bob_app=")).is_relative_to(self.root),
                                f"doctor read a Bob app outside the test: {line}")
        return code, out.getvalue(), err.getvalue()

    def write_record(self, port, **extra):
        PLUGIN.state_dir().mkdir(parents=True, exist_ok=True)
        record = {"port": port, "binary": "/opt/cg/context-guru-proxy",
                  "upstream": PLUGIN.DEFAULT_UPSTREAM, **extra}
        PLUGIN.record_path().write_text(json.dumps(record))


# ---------------------------------------------------------------------------- rc block (bob_route)

class RouteTest(TempHome):
    def test_block_is_the_fixed_text_with_only_the_port_filled_in(self):
        self.assertEqual(ROUTE.block(8891), (
            b"\n# >>> context-guru (bob) >>>\n"
            b"# IBM Bob reads this when it starts. Remove it with the context-guru-uninstall skill.\n"
            b'if [ "${VSCODE_RESOLVING_ENVIRONMENT:-}" = 1 ] && '
            b'[ "${__CFBundleIdentifier:-}" = com.ibm.software.bob ]; then\n'
            b"  export VITE_GATEWAY_BASE_URL=http://127.0.0.1:8891\n"
            b"fi\n"
            b"export BOB_GATEWAY_URL=http://127.0.0.1:8891\n"
            b"# <<< context-guru (bob) <<<\n"))
        self.assertNotIn(b"CUSTOM_BASE_URL", ROUTE.block(8891))
        # The legacy form is still recognized, so an installed block can be removed.
        self.assertEqual(ROUTE.our_blocks(8891), (ROUTE.block(8891), self.LEGACY_BLOCK))

    def test_zsh_round_trip_is_byte_identical(self):
        rc = self.home / ".zshrc"
        for original in (b"", b"alias ll='ls -l'\n", b"export EDITOR=vim", b"\n\n# end\n\n",
                         "echo héllo\n".encode(), b"bad \xff byte\n"):
            with self.subTest(original=original):
                rc.write_bytes(original)
                self.assertEqual(ROUTE.install(rc, 8891), "installed")
                self.assertTrue(ROUTE.is_routed(rc, 8891))
                self.assertEqual(rc.read_bytes(), original + ROUTE.block(8891))
                self.assertEqual(ROUTE.remove(rc), "removed")
                self.assertEqual(rc.read_bytes(), original)
                self.assertFalse(ROUTE.is_routed(rc))

    # The block as setup wrote it before 2026-11-09, on Macs that have it installed now.
    LEGACY_BLOCK = (b'\n# >>> context-guru (bob) >>>\n'
                    b'# IBM Bob reads this when it starts. Remove it with the context-guru-uninstall skill.\n'
                    b'if [ "$VSCODE_RESOLVING_ENVIRONMENT" = 1 ] && [ "$__CFBundleIdentifier" = com.ibm.software.bob ]; then\n'
                    b'  export VITE_GATEWAY_BASE_URL=http://127.0.0.1:8891\n'
                    b'fi\n'
                    b'export BOB_GATEWAY_URL=http://127.0.0.1:8891\n'
                    b'# <<< context-guru (bob) <<<\n')

    @unittest.skipUnless(shutil.which("zsh"), "needs zsh")
    def test_the_block_survives_nounset_and_the_rest_of_zshrc_still_runs(self):
        # With `setopt nounset`, an unset variable stops zsh reading .zshrc at that line, so every
        # line of the user's after the block would be skipped too.
        rc = self.home / ".zshrc"
        rc.write_bytes(b"setopt nounset\n")
        ROUTE.install(rc, 8891)
        with rc.open("ab") as f:
            f.write(b"echo USER-LINE-RAN\n")
        done = subprocess.run(["zsh", "-i", "-c", 'echo "url=${BOB_GATEWAY_URL:-unset}"'],
                              env={"HOME": str(self.home), "ZDOTDIR": str(self.home),
                                   "PATH": "/usr/bin:/bin"},
                              capture_output=True, text=True, timeout=30)
        self.assertNotIn("parameter not set", done.stderr)
        self.assertIn("USER-LINE-RAN", done.stdout)
        self.assertIn("url=http://127.0.0.1:8891", done.stdout)

    def test_a_legacy_block_is_still_ours_and_remove_is_byte_identical(self):
        rc = self.home / ".zshrc"
        rc.write_bytes(b"alias ll='ls -l'\n" + self.LEGACY_BLOCK)
        self.assertEqual(ROUTE.ownership(rc.read_bytes()), ("ours", 8891))
        self.assertEqual(ROUTE.remove(rc), "removed")
        self.assertEqual(rc.read_bytes(), b"alias ll='ls -l'\n")

    def test_install_on_a_legacy_block_writes_the_new_block_once(self):
        rc = self.home / ".zshrc"
        rc.write_bytes(b"x=1\n" + self.LEGACY_BLOCK)
        ROUTE.install(rc, 8891)
        self.assertEqual(rc.read_bytes(), b"x=1\n" + ROUTE.block(8891))

    def test_install_twice_writes_one_block(self):
        rc = self.home / ".zshrc"
        rc.write_bytes(b"x=1\n")
        ROUTE.install(rc, 8891)
        self.assertEqual(ROUTE.install(rc, 8891), "already")
        self.assertEqual(rc.read_bytes(), b"x=1\n" + ROUTE.block(8891))

    def test_new_port_replaces_our_block_in_place(self):
        rc = self.home / ".zshrc"
        rc.write_bytes(b"a\n" + ROUTE.block(8891) + b"b\n")
        self.assertEqual(ROUTE.install(rc, 8892), "installed")
        self.assertEqual(rc.read_bytes(), b"a\n" + ROUTE.block(8892) + b"b\n")

    def test_zsh_target_is_zshrc(self):
        target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertEqual((target, advice), (self.home / ".zshrc", None))

    def test_bash_target_is_the_first_login_file(self):
        (self.home / ".bashrc").write_text("# not read by a login shell\n")
        (self.home / ".profile").write_text("# p\n")
        (self.home / ".bash_login").write_text("# l\n")
        self.assertEqual(ROUTE.resolve(self.home, "/bin/bash")[0], self.home / ".bash_login")
        (self.home / ".bash_profile").write_text("# bp\n")
        self.assertEqual(ROUTE.resolve(self.home, "/opt/homebrew/bin/bash")[0],
                         self.home / ".bash_profile")

    def test_bash_with_only_profile_uses_profile(self):
        (self.home / ".bashrc").write_text("# rc\n")
        (self.home / ".profile").write_text("# p\n")
        self.assertEqual(ROUTE.resolve(self.home, "/bin/bash"), (self.home / ".profile", None))

    def test_bash_with_no_login_file_creates_bash_profile_and_round_trip_removes_it(self):
        (self.home / ".bashrc").write_text("# rc\n")
        target, advice = ROUTE.resolve(self.home, "/bin/bash")
        self.assertEqual((target, advice), (self.home / ".bash_profile", None))
        ROUTE.install(target, 8891)
        self.assertEqual(target.read_bytes(), ROUTE.block(8891))
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o644)
        ROUTE.remove(target, created=True)
        self.assertFalse(target.exists())
        self.assertEqual((self.home / ".bashrc").read_text(), "# rc\n")

    def test_unknown_shell_is_advice_and_writes_nothing(self):
        for shell in ("/usr/local/bin/fish", ""):
            with self.subTest(shell=shell):
                target, advice = ROUTE.resolve(self.home, shell)
                self.assertIsNone(target)
                self.assertIn("zsh and bash", advice)
        self.assertEqual(list(self.home.iterdir()), [])

    def test_one_symlink_hop_is_followed_and_the_link_stays(self):
        dotfiles = self.root / "dotfiles"
        dotfiles.mkdir()
        (dotfiles / "zshrc").write_bytes(b"# tracked\n")
        (self.home / ".zshrc").symlink_to(dotfiles / "zshrc")
        target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertEqual((target, advice), (dotfiles / "zshrc", None))
        ROUTE.install(target, 8891)
        self.assertTrue((self.home / ".zshrc").is_symlink())
        self.assertEqual((dotfiles / "zshrc").read_bytes(), b"# tracked\n" + ROUTE.block(8891))
        ROUTE.remove(target)
        self.assertEqual((dotfiles / "zshrc").read_bytes(), b"# tracked\n")

    def test_relative_symlink_resolves_against_the_link_directory(self):
        (self.home / "dot").mkdir()
        (self.home / "dot" / "zshrc").write_bytes(b"")
        (self.home / ".zshrc").symlink_to("dot/zshrc")
        self.assertEqual(ROUTE.resolve(self.home, "/bin/zsh")[0], self.home / "dot" / "zshrc")

    def test_two_hop_chain_is_refused(self):
        (self.root / "real").write_bytes(b"")
        (self.root / "mid").symlink_to(self.root / "real")
        (self.home / ".zshrc").symlink_to(self.root / "mid")
        target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertIsNone(target)
        self.assertIn("deeper than 1", advice)

    def test_self_link_is_refused_without_hanging(self):
        (self.home / ".zshrc").symlink_to(self.home / ".zshrc")
        target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertIsNone(target)
        self.assertIn("deeper than 1", advice)

    def test_dangling_link_is_refused_and_not_repaired(self):
        (self.home / ".zshrc").symlink_to(self.root / "gone")
        target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertIsNone(target)
        self.assertIn("dangling", advice)
        self.assertFalse((self.root / "gone").exists())

    def test_an_edited_block_is_not_ours_and_is_left_alone(self):
        rc = self.home / ".zshrc"
        edited = ROUTE.block(8891).replace(b"fi\n", b"fi\nexport MINE=1\n")
        rc.write_bytes(b"a\n" + edited)
        self.assertEqual(ROUTE.ownership(rc.read_bytes()), ("cannot_tell", None))
        with self.assertRaisesRegex(ROUTE.RouteRefused, "edited"):
            ROUTE.install(rc, 8891)
        with self.assertRaisesRegex(ROUTE.RouteRefused, "edited"):
            ROUTE.remove(rc)
        self.assertEqual(rc.read_bytes(), b"a\n" + edited)
        self.assertFalse(ROUTE.is_routed(rc))

    def test_two_copies_are_left_alone(self):
        rc = self.home / ".zshrc"
        text = ROUTE.block(8891) + ROUTE.block(8891)
        rc.write_bytes(text)
        with self.assertRaisesRegex(ROUTE.RouteRefused, "2 copies"):
            ROUTE.remove(rc)
        self.assertEqual(rc.read_bytes(), text)

    def test_a_value_the_user_set_is_not_ours(self):
        rc = self.home / ".zshrc"
        for line in (b"export VITE_GATEWAY_BASE_URL=https://mine\n",
                     b"export BOB_GATEWAY_URL=http://mine\n"):
            with self.subTest(line=line):
                rc.write_bytes(line)
                self.assertEqual(ROUTE.ownership(line), ("not_ours", None))
                with self.assertRaisesRegex(ROUTE.RouteRefused, "already sets"):
                    ROUTE.install(rc, 8891)
                self.assertEqual(rc.read_bytes(), line)

    def test_install_keeps_the_file_mode(self):
        rc = self.home / ".zshrc"
        rc.write_bytes(b"x\n")
        rc.chmod(0o600)
        ROUTE.install(rc, 8891)
        self.assertEqual(stat.S_IMODE(rc.stat().st_mode), 0o600)
        self.assertEqual([p.name for p in self.home.iterdir()], [".zshrc"])  # no temp left

    def test_backup_is_written_once(self):
        rc = self.home / ".zshrc"
        backup = self.root / "state" / "backup"
        rc.write_bytes(b"first\n")
        ROUTE.backup_once(rc, backup)
        rc.write_bytes(b"second\n")
        ROUTE.backup_once(rc, backup)
        self.assertEqual((backup / ".zshrc.before-context-guru").read_bytes(), b"first\n")


@unittest.skipUnless(Path("/bin/bash").exists(), "needs /bin/bash")
class ProbeTest(TempHome):
    """The verify step runs the real shells the way Bob does, against the temp HOME."""

    def test_zsh_probe_sees_the_ide_value_only_in_the_bob_resolve(self):
        if not Path("/bin/zsh").exists():
            self.skipTest("needs /bin/zsh (macOS has it; D14 is macOS only)")
        ROUTE.install(self.home / ".zshrc", 8891)
        result = ROUTE.probe(self.home, "/bin/zsh")
        self.assertEqual(result, {"ide": "http://127.0.0.1:8891", "terminal_ide": "",
                                  "terminal_shell": "http://127.0.0.1:8891"})

    def test_bash_probe_reads_bash_profile(self):
        (self.home / ".bash_profile").write_text("PS1='$ '\n")
        ROUTE.install(ROUTE.resolve(self.home, "/bin/bash")[0], 8891)
        result = ROUTE.probe(self.home, "/bin/bash")
        self.assertEqual(result["ide"], "http://127.0.0.1:8891")
        self.assertEqual(result["terminal_ide"], "")

    def test_bash_probe_reports_a_miss_for_a_block_in_bashrc(self):
        (self.home / ".bash_profile").write_text("PS1='$ '\n")
        (self.home / ".bashrc").write_bytes(ROUTE.block(8891))
        self.assertEqual(ROUTE.probe(self.home, "/bin/bash")["ide"], "")

    def test_probe_ignores_what_the_rc_file_prints(self):
        if not Path("/bin/zsh").exists():
            self.skipTest("needs /bin/zsh")
        (self.home / ".zshrc").write_text("echo welcome to my shell\n")
        ROUTE.install(self.home / ".zshrc", 8891)
        self.assertEqual(ROUTE.probe(self.home, "/bin/zsh")["ide"], "http://127.0.0.1:8891")


# ---------------------------------------------------------------------------- hooks (bob_hooks)

class HooksTest(TempHome):
    def settings(self):
        return self.home / ".bob/settings/settings.json"

    def state(self):
        return self.root / "state/context-guru-bob/hooks"

    def commands(self):
        return HOOKS.our_groups("/usr/bin/python3", "/p/scripts/bob_plugin.py")

    def test_absent_settings_file_is_created_and_round_trip_removes_it(self):
        HOOKS.install(self.settings(), self.state(), self.commands())
        data = json.loads(self.settings().read_text())
        self.assertEqual(sorted(data["hooks"]), ["SessionStart", "UserPromptSubmit"])
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "restored")
        self.assertFalse(self.settings().exists())
        self.assertFalse(self.state().exists())

    def test_merge_keeps_user_keys_order_and_hooks_and_remove_is_byte_identical(self):
        original = (b'{\n    "zeta": 1,\n    "hooks": {\n        "Stop": [\n            {\n'
                    b'                "hooks": [{"type": "command", "command": "say done"}]\n'
                    b'            }\n        ],\n        "SessionStart": [\n            {\n'
                    b'                "matcher": "startup",\n                "hooks": '
                    b'[{"type": "command", "command": "mine.sh"}]\n            }\n        ]\n'
                    b'    },\n    "alpha": true\n}\n')
        self.settings().parent.mkdir(parents=True)
        self.settings().write_bytes(original)
        HOOKS.install(self.settings(), self.state(), self.commands())
        data = json.loads(self.settings().read_text())
        self.assertEqual(list(data), ["zeta", "hooks", "alpha"])
        self.assertEqual(list(data["hooks"]), ["Stop", "SessionStart", "UserPromptSubmit"])
        self.assertEqual(data["hooks"]["Stop"],
                         [{"hooks": [{"type": "command", "command": "say done"}]}])
        self.assertEqual(data["hooks"]["SessionStart"][0]["hooks"][0]["command"], "mine.sh")
        self.assertEqual(len(data["hooks"]["SessionStart"]), 2)
        self.assertTrue(self.settings().read_text().startswith('{\n    "zeta"'))  # indent kept
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "restored")
        self.assertEqual(self.settings().read_bytes(), original)

    def test_remove_after_bob_rewrote_the_file_takes_out_only_ours(self):
        self.settings().parent.mkdir(parents=True)
        self.settings().write_text(json.dumps({"hooks": {"Stop": [
            {"hooks": [{"type": "command", "command": "say done"}]}]}}, indent=2) + "\n")
        HOOKS.install(self.settings(), self.state(), self.commands())
        data = json.loads(self.settings().read_text())
        data["bob.theme"] = "dark"  # Bob changed a setting since
        self.settings().write_text(json.dumps(data, indent=2) + "\n")
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "removed")
        self.assertEqual(json.loads(self.settings().read_text()), {
            "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]},
            "bob.theme": "dark"})

    def test_jsonc_is_refused_and_the_file_is_unchanged(self):
        self.settings().parent.mkdir(parents=True)
        for text in ('{\n  // my comment\n  "a": 1\n}\n', '{"a": 1,}\n', '[1]\n',
                     '{"hooks": []}\n'):
            with self.subTest(text=text):
                self.settings().write_text(text)
                with self.assertRaises(HOOKS.HooksRefused):
                    HOOKS.install(self.settings(), self.state(), self.commands())
                self.assertEqual(self.settings().read_text(), text)
                self.assertFalse(self.state().exists())

    def test_install_twice_keeps_one_copy(self):
        HOOKS.install(self.settings(), self.state(), self.commands())
        first = self.settings().read_bytes()
        HOOKS.install(self.settings(), self.state(), self.commands())
        self.assertEqual(self.settings().read_bytes(), first)
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "restored")
        self.assertFalse(self.settings().exists())

    def test_our_hook_commands_are_absolute_short_and_cannot_exit_nonzero(self):
        groups = HOOKS.our_groups("/usr/bin/python3", "/Users/u/.bob/plugins/context-guru/"
                                  "scripts/bob_plugin.py")
        self.assertEqual(sorted(groups), ["SessionStart", "UserPromptSubmit"])
        self.assertNotIn("matcher", groups["SessionStart"])
        subcommands = []
        for group in groups.values():
            for handler in group["hooks"]:
                self.assertEqual(handler["type"], "command")
                self.assertLessEqual(handler["timeout"], 5)
                command = handler["command"]
                self.assertTrue(command.startswith("/usr/bin/python3 /Users/u/.bob/"), command)
                self.assertIn(" </dev/null" if " signal " not in command else " signal ", command)
                self.assertTrue(command.endswith("|| true " + HOOKS.TAG), command)
                subcommands.append(command.split("bob_plugin.py ")[1].split()[0])
        self.assertEqual(subcommands, ["ensure", "signal", "check"])

    def test_is_installed_reports_our_groups(self):
        self.assertFalse(HOOKS.is_installed(self.settings()))
        HOOKS.install(self.settings(), self.state(), self.commands())
        self.assertTrue(HOOKS.is_installed(self.settings()))


class HookCommandShellTest(TempHome):
    """Bob runs a command hook with Node's child_process.exec, which is `/bin/sh -c`."""

    def test_a_missing_interpreter_or_script_still_exits_zero(self):
        for python, script in (("/nonexistent/python3", str(HERE / "bob_plugin.py")),
                               (sys.executable, "/nonexistent/bob_plugin.py")):
            for group in HOOKS.our_groups(python, script).values():
                for handler in group["hooks"]:
                    with self.subTest(command=handler["command"]):
                        done = subprocess.run(["/bin/sh", "-c", handler["command"]],
                                              input='{"hook_event_name":"UserPromptSubmit"}',
                                              text=True, capture_output=True, timeout=10)
                        self.assertEqual(done.returncode, 0)
                        self.assertEqual(done.stdout, "")


# ---------------------------------------------------------------------------- controller

class SetupTest(TempHome):
    def test_plan_writes_nothing(self):
        self.rc.write_bytes(b"x\n")
        before = self.tree()
        code, out, _ = self.run_main("setup", "--plan")
        self.assertEqual(code, 0)
        self.assertIn("result=planned", out)
        self.assertIn(f"rc_file={self.rc}", out)
        self.assertIn(f"settings={self.home / '.bob/settings/settings.json'}", out)
        self.assertIn("consent_question=", out)
        self.assertIn("log in to Bob once", out)
        self.assertEqual(self.tree(), before)
        self.assertEqual(self.launchctl_calls(), [])

    def test_without_consent_setup_refuses_and_writes_nothing(self):
        before = self.tree()
        code, out, _ = self.run_main("setup")
        self.assertEqual(code, 2)
        self.assertIn("result=refused\nreason=consent_required", out)
        self.assertEqual(self.tree(), before)
        self.assertEqual(self.launchctl_calls(), [])

    @staticmethod
    def good_probe(port):
        # The zsh rc probe needs a macOS home. These tests are about the old rc block, not the probe.
        return {"ide": f"http://127.0.0.1:{port}", "terminal_ide": "", "terminal_shell": ""}

    def test_a_shell_change_between_setups_leaves_no_block_and_keeps_the_users_file(self):
        # Setup under bash created ~/.bash_profile. The user then switched to zsh (macOS asks for
        # it) and ran setup again; their own ~/.zshrc was already there. Uninstall must remove the
        # block from both files, and must not delete the user's .zshrc.
        zshrc = self.home / ".zshrc"
        self.assertFalse(self.rc.exists())
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
            self.assertTrue(ROUTE.is_routed(self.rc))
            zshrc.write_bytes(b"alias ll='ls -l'\n")
            with mock.patch.dict(os.environ, {"SHELL": "/bin/zsh"}), \
                 mock.patch.object(PLUGIN.bob_route, "probe", return_value=self.good_probe(proxy.port)):
                self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
                code, out, _ = self.run_main("uninstall")
        self.assertEqual(code, 0, out)
        self.assertIn("result=removed", out)
        self.assertFalse(self.rc.exists() and ROUTE.is_routed(self.rc),
                         "the old .bash_profile still routes Bob after uninstall")
        self.assertEqual(zshrc.read_bytes(), b"alias ll='ls -l'\n",
                         "the user's own .zshrc was deleted or changed")

    def test_a_failed_shell_change_keeps_the_old_rc_block(self):
        # Bash setup, then a zsh re-run that fails after the new block is in. The old block must
        # stay: after the rollback it is the only route that still sends Bob to the proxy.
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
            with mock.patch.dict(os.environ, {"SHELL": "/bin/zsh"}), \
                 mock.patch.object(PLUGIN.bob_route, "probe", return_value=self.good_probe(proxy.port)), \
                 mock.patch.object(PLUGIN.bob_hooks, "install", side_effect=OSError("disk full")):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1, out)
        self.assertIn("result=setup_failed", out)
        self.assertTrue(ROUTE.is_routed(self.rc), "the old .bash_profile block was removed")

    def test_a_shell_change_removes_the_bash_profile_setup_had_created(self):
        # Setup created ~/.bash_profile with only our block in it. After a move to zsh, that file
        # is ours and empty again, so it goes, as uninstall would remove it.
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.run_main("setup", "--i-consent-to-traffic-interception")
            self.assertTrue(self.rc.exists())
            with mock.patch.dict(os.environ, {"SHELL": "/bin/zsh"}), \
                 mock.patch.object(PLUGIN.bob_route, "probe", return_value=self.good_probe(proxy.port)):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertIn("old_rc_block=removed", out)
        self.assertFalse(self.rc.exists())
        self.assertTrue(ROUTE.is_routed(self.home / ".zshrc"))

    def test_a_shell_change_leaves_an_edited_old_block_and_says_so(self):
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.run_main("setup", "--i-consent-to-traffic-interception")
            self.rc.write_bytes(self.rc.read_bytes().replace(b"fi\n", b"fi\n# my edit\n"))
            edited = self.rc.read_bytes()
            with mock.patch.dict(os.environ, {"SHELL": "/bin/zsh"}), \
                 mock.patch.object(PLUGIN.bob_route, "probe", return_value=self.good_probe(proxy.port)):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertIn("old_rc_block=left_alone", out)
        self.assertEqual(self.rc.read_bytes(), edited)

    def test_a_failed_rerun_after_a_shell_change_keeps_bob_routed_through_the_old_file(self):
        # The old block is removed only after the last step that can fail. A re-run under zsh that
        # fails later must leave ~/.bash_profile routing Bob, or the rollback leaves Bob routed
        # nowhere while it reports rolled_back=true.
        zshrc = self.home / ".zshrc"
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
            zshrc.write_bytes(b"alias ll='ls -l'\n")
            with mock.patch.dict(os.environ, {"SHELL": "/bin/zsh"}), \
                 mock.patch.object(PLUGIN.bob_hooks, "install", side_effect=OSError("boom")):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1, out)
        self.assertIn("rolled_back=true", out)
        self.assertNotIn("old_rc_block=removed", out)
        self.assertTrue(ROUTE.is_routed(self.rc), "Bob is routed nowhere after the rollback")
        self.assertEqual(zshrc.read_bytes(), b"alias ll='ls -l'\n")

    def test_setup_starts_the_service_then_routes_and_verifies(self):
        self.rc.write_bytes(b"alias g=git\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertIn("result=installed", out)
        self.assertIn("rc_probe=ok", out)
        self.assertIn("log in to Bob once", out)
        self.assertIn("quit and reopen Bob", out)
        with open(PLUGIN.launchd_plist(), "rb") as handle:
            plist = plistlib.load(handle)
        state = self.root / "state/context-guru-bob"
        self.assertEqual(plist["Label"], "com.rossoctl.context-guru.bob")
        self.assertTrue(plist["KeepAlive"])
        self.assertTrue(plist["ContextGuruManaged"])
        self.assertEqual(plist["ProgramArguments"], [
            "/opt/cg/context-guru-proxy", "--listen", f"127.0.0.1:{proxy.port}",
            "--bob-upstream", "https://api.us-east.bob.ibm.com", "--dashboard",
            "--dashboard-db", str(state / f"dashboard-{proxy.port}.db"),
            "--config", str(state / f"bob-{proxy.port}.yaml")])
        self.assertNotIn("--idle-exit", plist["ProgramArguments"])
        self.assertEqual((state / f"bob-{proxy.port}.yaml").read_text(),
                         PLUGIN.CONFIG_MARKER + "\npipeline: [format, toon, cachesplit]\n")
        self.assertEqual(self.rc.read_bytes(),
                         b"alias g=git\n" + ROUTE.block(proxy.port))
        self.assertTrue(HOOKS.is_installed(self.home / ".bob/settings/settings.json"))
        self.assertTrue((state / "context-guru-reset").exists())
        uid = os.getuid()
        self.assertIn(f"bootstrap gui/{uid} {PLUGIN.launchd_plist()}", self.launchctl_calls())

    def test_a_proxy_that_never_gets_healthy_routes_nothing(self):
        self.rc.write_bytes(b"x\n")
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=dead_port()), \
             mock.patch.object(PLUGIN, "HEALTH_WAIT_S", 0.3):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertIn("result=start_failed", out)
        self.assertEqual(self.rc.read_bytes(), b"x\n")
        self.assertFalse((self.home / ".bob/settings/settings.json").exists())
        self.assertFalse(PLUGIN.launchd_plist().exists())

    def test_jsonc_settings_refuse_before_anything_starts(self):
        settings = self.home / ".bob/settings/settings.json"
        settings.parent.mkdir(parents=True)
        settings.write_text('{ // mine\n}\n')
        self.rc.write_bytes(b"x\n")
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 2)
        self.assertIn("result=refused\nreason=settings_not_plain_json", out)
        self.assertEqual(settings.read_text(), '{ // mine\n}\n')
        self.assertEqual(self.launchctl_calls(), [])
        self.assertFalse(PLUGIN.state_dir().exists())

    def test_a_late_failure_rolls_back_hooks_service_and_rc_block(self):
        rc = self.rc
        rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port), \
             mock.patch.object(PLUGIN, "install_escape_hatch", side_effect=OSError("disk full")):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertIn("result=setup_failed", out)
        self.assertIn("rolled_back=true", out)
        self.assertEqual(rc.read_bytes(), b"x\n")
        self.assertFalse((self.home / ".bob/settings/settings.json").exists())
        self.assertFalse(PLUGIN.launchd_plist().exists())
        self.assertIn(f"bootout gui/{os.getuid()}/com.rossoctl.context-guru.bob",
                      self.launchctl_calls())
        self.assertFalse(PLUGIN.record_path().exists())

    def test_unknown_shell_prints_the_block_and_writes_no_rc_file(self):
        os.environ["SHELL"] = "/usr/local/bin/fish"
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertIn("result=installed_route_by_hand", out)
        self.assertIn(f"export VITE_GATEWAY_BASE_URL=http://127.0.0.1:{proxy.port}", out)
        self.assertFalse(any(p.name.startswith(".") and p.is_file() for p in self.home.iterdir()))

    def test_a_probe_miss_is_reported_plainly(self):
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port), \
             mock.patch.object(PLUGIN.bob_route, "probe", return_value={
                 "ide": "", "terminal_ide": "", "terminal_shell": ""}):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertIn("result=installed_not_verified", out)
        self.assertIn("rc_probe=miss", out)


class LoopbackNeverUsesTheUserProxyTest(TempHome):
    """A corporate Mac sets HTTP_PROXY with no NO_PROXY for 127.0.0.1. The plugin's calls to its
    own local proxy must still go straight to it: through the corporate proxy they fail, and the
    signal hook would send the Bob session id off the machine."""

    def setUp(self):
        super().setUp()
        self.corp = FakeProxy()
        self.corp.__enter__()
        self.addCleanup(self.corp.__exit__, None, None, None)
        env = {"HTTP_PROXY": f"http://127.0.0.1:{self.corp.port}",
               "http_proxy": f"http://127.0.0.1:{self.corp.port}"}
        patch = mock.patch.dict(os.environ, env)
        patch.start()
        self.addCleanup(patch.stop)
        for name in ("NO_PROXY", "no_proxy"):
            os.environ.pop(name, None)

    def assert_nothing_reached_the_user_proxy(self):
        self.assertEqual((self.corp.paths, self.corp.signals), ([], []),
                         "a call to 127.0.0.1 went through the user's HTTP_PROXY")

    def test_status_reaches_the_local_proxy_directly(self):
        import time
        with FakeProxy(last_bob_ts_ms=int(time.time() * 1000) - 60_000) as proxy:
            ROUTE.install(self.rc, proxy.port)
            self.write_record(proxy.port, rc_file=str(self.rc))
            code, out, _ = self.run_main("status")
        self.assertEqual(code, 0, out)
        self.assertIn("proxy_up=true", out)
        self.assertIn("/api/requests?limit=1&agent=bob", proxy.paths)
        self.assert_nothing_reached_the_user_proxy()

    def test_signal_reaches_the_local_proxy_directly(self):
        payload = {"session_id": "task-123", "hook_event_name": "SessionStart", "source": "compact"}
        with FakeProxy() as proxy:
            self.write_record(proxy.port)
            self.assertEqual(self.run_main("signal", stdin=json.dumps(payload)), (0, "", ""))
        self.assertEqual(len(proxy.signals), 1)
        self.assert_nothing_reached_the_user_proxy()


class StatusTest(TempHome):
    def test_status_reports_route_health_and_recent_bob_traffic(self):
        import time
        with FakeProxy(last_bob_ts_ms=int(time.time() * 1000) - 120_000) as proxy:
            ROUTE.install(self.rc, proxy.port)
            self.write_record(proxy.port, rc_file=str(self.rc))
            code, out, _ = self.run_main("status")
        self.assertEqual(code, 0, out)
        self.assertIn("result=ok", out)
        self.assertIn("routed=true", out)
        self.assertIn("proxy_up=true", out)
        self.assertRegex(out, r"last_bob_request_age_s=1[12]\d\n")
        self.assertIn("/api/requests?limit=1&agent=bob", proxy.paths)
        self.assertIn("log in to Bob once", out)

    def test_status_with_no_bob_traffic_says_so(self):
        with FakeProxy() as proxy:
            ROUTE.install(self.rc, proxy.port)
            self.write_record(proxy.port, rc_file=str(self.rc))
            code, out, _ = self.run_main("status")
        self.assertEqual(code, 1)
        self.assertIn("result=no_bob_traffic", out)
        self.assertIn("last_bob_request_age_s=(none)", out)

    def test_status_with_proxy_down(self):
        port = dead_port()
        self.write_record(port, rc_file=str(self.rc))
        code, out, _ = self.run_main("status")
        self.assertEqual(code, 1)
        self.assertIn("proxy_up=false", out)
        self.assertIn("routed=false", out)

    def test_status_not_installed(self):
        code, out, _ = self.run_main("status")
        self.assertEqual(code, 1)
        self.assertIn("result=not_installed", out)


class HookHandlerTest(TempHome):
    def test_ensure_with_healthy_proxy_prints_nothing_and_starts_nothing(self):
        with FakeProxy() as proxy:
            self.write_record(proxy.port)
            self.assertEqual(self.run_main("ensure"), (0, "", ""))
        self.assertEqual(self.launchctl_calls(), [])

    def test_ensure_with_proxy_down_kicks_the_service_and_prints_nothing(self):
        self.write_record(dead_port())
        PLUGIN.launchd_plist().parent.mkdir(parents=True)
        with open(PLUGIN.launchd_plist(), "wb") as handle:
            plistlib.dump({"Label": PLUGIN.LABEL, "ContextGuruManaged": True}, handle)
        self.assertEqual(self.run_main("ensure"), (0, "", ""))
        self.assertIn(f"kickstart gui/{os.getuid()}/{PLUGIN.LABEL}", self.launchctl_calls())

    def test_ensure_never_touches_a_service_that_is_not_ours(self):
        self.write_record(dead_port())
        PLUGIN.launchd_plist().parent.mkdir(parents=True)
        with open(PLUGIN.launchd_plist(), "wb") as handle:
            plistlib.dump({"Label": PLUGIN.LABEL}, handle)
        self.assertEqual(self.run_main("ensure"), (0, "", ""))
        self.assertEqual(self.launchctl_calls(), [])

    def test_ensure_not_installed_and_broken_state_exit_zero_silently(self):
        self.assertEqual(self.run_main("ensure"), (0, "", ""))
        PLUGIN.state_dir().mkdir(parents=True)
        PLUGIN.record_path().write_text("{not json")
        self.assertEqual(self.run_main("ensure"), (0, "", ""))

    def test_check_with_healthy_proxy_prints_nothing(self):
        with FakeProxy() as proxy:
            self.write_record(proxy.port)
            self.assertEqual(self.run_main("check", stdin=json.dumps({"prompt": CANARY})),
                             (0, "", ""))

    def test_check_with_proxy_down_prints_one_line(self):
        port = dead_port()
        self.write_record(port)
        code, out, err = self.run_main("check")
        self.assertEqual((code, err), (0, ""))
        self.assertEqual(out.count("\n"), 1)
        self.assertIn(f"127.0.0.1:{port}", out)
        self.assertIn("context-guru-reset --yes", out)

    def test_check_not_installed_or_broken_prints_nothing(self):
        self.assertEqual(self.run_main("check"), (0, "", ""))
        PLUGIN.state_dir().mkdir(parents=True)
        PLUGIN.record_path().write_text('{"port": "x"}')
        self.assertEqual(self.run_main("check"), (0, "", ""))

    def test_signal_forwards_the_session_start_fields_to_the_proxy(self):
        payload = {"session_id": "task-123", "cwd": "/Users/u/secret-project",
                   "hook_event_name": "SessionStart", "source": "compact"}
        with FakeProxy() as proxy:
            self.write_record(proxy.port)
            self.assertEqual(self.run_main("signal", stdin=json.dumps(payload)), (0, "", ""))
        self.assertEqual(len(proxy.signals), 1)
        path, content_type, body = proxy.signals[0]
        self.assertEqual((path, content_type), ("/signal", "application/json"))
        self.assertEqual(json.loads(body), {"session_id": "task-123",
                                            "hook_event_name": "SessionStart",
                                            "source": "compact"})

    def test_signal_sends_nothing_for_events_the_proxy_refuses(self):
        for payload in ({"session_id": "t", "hook_event_name": "Stop",
                         "last_assistant_message": CANARY},
                        {"session_id": "t", "hook_event_name": "UserPromptSubmit",
                         "prompt": CANARY},
                        {"session_id": "../x", "hook_event_name": "SessionStart"}):
            with self.subTest(payload=payload), FakeProxy() as proxy:
                self.write_record(proxy.port)
                self.assertEqual(self.run_main("signal", stdin=json.dumps(payload)), (0, "", ""))
                self.assertEqual(proxy.signals, [])

    def test_signal_exits_zero_when_down_or_given_garbage(self):
        self.write_record(dead_port())
        for stdin in ('{"session_id":"t","hook_event_name":"SessionStart","source":"startup"}',
                      "not json", "", "[1]"):
            with self.subTest(stdin=stdin):
                self.assertEqual(self.run_main("signal", stdin=stdin), (0, "", ""))


class CanaryTest(TempHome):
    """A canary prompt on UserPromptSubmit stdin never reaches any file or any output."""

    def test_canary_prompt_is_never_written_or_printed(self):
        payload = json.dumps({"session_id": "task-1", "cwd": str(self.home),
                              "hook_event_name": "UserPromptSubmit", "prompt": CANARY})
        outputs = []
        with FakeProxy() as proxy:
            self.write_record(proxy.port)
            groups = HOOKS.our_groups(sys.executable, str(HERE / "bob_plugin.py"))
            for state in ("up", "down"):
                if state == "down":
                    self.write_record(dead_port())
                for group in groups.values():
                    for handler in group["hooks"]:
                        done = subprocess.run(["/bin/sh", "-c", handler["command"]], input=payload,
                                              text=True, capture_output=True, timeout=15,
                                              cwd=self.home)
                        self.assertEqual(done.returncode, 0)
                        outputs += [done.stdout, done.stderr]
                for sub in ("check", "signal", "ensure", "status"):
                    done = subprocess.run([sys.executable, str(HERE / "bob_plugin.py"), sub],
                                          input=payload, text=True, capture_output=True,
                                          timeout=15, cwd=self.home)
                    outputs += [done.stdout, done.stderr]
            sent = b"".join(body for _, _, body in proxy.signals)
        self.assertNotIn(CANARY.encode(), sent)
        for text in outputs:
            self.assertNotIn(CANARY, text)
        for path in self.root.rglob("*"):
            if path.is_file() and not path.is_symlink():
                self.assertNotIn(CANARY.encode(), path.read_bytes(), str(path))


class UninstallTest(TempHome):
    def install(self, proxy):
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)

    def test_uninstall_restores_files_byte_identical_and_keeps_data(self):
        rc = self.rc
        rc.write_bytes(b"export A=1")  # no trailing newline
        settings = self.home / ".bob/settings/settings.json"
        settings.parent.mkdir(parents=True)
        original = b'{"bob.x": 1, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "a"}]}]}}'
        settings.write_bytes(original)
        with FakeProxy() as proxy:
            self.install(proxy)
            db = PLUGIN.state_dir() / f"dashboard-{proxy.port}.db"
            db.write_bytes(b"sqlite")
            code, out, _ = self.run_main("uninstall")
        self.assertEqual(code, 0, out)
        self.assertIn("result=removed", out)
        self.assertIn("quit and reopen Bob, then log in once", out)
        self.assertEqual(rc.read_bytes(), b"export A=1")
        self.assertEqual(settings.read_bytes(), original)
        self.assertFalse(PLUGIN.launchd_plist().exists())
        self.assertIn(f"bootout gui/{os.getuid()}/{PLUGIN.LABEL}", self.launchctl_calls())
        self.assertTrue(db.exists())
        self.assertFalse(PLUGIN.record_path().exists())
        self.assertFalse((PLUGIN.state_dir() / f"bob-{proxy.port}.yaml").exists())

    def test_uninstall_purge_removes_the_state_folder(self):
        with FakeProxy() as proxy:
            self.install(proxy)
            code, out, _ = self.run_main("uninstall", "--purge")
        self.assertEqual(code, 0, out)
        self.assertFalse(PLUGIN.state_dir().exists())
        self.assertFalse(self.rc.exists())
        self.assertFalse((self.home / ".bob/settings/settings.json").exists())

    def test_uninstall_dry_run_changes_nothing(self):
        with FakeProxy() as proxy:
            self.install(proxy)
            before = self.tree()
            calls = len(self.launchctl_calls())
            code, out, _ = self.run_main("uninstall", "--dry-run")
        self.assertEqual(code, 0)
        self.assertIn("result=planned", out)
        self.assertEqual(self.tree(), before)
        self.assertEqual(len(self.launchctl_calls()), calls)

    def test_uninstall_leaves_an_edited_rc_block_and_says_so(self):
        with FakeProxy() as proxy:
            self.install(proxy)
            rc = self.rc
            rc.write_bytes(rc.read_bytes().replace(b"fi\n", b"fi\n# mine\n"))
            edited = rc.read_bytes()
            code, out, _ = self.run_main("uninstall")
        self.assertEqual(code, 1)
        self.assertIn("rc_block=left_alone", out)
        self.assertEqual(rc.read_bytes(), edited)
        self.assertFalse(HOOKS.is_installed(self.home / ".bob/settings/settings.json"))


@unittest.skipUnless(shutil.which("osascript") and Path("/usr/bin/perl").exists(),
                     "reset.sh is macOS only (plan D14): it needs osascript and /usr/bin/perl")
class ResetScriptTest(TempHome):
    def install(self, proxy, rc_bytes, settings_bytes):
        self.rc.write_bytes(rc_bytes)
        settings = self.home / ".bob/settings/settings.json"
        settings.parent.mkdir(parents=True)
        settings.write_bytes(settings_bytes)
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        return settings

    def reset(self, *args):
        return subprocess.run(["/bin/sh", str(PLUGIN.state_dir() / "context-guru-reset"), *args],
                              env=os.environ.copy(), text=True, capture_output=True, timeout=30)

    def test_reset_keeps_a_proxy_config_the_user_wrote(self):
        # uninstall deletes the config only when it starts with our marker; reset must too.
        with FakeProxy() as proxy:
            self.install(proxy, b"x\n", b"{}\n")
            config = PLUGIN.state_dir() / f"bob-{proxy.port}.yaml"
            config.write_text("pipeline: [format]\n# mine\n")
            done = self.reset("--yes")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(config.read_text(), "pipeline: [format]\n# mine\n")

    def test_reset_uses_the_same_config_marker_as_the_plugin(self):
        # reset.sh needs no Python, so it repeats the marker. The two must not drift.
        self.assertIn(f'= "{PLUGIN.CONFIG_MARKER}" ]', (HERE / "reset.sh").read_text())

    def test_reset_removes_our_own_proxy_config(self):
        with FakeProxy() as proxy:
            self.install(proxy, b"x\n", b"{}\n")
            config = PLUGIN.state_dir() / f"bob-{proxy.port}.yaml"
            self.assertTrue(config.read_text().startswith(PLUGIN.CONFIG_MARKER))
            done = self.reset("--yes")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertFalse(config.exists())

    def test_reset_removes_a_legacy_block(self):
        with FakeProxy() as proxy:
            self.install(proxy, b"alias g=git", b"{}\n")
            # The block as setup wrote it before 2026-11-09.
            self.rc.write_bytes(b"alias g=git" + RouteTest.LEGACY_BLOCK.replace(
                b"8891", str(proxy.port).encode()))
            done = self.reset("--yes")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("rc_block=removed", done.stdout)
        self.assertEqual(self.rc.read_bytes(), b"alias g=git")

    def test_dry_run_changes_nothing(self):
        with FakeProxy() as proxy:
            self.install(proxy, b"x\n", b"{}\n")
            before = self.tree()
            calls = len(self.launchctl_calls())
            done = self.reset("--dry-run")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("result=planned", done.stdout)
        self.assertEqual(self.tree(), before)
        self.assertEqual(len(self.launchctl_calls()), calls)

    def test_reset_removes_the_block_our_hooks_and_the_agent_without_python(self):
        settings_bytes = (b'{\n  "z": 1,\n  "hooks": {\n    "Stop": [\n      {\n        "hooks": [\n'
                          b'          {\n            "type": "command",\n            "command": "a"\n'
                          b'          }\n        ]\n      }\n    ]\n  }\n}\n')
        with FakeProxy() as proxy:
            settings = self.install(proxy, b"alias g=git", settings_bytes)
            # Bob rewrote the file since: the reset must still remove only ours.
            data = json.loads(settings.read_text())
            data["late"] = True
            settings.write_text(json.dumps(data, indent=2) + "\n")
            done = self.reset("--yes")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("rc_block=removed", done.stdout)
        self.assertIn("hooks=removed", done.stdout)
        self.assertIn("result=removed", done.stdout)
        self.assertEqual(self.rc.read_bytes(), b"alias g=git")
        self.assertEqual(json.loads(settings.read_text()), {
            "z": 1, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "a"}]}]},
            "late": True})
        self.assertEqual(list(json.loads(settings.read_text())), ["z", "hooks", "late"])
        self.assertFalse(PLUGIN.launchd_plist().exists())
        self.assertIn(f"bootout gui/{os.getuid()}/{PLUGIN.LABEL}", self.launchctl_calls())
        self.assertFalse(PLUGIN.record_path().exists())

    def test_reset_drops_the_hooks_key_it_added(self):
        with FakeProxy() as proxy:
            settings = self.install(proxy, b"x\n", b'{"a": 1}')
            data = json.loads(settings.read_text())
            data["late"] = True
            settings.write_text(json.dumps(data, indent=2) + "\n")
            done = self.reset("--yes")
        self.assertIn("hooks=removed", done.stdout)
        self.assertEqual(json.loads(settings.read_text()), {"a": 1, "late": True})

    def test_reset_leaves_an_edited_block(self):
        with FakeProxy() as proxy:
            self.install(proxy, b"x\n", b"{}\n")
            rc = self.rc
            rc.write_bytes(rc.read_bytes().replace(b"fi\n", b"fi\n# mine\n"))
            edited = rc.read_bytes()
            done = self.reset("--yes")
        self.assertIn("rc_block=left_alone", done.stdout)
        self.assertEqual(rc.read_bytes(), edited)


# ---------------------------------------------------------------------------- review fixes

def fake_binary(folder, version_line, has_bob=True):
    """A fake context-guru-proxy that answers --version and --help."""
    path = Path(folder) / "context-guru-proxy"
    flag = "  -bob-upstream string" if has_bob else "  -openai-upstream string"
    path.write_text("#!/bin/sh\n"
                    f'case "$1" in --version) echo "{version_line}" ;; '
                    f'--help|-h) echo "Usage:"; echo "{flag}" ;; esac\nexit 0\n')
    path.chmod(0o755)
    return path


class HooksRerunTest(TempHome):
    """Finding 1: a second setup must not make uninstall throw away the user's later edits."""

    def settings(self):
        return self.home / ".bob/settings/settings.json"

    def state(self):
        return self.root / "state/context-guru-bob/hooks"

    def groups(self):
        return HOOKS.our_groups("/usr/bin/python3", "/p/scripts/bob_plugin.py")

    def add_user_key(self):
        data = json.loads(self.settings().read_text())
        data["user.added"] = "keep me"
        self.settings().write_text(json.dumps(data, indent=2) + "\n")

    def test_absent_then_setup_edit_setup_uninstall_keeps_the_edit_and_the_file(self):
        HOOKS.install(self.settings(), self.state(), self.groups())
        self.add_user_key()
        HOOKS.install(self.settings(), self.state(), self.groups())
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "removed")
        self.assertEqual(json.loads(self.settings().read_text()), {"user.added": "keep me"})

    def test_existed_then_setup_edit_setup_uninstall_keeps_the_edit(self):
        self.settings().parent.mkdir(parents=True)
        self.settings().write_text('{"first": 1}\n')
        HOOKS.install(self.settings(), self.state(), self.groups())
        self.add_user_key()
        HOOKS.install(self.settings(), self.state(), self.groups())
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "removed")
        self.assertEqual(json.loads(self.settings().read_text()),
                         {"first": 1, "user.added": "keep me"})

    def test_setup_twice_with_no_edit_still_restores_byte_identical(self):
        self.settings().parent.mkdir(parents=True)
        self.settings().write_bytes(b'{"first":1}')
        HOOKS.install(self.settings(), self.state(), self.groups())
        HOOKS.install(self.settings(), self.state(), self.groups())
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "restored")
        self.assertEqual(self.settings().read_bytes(), b'{"first":1}')

    def test_missing_before_json_falls_back_to_strip_by_tag(self):
        """Finding 7: stale state must not raise."""
        self.settings().parent.mkdir(parents=True)
        self.settings().write_text('{"first": 1}\n')
        HOOKS.install(self.settings(), self.state(), self.groups())
        (self.state() / "before.json").unlink()
        self.assertEqual(HOOKS.remove(self.settings(), self.state()), "removed")
        self.assertEqual(json.loads(self.settings().read_text()), {"first": 1})


class SettingsSymlinkTest(TempHome):
    """Finding 2: a symlinked settings.json stays a link; the target gets the change."""

    def settings(self):
        return self.home / ".bob/settings/settings.json"

    def link_to_dotfiles(self, data):
        dotfiles = self.root / "dotfiles"
        dotfiles.mkdir()
        target = dotfiles / "bob-settings.json"
        target.write_bytes(data)
        self.settings().parent.mkdir(parents=True)
        self.settings().symlink_to(target)
        return target

    def test_setup_and_uninstall_keep_the_link_and_edit_the_target(self):
        target = self.link_to_dotfiles(b'{"mine": true}\n')
        state = self.root / "state/context-guru-bob/hooks"
        HOOKS.install(self.settings(), state, HOOKS.our_groups("/py", "/s.py"))
        self.assertTrue(self.settings().is_symlink())
        self.assertTrue(HOOKS.is_installed(target))
        HOOKS.remove(self.settings(), state)
        self.assertTrue(self.settings().is_symlink())
        self.assertEqual(target.read_bytes(), b'{"mine": true}\n')

    def test_a_dangling_or_deep_settings_link_is_refused(self):
        self.settings().parent.mkdir(parents=True)
        self.settings().symlink_to(self.root / "gone.json")
        state = self.root / "state/context-guru-bob/hooks"
        with self.assertRaisesRegex(HOOKS.HooksRefused, "dangling"):
            HOOKS.install(self.settings(), state, HOOKS.our_groups("/py", "/s.py"))
        self.assertFalse((self.root / "gone.json").exists())
        self.settings().unlink()
        (self.root / "real.json").write_text("{}")
        (self.root / "mid.json").symlink_to(self.root / "real.json")
        self.settings().symlink_to(self.root / "mid.json")
        with self.assertRaisesRegex(HOOKS.HooksRefused, "deeper than 1"):
            HOOKS.install(self.settings(), state, HOOKS.our_groups("/py", "/s.py"))

    @unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
    def test_reset_keeps_the_link(self):
        target = self.link_to_dotfiles(b'{"mine": true}\n')
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
            self.assertEqual(code, 0, out)
            data = json.loads(target.read_text())
            data["late"] = 1
            target.write_text(json.dumps(data))
            done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"],
                                  text=True, capture_output=True, timeout=30)
        self.assertIn("hooks=removed", done.stdout, done.stdout + done.stderr)
        self.assertTrue(self.settings().is_symlink())
        self.assertEqual(json.loads(target.read_text()), {"mine": True, "late": 1})


class InterruptedSetupTest(TempHome):
    """Finding 3: a setup killed after the rc write must still be undoable."""

    def crash_after_rc(self, error=SystemExit):
        real_install = HOOKS.install

        def boom(*args, **kwargs):
            raise error("killed")

        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port), \
             mock.patch.object(PLUGIN.bob_hooks, "install", side_effect=boom):
            try:
                self.run_main("setup", "--i-consent-to-traffic-interception")
            except BaseException:
                pass
        del real_install
        return proxy.port

    def test_a_kill_leaves_a_record_and_uninstall_removes_the_block(self):
        self.rc.write_bytes(b"x\n")
        port = self.crash_after_rc()  # SystemExit: like a kill, no rollback runs
        self.assertTrue(ROUTE.is_routed(self.rc, port))
        record = json.loads(PLUGIN.record_path().read_text())
        self.assertEqual(record["rc_file"], str(self.rc))
        code, out, _ = self.run_main("uninstall")
        self.assertIn("rc_block=removed", out)
        self.assertEqual(self.rc.read_bytes(), b"x\n")

    def test_ctrl_c_rolls_back(self):
        self.rc.write_bytes(b"x\n")
        self.crash_after_rc(KeyboardInterrupt)
        self.assertEqual(self.rc.read_bytes(), b"x\n")
        self.assertFalse(PLUGIN.launchd_plist().exists())

    def test_rollback_keeps_the_record_when_an_undo_fails(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port), \
             mock.patch.object(PLUGIN, "install_escape_hatch", side_effect=OSError("disk")), \
             mock.patch.object(PLUGIN.bob_route, "remove", side_effect=OSError("busy")):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertIn("rolled_back=false", out)
        self.assertIn("rollback_failed=rc", out)
        self.assertEqual(json.loads(PLUGIN.record_path().read_text())["rc_file"], str(self.rc))

    @unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
    def test_reset_without_a_record_scans_for_our_block_and_hooks(self):
        self.rc.write_bytes(b"x\n")
        self.crash_after_rc()
        HOOKS.install(self.home / ".bob/settings/settings.json", PLUGIN.hooks_state(),
                      HOOKS.our_groups("/py", "/s.py"))
        shutil.copyfile(HERE / "reset.sh", self.root / "reset.sh")
        PLUGIN.record_path().unlink()
        shutil.rmtree(PLUGIN.hooks_state())
        done = subprocess.run(["/bin/sh", str(self.root / "reset.sh"), "--yes"],
                              text=True, capture_output=True, timeout=30)
        self.assertIn("rc_block=removed", done.stdout, done.stdout + done.stderr)
        self.assertIn("hooks=removed", done.stdout)
        self.assertEqual(self.rc.read_bytes(), b"x\n")
        self.assertFalse(HOOKS.is_installed(self.home / ".bob/settings/settings.json"))


class ServiceRestartTest(TempHome):
    """Finding 4: a failed first bootstrap is retried, and a re-run never tears down old steps."""

    def test_bootstrap_is_retried_after_a_failure(self):
        fail = self.root / "fail"
        fail.write_text("1")
        os.environ["CG_TEST_FAIL_BOOTSTRAP"] = str(fail)
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 0, out)
        self.assertEqual(sum(c.startswith("bootstrap") for c in self.launchctl_calls()), 2)
        self.assertTrue(any(c.startswith("print") for c in self.launchctl_calls()))

    def test_a_failed_rerun_keeps_the_working_install(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
            with mock.patch.object(PLUGIN, "install_escape_hatch", side_effect=OSError("disk")):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 1)
        self.assertTrue(ROUTE.is_routed(self.rc, proxy.port))
        self.assertTrue(HOOKS.is_installed(self.home / ".bob/settings/settings.json"))
        self.assertTrue(PLUGIN.launchd_plist().exists())
        self.assertTrue(PLUGIN.record_path().exists())


class BinaryCheckTest(TempHome):
    """Finding 5: an old proxy on PATH without --bob-upstream is not used."""

    def test_an_old_binary_on_path_is_treated_as_missing(self):
        folder = self.root / "oldbin"
        folder.mkdir()
        fake_binary(folder, "context-guru-proxy v0.2.0 (commit abc)", has_bob=False)
        os.environ["CONTEXT_GURU_BIN"] = str(folder / "context-guru-proxy")
        reasons = []
        self.assertIsNone(PLUGIN.find_binary(reasons))
        self.assertIn("no --bob-upstream", reasons[0])

    def test_a_new_binary_is_accepted(self):
        folder = self.root / "newbin"
        folder.mkdir()
        path = fake_binary(folder, "context-guru-proxy v0.5.1 (commit abc)")
        os.environ["CONTEXT_GURU_BIN"] = str(path)
        self.assertEqual(PLUGIN.find_binary(), str(path))

    def test_setup_says_why_it_downloads(self):
        folder = self.root / "oldbin"
        folder.mkdir()
        fake_binary(folder, "context-guru-proxy v0.2.0", has_bob=False)
        os.environ["CONTEXT_GURU_BIN"] = str(folder / "context-guru-proxy")
        with mock.patch.object(PLUGIN, "install_release_binary",
                               side_effect=RuntimeError("offline")):
            code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 2)
        self.assertIn("binary_skipped=", out)
        self.assertIn("no --bob-upstream", out)
        self.assertIn("result=binary_install_failed", out)


@unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
class NonAsciiHomeTest(TempHome):
    """Finding 6: reset.sh must find the rc file under a non-ASCII HOME."""

    def setUp(self):
        super().setUp()
        accented = self.root / "jos\u00e9"
        accented.mkdir()
        os.environ["HOME"] = str(accented)
        self.home = accented
        self.rc = accented / ".bash_profile"

    def test_reset_removes_the_block_under_a_non_ascii_home(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
            # The record must hold the real path, not \u escapes.
            self.assertIn(str(self.rc), PLUGIN.record_path().read_text(encoding="utf-8"))
            done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"],
                                  text=True, capture_output=True, timeout=30)
        self.assertIn("rc_block=removed", done.stdout, done.stdout + done.stderr)
        self.assertEqual(self.rc.read_bytes(), b"x\n")

    def test_a_path_with_a_quote_is_refused(self):
        quoted = self.root / 'a"b'
        quoted.mkdir()
        os.environ["HOME"] = str(quoted)
        code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception")
        self.assertEqual(code, 2)
        self.assertIn("reason=unsafe_path", out)


class UninstallRobustTest(TempHome):
    """Finding 7: one failing step must not skip the others."""

    def test_an_oserror_in_hooks_still_removes_the_rc_block_and_service(self):
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
        with mock.patch.object(PLUGIN.bob_hooks, "remove", side_effect=OSError("denied")):
            code, out, _ = self.run_main("uninstall")
        self.assertEqual(code, 1)
        self.assertIn("hooks=failed", out)
        self.assertIn("rc_block=removed", out)
        self.assertIn("service=stopped", out)
        self.assertIn("result=partly_removed", out)


class MinorFixesTest(TempHome):
    """Finding 8."""

    def test_state_and_backup_folders_are_private(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, \
             mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            self.assertEqual(self.run_main("setup", "--i-consent-to-traffic-interception")[0], 0)
        for folder in (PLUGIN.state_dir(), PLUGIN.state_dir() / "backup"):
            self.assertEqual(stat.S_IMODE(folder.stat().st_mode), 0o700, folder)

    def test_resolve_reports_an_unreadable_home_as_advice(self):
        with mock.patch.object(ROUTE.os.path, "lexists", side_effect=PermissionError("denied")):
            target, advice = ROUTE.resolve(self.home, "/bin/bash")
        self.assertIsNone(target)
        self.assertIn("cannot read", advice)
        with mock.patch.object(ROUTE.os.path, "islink", side_effect=OSError(62, "loop")):
            target, advice = ROUTE.resolve(self.home, "/bin/zsh")
        self.assertIsNone(target)
        self.assertIn("cannot read", advice)

    def test_upstream_must_be_a_bob_https_host(self):
        for bad in ("http://api.us-east.bob.ibm.com", "https://evil.example.com",
                    "https://bob.ibm.com.evil.com", "https://x.bob.ibm.com@evil.com",
                    "https://x.bob.ibm.com/path"):
            with self.subTest(upstream=bad):
                code, out, _ = self.run_main("setup", "--i-consent-to-traffic-interception",
                                             "--upstream", bad)
                self.assertEqual(code, 2)
                self.assertIn("reason=bad_upstream", out)
        self.assertEqual(self.launchctl_calls(), [])
        self.assertTrue(PLUGIN.valid_upstream("https://api.eu-de.bob.ibm.com"))

    def test_setup_skill_names_the_facts_plan_prints(self):
        text = (PLUGIN_ROOT / "skills/setup/SKILL.md").read_text()
        code, out, _ = self.run_main("setup", "--plan")
        step = next(line for line in text.splitlines() if "Show the user what it found" in line)
        for key in re.findall(r"`([a-z_]+)=`", step):
            self.assertIn(f"\n{key}=", "\n" + out, key)


class SecondReviewTest(TempHome):
    """Second review: failure paths of setup, a stale hooks state, unsafe rc paths, small gaps."""

    def setup_with(self, proxy, *extra, **patches):
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(PLUGIN, "find_binary",
                                                  return_value="/opt/cg/context-guru-proxy"))
            stack.enter_context(mock.patch.object(PLUGIN, "first_free_port",
                                                  return_value=proxy.port))
            for name, value in patches.items():
                target = PLUGIN.bob_hooks if name == "hooks_install" else PLUGIN
                attr = "install" if name == "hooks_install" else name
                stack.enter_context(mock.patch.object(target, attr, value))
            return self.run_main("setup", "--i-consent-to-traffic-interception", *extra)

    # ---- 1: a step that fails half way is still undone

    def test_every_bootstrap_failing_leaves_no_plist_and_no_record(self):
        fail = self.root / "fail"
        fail.write_text("99")
        os.environ["CG_TEST_FAIL_BOOTSTRAP"] = str(fail)
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy, mock.patch.object(PLUGIN.time, "sleep"):
            code, out, _ = self.setup_with(proxy)
        self.assertEqual(code, 1)
        self.assertIn("result=setup_failed", out)
        self.assertIn("rolled_back=true", out)
        self.assertFalse(PLUGIN.launchd_plist().exists())
        self.assertFalse(PLUGIN.record_path().exists())
        self.assertEqual(self.rc.read_bytes(), b"x\n")

    def test_hooks_failing_after_the_settings_write_are_removed(self):
        real = HOOKS.install

        def half(path, state_dir, groups):
            real(path, state_dir, groups)
            raise OSError("after.json: disk full")

        with FakeProxy() as proxy:
            code, out, _ = self.setup_with(proxy, hooks_install=half)
        self.assertEqual(code, 1)
        self.assertIn("rolled_back=true", out)
        self.assertFalse(HOOKS.is_installed(self.home / ".bob/settings/settings.json"))

    # ---- 2: a failed re-run puts the old service back, or says it could not

    def first_install(self, proxy):
        code, out, _ = self.setup_with(proxy)
        self.assertEqual(code, 0, out)
        return PLUGIN.launchd_plist().read_bytes()

    def test_a_failed_rerun_restores_the_old_plist_and_service(self):
        with FakeProxy() as proxy:
            old = self.first_install(proxy)
            calls = len(self.launchctl_calls())
            code, out, _ = self.setup_with(
                proxy, find_binary=mock.Mock(return_value="/opt/new/context-guru-proxy"),
                wait_healthy=mock.Mock(side_effect=[False, True]))
        self.assertEqual(code, 1)
        self.assertIn("result=start_failed", out)
        self.assertIn("rolled_back=true", out)
        self.assertEqual(PLUGIN.launchd_plist().read_bytes(), old)
        later = self.launchctl_calls()[calls:]
        self.assertEqual(sum(c.startswith("bootstrap") for c in later), 2, later)
        self.assertTrue(PLUGIN.record_path().exists())

    def test_a_failed_restore_is_reported_not_hidden(self):
        with FakeProxy() as proxy:
            self.first_install(proxy)
            code, out, _ = self.setup_with(
                proxy, find_binary=mock.Mock(return_value="/opt/new/context-guru-proxy"),
                wait_healthy=mock.Mock(return_value=False))
        self.assertEqual(code, 1)
        self.assertIn("rolled_back=false", out)
        self.assertIn("rollback_failed=service", out)

    # ---- 3: our own groups already in the file are never "the user's bytes"

    def test_stale_hooks_without_state_are_not_restored_by_uninstall(self):
        settings = self.home / ".bob/settings/settings.json"
        state = PLUGIN.hooks_state()
        HOOKS.install(settings, state, HOOKS.our_groups("/old/python3", "/old/bob_plugin.py"))
        shutil.rmtree(state)
        HOOKS.install(settings, state, HOOKS.our_groups("/usr/bin/python3", "/p/bob_plugin.py"))
        HOOKS.remove(settings, state)
        self.assertFalse(HOOKS.is_installed(settings))

    # ---- 6: unsafe paths behind a link, and a record naming a missing rc file

    def test_an_rc_target_with_a_quote_is_refused(self):
        folder = self.root / 'dot"files'
        folder.mkdir()
        (folder / "profile").write_bytes(b"x\n")
        self.rc.symlink_to(folder / "profile")
        with FakeProxy() as proxy:
            code, out, _ = self.setup_with(proxy)
        self.assertEqual(code, 2)
        self.assertIn("reason=unsafe_path", out)
        self.assertEqual((folder / "profile").read_bytes(), b"x\n")
        self.assertEqual(self.launchctl_calls(), [])

    def test_a_settings_target_with_a_backslash_is_refused(self):
        folder = self.root / "dot\\files"
        folder.mkdir()
        (folder / "settings.json").write_text("{}")
        settings = self.home / ".bob/settings/settings.json"
        settings.parent.mkdir(parents=True)
        settings.symlink_to(folder / "settings.json")
        with FakeProxy() as proxy:
            code, out, _ = self.setup_with(proxy)
        self.assertEqual(code, 2)
        self.assertIn("reason=unsafe_path", out)

    @unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
    def test_reset_says_left_alone_when_the_recorded_rc_file_is_missing(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy:
            self.assertEqual(self.setup_with(proxy)[0], 0)
        record = json.loads(PLUGIN.record_path().read_text())
        record["rc_file"] = str(self.root / "elsewhere/.zshrc")
        PLUGIN.write_record(record)
        done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"], text=True,
                              capture_output=True, timeout=30)
        self.assertIn("rc_block=removed", done.stdout)  # the scan still finds ours
        self.assertEqual(self.rc.read_bytes(), b"x\n")
        self.rc.write_bytes(b"x\n")
        PLUGIN.write_record(record)
        done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"], text=True,
                              capture_output=True, timeout=30)
        self.assertIn("rc_block=left_alone", done.stdout)
        self.assertIn("result=partly_removed", done.stdout)

    # ---- 4, 5: reset.sh keeps the mode, refuses duplicate keys, keeps state on a failure

    @unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
    def test_reset_keeps_the_settings_mode_on_both_paths(self):
        settings = self.home / ".bob/settings/settings.json"
        settings.parent.mkdir(parents=True)
        for edit in (False, True):
            with self.subTest(edit=edit):
                settings.write_text('{"a": 1}\n')
                settings.chmod(0o640)
                with FakeProxy() as proxy:
                    self.assertEqual(self.setup_with(proxy)[0], 0)
                if edit:
                    data = json.loads(settings.read_text())
                    data["late"] = 1
                    settings.write_text(json.dumps(data))
                done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"],
                                      text=True, capture_output=True, timeout=30)
                self.assertIn("hooks=removed", done.stdout)
                self.assertEqual(stat.S_IMODE(settings.stat().st_mode), 0o640)

    @unittest.skipUnless(shutil.which("osascript"), "reset.sh is macOS only")
    def test_reset_refuses_duplicate_keys_and_keeps_its_state(self):
        settings = self.home / ".bob/settings/settings.json"
        with FakeProxy() as proxy:
            self.assertEqual(self.setup_with(proxy)[0], 0)
        text = settings.read_text().replace("{", '{"dup": 1, "dup": 2, ', 1)
        settings.write_text(text)
        done = subprocess.run(["/bin/sh", str(PLUGIN.reset_path()), "--yes"], text=True,
                              capture_output=True, timeout=30)
        self.assertIn("hooks=duplicate_keys", done.stdout)
        self.assertIn("result=partly_removed", done.stdout)
        self.assertEqual(settings.read_text(), text)
        self.assertTrue((PLUGIN.hooks_state() / "record.json").exists())

    # ---- 7: small gaps

    def test_no_record_uninstall_scans_every_bash_file(self):
        (self.home / ".bash_profile").write_bytes(b"# empty\n")
        profile = self.home / ".profile"
        profile.write_bytes(b"p\n" + ROUTE.block(8891))
        code, out, _ = self.run_main("uninstall")
        self.assertIn("rc_block=removed", out)
        self.assertEqual(profile.read_bytes(), b"p\n")

    def test_no_record_uninstall_survives_an_unreadable_rc_file(self):
        self.rc.write_bytes(ROUTE.block(8891))
        with mock.patch.object(Path, "read_bytes", side_effect=PermissionError("denied")):
            code, out, _ = self.run_main("uninstall")
        self.assertIn("result=", out)

    def test_uninstall_after_a_shell_change_cleans_the_recorded_file(self):
        self.rc.write_bytes(b"x\n")
        with FakeProxy() as proxy:
            self.assertEqual(self.setup_with(proxy)[0], 0)
        os.environ["SHELL"] = "/bin/zsh"
        code, out, _ = self.run_main("uninstall")
        self.assertIn("rc_block=removed", out)
        self.assertEqual(self.rc.read_bytes(), b"x\n")

    def test_status_reports_an_incomplete_setup(self):
        with FakeProxy() as proxy:
            self.write_record(proxy.port, rc_file=str(self.rc), complete=False)
            ROUTE.install(self.rc, proxy.port)
            code, out, _ = self.run_main("status")
        self.assertEqual(code, 1)
        self.assertIn("result=setup_incomplete", out)

    def test_ensure_fits_the_hook_timeout_when_launchctl_hangs(self):
        import time
        self.write_record(dead_port())
        PLUGIN.launchd_plist().parent.mkdir(parents=True)
        with open(PLUGIN.launchd_plist(), "wb") as handle:
            plistlib.dump({"Label": PLUGIN.LABEL, "ContextGuruManaged": True}, handle)
        os.environ["CG_TEST_SLOW_KICKSTART"] = "30"
        start = time.monotonic()
        self.assertEqual(self.run_main("ensure"), (0, "", ""))
        self.assertLess(time.monotonic() - start, 4.5)

    def test_bootstrap_waits_until_launchd_lets_the_label_go(self):
        loaded = self.root / "loaded"
        loaded.write_text("3")
        os.environ["CG_TEST_PRINT_LOADED"] = str(loaded)
        with FakeProxy() as proxy:
            code, out, _ = self.setup_with(proxy)
        self.assertEqual(code, 0, out)
        calls = self.launchctl_calls()
        first_bootstrap = next(i for i, c in enumerate(calls) if c.startswith("bootstrap"))
        self.assertEqual([c.split()[0] for c in calls[:first_bootstrap]],
                         ["bootout", "print", "print", "print", "print"])


# A fake bobide. It logs its arguments and keeps the installed extension ids in a file.
# CG_TEST_BOBIDE_FAIL makes every call exit 1.
FAKE_BOBIDE = """#!/bin/sh
printf '%s\\n' "$*" >> "$CG_TEST_BOBIDE_LOG"
if [ -n "${CG_TEST_BOBIDE_FAIL:-}" ]; then echo "bobide: failed" >&2; exit 1; fi
case "$1" in
  --install-extension) echo rossoctl.context-guru-bob >> "$CG_TEST_BOBIDE_DB" ;;
  --uninstall-extension) grep -v "^$2$" "$CG_TEST_BOBIDE_DB" > "$CG_TEST_BOBIDE_DB.new";
                         mv "$CG_TEST_BOBIDE_DB.new" "$CG_TEST_BOBIDE_DB" ;;
  --list-extensions) cat "$CG_TEST_BOBIDE_DB" ;;
esac
exit 0
"""


class StatusbarTest(TempHome):
    """setup installs the status bar extension with bobide, uninstall removes it."""

    def setUp(self):
        super().setUp()
        tools = self.root.parent / f"{self.root.name}.tools"  # outside the tree doctor checks
        tools.mkdir()
        self.addCleanup(shutil.rmtree, tools, ignore_errors=True)
        self.bobide = tools / "bobide"
        self.bobide.write_text(FAKE_BOBIDE)
        self.bobide.chmod(0o755)
        self.bobide_log = tools / "bobide.log"
        self.bobide_db = tools / "extensions"
        self.bobide_db.write_text("")
        self.vsix = tools / "context-guru-bob.vsix"
        self.vsix.write_bytes(b"PK fake vsix")
        os.environ.update({"CONTEXT_GURU_BOBIDE": str(self.bobide),
                           "CG_TEST_BOBIDE_LOG": str(self.bobide_log),
                           "CG_TEST_BOBIDE_DB": str(self.bobide_db)})
        patcher = mock.patch.object(PLUGIN, "statusbar_vsix", return_value=self.vsix)
        patcher.start()
        self.addCleanup(patcher.stop)

    def bobide_calls(self):
        return self.bobide_log.read_text().splitlines() if self.bobide_log.exists() else []

    def setup_run(self, proxy):
        with mock.patch.object(PLUGIN, "find_binary", return_value="/opt/cg/context-guru-proxy"), \
             mock.patch.object(PLUGIN, "first_free_port", return_value=proxy.port):
            return self.run_main("setup", "--i-consent-to-traffic-interception")

    def test_setup_installs_the_vsix_and_records_it(self):
        with FakeProxy() as proxy:
            code, out, _ = self.setup_run(proxy)
        self.assertEqual(code, 0, out)
        self.assertIn("statusbar=installed", out)
        self.assertEqual(self.bobide_calls(), [f"--install-extension {self.vsix}"])
        self.assertIs(PLUGIN.read_record()["statusbar"], True)

    def test_setup_without_the_vsix_skips_and_still_installs(self):
        PLUGIN.statusbar_vsix.return_value = self.root / "missing.vsix"
        with FakeProxy() as proxy:
            code, out, _ = self.setup_run(proxy)
        self.assertEqual(code, 0, out)
        self.assertIn("statusbar=skipped\nstatusbar_reason=no_vsix", out)
        self.assertIn("result=installed", out)
        self.assertEqual(self.bobide_calls(), [])
        self.assertIs(PLUGIN.read_record()["statusbar"], False)

    def test_setup_without_bobide_skips_and_still_installs(self):
        os.environ["CONTEXT_GURU_BOBIDE"] = str(self.root / "no-bobide")
        with mock.patch.object(PLUGIN, "BOBIDE_PATHS", (self.root / "a/bobide",)), \
             FakeProxy() as proxy:
            code, out, _ = self.setup_run(proxy)
        self.assertEqual(code, 0, out)
        self.assertIn("statusbar=skipped\nstatusbar_reason=no_bobide", out)
        self.assertIn("result=installed", out)
        self.assertIs(PLUGIN.read_record()["statusbar"], False)

    def test_a_failing_bobide_does_not_fail_setup(self):
        os.environ["CG_TEST_BOBIDE_FAIL"] = "1"
        with FakeProxy() as proxy:
            code, out, _ = self.setup_run(proxy)
        self.assertEqual(code, 0, out)
        self.assertIn("statusbar=skipped\nstatusbar_reason=install_failed", out)
        self.assertIn("result=installed", out)
        self.assertIs(PLUGIN.read_record()["statusbar"], False)

    def test_uninstall_removes_the_extension_when_the_record_says_so(self):
        with FakeProxy() as proxy:
            self.setup_run(proxy)
            code, out, _ = self.run_main("uninstall")
        self.assertEqual(code, 0, out)
        self.assertIn("statusbar=removed", out)
        self.assertEqual(self.bobide_calls()[-1],
                         "--uninstall-extension rossoctl.context-guru-bob")
        self.assertEqual(self.bobide_db.read_text(), "")
        self.assertIn("result=removed", out)

    def test_uninstall_leaves_the_extension_alone_when_setup_did_not_install_it(self):
        self.write_record(dead_port(), statusbar=False)
        code, out, _ = self.run_main("uninstall")
        self.assertNotIn("statusbar=", out)
        self.assertEqual(self.bobide_calls(), [])

    def test_a_failing_uninstall_says_left_alone_and_goes_on(self):
        with FakeProxy() as proxy:
            self.setup_run(proxy)
            os.environ["CG_TEST_BOBIDE_FAIL"] = "1"
            code, out, _ = self.run_main("uninstall")
        self.assertIn("statusbar=left_alone", out)
        self.assertIn("service=stopped", out)
        self.assertIn("hooks=restored", out)
        self.assertIn("rc_block=removed", out)
        self.assertIn("result=removed", out)

    def test_doctor_says_ok_when_the_extension_is_listed(self):
        self.write_record(dead_port(), statusbar=True)
        self.bobide_db.write_text("other.ext\nrossoctl.context-guru-bob\n")
        before = self.tree()
        _, out, _ = self.run_main("doctor")
        self.assertIn("statusbar=ok\n", out)
        self.assertEqual(self.bobide_calls(), ["--list-extensions"])
        self.assertEqual(self.tree(), before)

    def test_doctor_warns_when_the_extension_is_missing(self):
        self.write_record(dead_port(), statusbar=True)
        _, out, _ = self.run_main("doctor")
        self.assertIn("statusbar=warn\nfix=", out)
        fix = out.split("statusbar=warn\nfix=", 1)[1].splitlines()[0]
        # An installed plugin has no bob-plugin/statusbar folder: name the repo's make target.
        self.assertIn("make bob-statusbar in the context-guru repo, then run setup again", fix)
        self.assertNotIn("bob-plugin/statusbar", fix)

    def test_doctor_warns_without_bobide(self):
        self.write_record(dead_port(), statusbar=True)
        os.environ["CONTEXT_GURU_BOBIDE"] = str(self.root / "no-bobide")
        with mock.patch.object(PLUGIN, "BOBIDE_PATHS", (self.root / "a/bobide",)):
            _, out, _ = self.run_main("doctor")
        self.assertIn("statusbar=warn\nfix=", out)

    def test_doctor_does_not_warn_when_setup_skipped_the_extension(self):
        self.write_record(dead_port(), statusbar=False)
        _, out, _ = self.run_main("doctor")
        self.assertIn("statusbar=not_installed\n", out)
        self.assertNotIn("statusbar=warn", out)
        self.assertEqual(self.bobide_calls(), [])


class ManifestTest(unittest.TestCase):
    def test_reset_script_parses_in_sh_and_bash(self):
        for shell in ("/bin/sh", "/bin/bash"):
            done = subprocess.run([shell, "-n", str(HERE / "reset.sh")], capture_output=True,
                                  text=True)
            self.assertEqual((done.returncode, done.stderr), (0, ""), shell)

    def test_plugin_json_has_the_bob_fields(self):
        data = json.loads((PLUGIN_ROOT / "plugin.json").read_text())
        self.assertEqual(data["id"], "context-guru")
        self.assertEqual(data["name"], "context-guru")
        for key in ("displayName", "description", "version", "category", "author", "homepage",
                    "keywords"):
            self.assertTrue(data[key], key)
        self.assertIs(data["disabled"], False)
        self.assertNotIn("hooks", data)  # a Bob plugin cannot carry hooks

    def test_skills_are_prefixed_and_call_the_controller_by_absolute_path(self):
        names = {}
        for skill in sorted((PLUGIN_ROOT / "skills").glob("*/SKILL.md")):
            text = skill.read_text()
            self.assertTrue(text.startswith("---\nname: "), skill)
            name = text.split("\n", 2)[1].removeprefix("name: ")
            names[skill.parent.name] = name
            self.assertRegex(name, r"^context-guru-[a-z0-9-]{1,51}$")
            self.assertIn("\ndescription: ", text.split("---", 2)[1])
            self.assertIn('python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py"', text)
            self.assertNotIn("CLAUDE_PLUGIN_ROOT", text)
        self.assertEqual(names, {folder: f"context-guru-{folder}" for folder in (
            "setup", "status", "uninstall", "preset-picker", "cache-strategy-picker", "update",
            "doctor")})

    def test_setup_skill_asks_consent_and_names_the_login(self):
        text = (PLUGIN_ROOT / "skills/setup/SKILL.md").read_text()
        self.assertIn("setup --plan", text)
        self.assertIn("--i-consent-to-traffic-interception", text)
        self.assertIn("log in", text)


if __name__ == "__main__":
    unittest.main()
