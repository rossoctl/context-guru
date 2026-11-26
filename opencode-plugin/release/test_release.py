"""install-opencode.sh piped into bash the way `curl -fsSL …/install-opencode.sh | bash` runs it, against a local
release folder that package.sh builds, as the release workflow does. The installer itself is replaced by a
recorder (CG_OC_PYTHON), so this tests only the bootstrap: download, SHA-256, unpack, hand over."""
import functools
import hashlib
import http.server
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import threading
import unittest

HERE = Path(__file__).resolve().parent
BOOTSTRAP = HERE / "install-opencode.sh"
PACKAGER = HERE / "package.sh"
PACKAGE = HERE.parent

RECORDER = """#!/bin/sh
# Records what the bootstrap hands to the installer: the arguments, and the first line of stdin.
read -r answer || answer=""
printf '%s\\n' "$@" > "$CG_TEST_OUT/argv"
printf '%s' "$answer" > "$CG_TEST_OUT/stdin"
exit "${CG_TEST_EXIT:-0}"
"""


class QuietFiles(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


class Release(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.release = self.root / "release"
        built = subprocess.run(["/bin/sh", str(PACKAGER), "v9.9.9-test", str(self.release)],
                               capture_output=True, text=True, timeout=60)
        self.assertEqual(built.returncode, 0, built.stderr)
        self.tarball = self.release / "context-guru-opencode-v9.9.9-test.tar.gz"
        handler = functools.partial(QuietFiles, directory=str(self.release))
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_address[1]}"
        self.out = self.root / "out"
        self.out.mkdir()
        self.recorder = self.root / "recorder.sh"
        self.recorder.write_text(RECORDER)
        self.recorder.chmod(0o755)
        self.tty = self.root / "tty"

    def write_sums(self, line):
        (self.release / "context-guru-opencode-SHA256SUMS").write_text(line + "\n")

    def pipe(self, answers="y\n", *args, exit_code=0):
        self.tty.write_text(answers)
        env = dict(os.environ, CG_OC_RELEASE_URL=self.url, CG_OC_TTY=str(self.tty),
                   CG_OC_PYTHON=str(self.recorder), CG_TEST_OUT=str(self.out), CG_TEST_EXIT=str(exit_code))
        argv = ["/bin/bash", "-s", *args] if args else ["/bin/bash"]
        return subprocess.run(argv, input=BOOTSTRAP.read_text(), text=True, capture_output=True, env=env,
                              cwd=str(self.root), timeout=120)

    def handed(self):
        return (self.out / "argv").read_text().splitlines() if (self.out / "argv").exists() else None


class Packager(Release):
    def test_the_tarball_has_the_plugin_and_the_installer_and_no_tests(self):
        with tarfile.open(self.tarball) as archive:
            names = archive.getnames()
        for wanted in ("opencode-plugin/index.js", "opencode-plugin/tui.js", "opencode-plugin/README.md",
                       "opencode-plugin/scripts/install_opencode.py"):
            self.assertIn(wanted, names)
        for name in names:
            self.assertNotIn("test", Path(name).name, name)
            self.assertFalse(name.startswith(("opencode-plugin/release", "opencode-plugin/test")), name)
            self.assertNotIn("__pycache__", name)

    def test_the_sums_name_the_tarball_with_its_digest(self):
        digest = hashlib.sha256(self.tarball.read_bytes()).hexdigest()
        self.assertEqual((self.release / "context-guru-opencode-SHA256SUMS").read_text(),
                         f"{digest}  {self.tarball.name}\n")


class Bootstrap(Release):
    def test_install_hands_the_unpacked_package_to_the_installer_with_the_terminal_answers(self):
        done = self.pipe("y\n")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("✓ download matches SHA-256", done.stdout)
        argv = self.handed()
        self.assertEqual(argv[0].rsplit("/", 2)[-2:], ["scripts", "install_opencode.py"])
        self.assertEqual(argv[1:3], ["install", "--source"])
        self.assertTrue(argv[3].endswith("/opencode-plugin"))
        self.assertEqual(argv[4], "--downloaded")
        self.assertEqual((self.out / "stdin").read_text(), "y")

    def test_uninstall_is_handed_over_too(self):
        done = self.pipe("y\n", "uninstall")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.handed()[1], "uninstall")

    def test_the_installers_exit_code_is_the_exit_code(self):
        self.assertEqual(self.pipe("y\n", exit_code=3).returncode, 3)

    def test_an_unknown_action_is_refused(self):
        done = self.pipe("y\n", "upgrade")
        self.assertEqual(done.returncode, 2)
        self.assertIsNone(self.handed())

    def test_a_wrong_digest_runs_nothing(self):
        self.write_sums(f"{'0' * 64}  {self.tarball.name}")
        done = self.pipe()
        self.assertEqual(done.returncode, 1)
        self.assertIn("does not match its SHA-256", done.stderr)
        self.assertIsNone(self.handed())

    def test_a_sums_file_with_an_unsafe_name_runs_nothing(self):
        # The server has a real file behind each bad name (curl folds `../`), so only the bootstrap's own name
        # check can stop it, not a failed download.
        digest = hashlib.sha256(self.tarball.read_bytes()).hexdigest()
        for copy in ("context-guru-opencode-x.tar.gz", "evil.tar.gz"):
            (self.release / copy).write_bytes(self.tarball.read_bytes())
        for name in ("../context-guru-opencode-x.tar.gz", "context-guru-opencode-x/../../evil.tar.gz", "evil.tar.gz"):
            self.write_sums(f"{digest}  {name}")
            done = self.pipe()
            self.assertEqual(done.returncode, 1, name)
            self.assertRegex(done.stderr, "unsafe file|does not name a context-guru-opencode tarball", name)
            self.assertIsNone(self.handed(), name)

    def test_no_terminal_runs_nothing(self):
        env = dict(os.environ, CG_OC_RELEASE_URL=self.url, CG_OC_TTY=str(self.root / "no-such-tty"),
                   CG_OC_PYTHON=str(self.recorder), CG_TEST_OUT=str(self.out))
        done = subprocess.run(["/bin/bash"], input=BOOTSTRAP.read_text(), text=True, capture_output=True,
                              env=env, timeout=60)
        self.assertEqual(done.returncode, 1)
        self.assertIn("no terminal", done.stderr)

    def test_dev_tty_without_a_controlling_terminal_runs_nothing(self):
        # /dev/tty is readable by its mode even with no terminal; the bootstrap must open it to know.
        env = dict(os.environ, CG_OC_RELEASE_URL=self.url, CG_OC_TTY="/dev/tty", CG_OC_PYTHON=str(self.recorder),
                   CG_TEST_OUT=str(self.out))
        done = subprocess.run(["/bin/bash"], input=BOOTSTRAP.read_text(), text=True, capture_output=True, env=env,
                              timeout=60, start_new_session=True)  # a new session has no controlling terminal
        self.assertEqual(done.returncode, 1)
        self.assertIn("no terminal", done.stderr)
        self.assertIsNone(self.handed())

    def test_the_temp_folder_is_removed_afterwards(self):
        tmpdir = self.root / "tmp"
        tmpdir.mkdir()
        self.tty.write_text("y\n")
        env = dict(os.environ, CG_OC_RELEASE_URL=self.url, CG_OC_TTY=str(self.tty), CG_OC_PYTHON=str(self.recorder),
                   CG_TEST_OUT=str(self.out), TMPDIR=str(tmpdir))
        subprocess.run(["/bin/bash"], input=BOOTSTRAP.read_text(), text=True, capture_output=True, env=env, timeout=60)
        self.assertEqual(list(tmpdir.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
