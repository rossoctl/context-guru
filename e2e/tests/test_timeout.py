import pathlib
import subprocess
import tempfile
import time
import unittest

LIB = pathlib.Path(__file__).resolve().parents[1] / "timeout.sh"


def _bash(script, cwd):
    return subprocess.run(["bash", "-c", f"source {LIB}; {script}"], cwd=cwd, capture_output=True, text=True,
                          timeout=60)


class TestPhaseBudget(unittest.TestCase):
    def test_budgets_scale_with_tier(self):
        out = _bash('for t in smoke full stress; do printf "%s " "$(phase_budget $t replay)"; done', ".").stdout
        self.assertEqual(out.split(), ["600", "1800", "3600"])

    def test_suite_and_ui_get_the_full_budget_on_full(self):
        out = _bash('phase_budget full suite; phase_budget full ui; phase_budget smoke ui', ".").stdout
        self.assertEqual(out.split(), ["1800", "1800", "600"])

    def test_a_per_phase_env_override_wins_for_that_phase_only(self):
        r = subprocess.run(["bash", "-c", f"source {LIB}; phase_budget stress replay; phase_budget stress import"],
                           env={"PATH": "/usr/bin:/bin", "E2E_TIMEOUT_replay": "7"}, capture_output=True, text=True)
        self.assertEqual(r.stdout.split(), ["7", "3600"])


class TestRunBounded(unittest.TestCase):
    def test_returns_the_functions_exit_code_when_it_finishes(self):
        with tempfile.TemporaryDirectory() as d:
            r = _bash(f'f() {{ echo hi; return 4; }}; run_bounded 10 f {d}/log true; echo "rc=$?"', d)
            self.assertIn("rc=4", r.stdout)
            self.assertEqual(pathlib.Path(d, "log").read_text(), "hi\n")

    def test_a_hang_is_killed_after_the_budget_and_returns_124(self):
        with tempfile.TemporaryDirectory() as d:
            t0 = time.monotonic()
            r = _bash(f'f() {{ sleep 300; }}; run_bounded 2 f {d}/log "echo captured >{d}/ev"; echo "rc=$?"', d)
            self.assertIn("rc=124", r.stdout)
            self.assertLess(time.monotonic() - t0, 20)

    def test_evidence_is_captured_before_the_kill(self):
        # The capture hook sees the hung child still alive: evidence first, kill second.
        with tempfile.TemporaryDirectory() as d:
            _bash(f'f() {{ sleep 300; }}; run_bounded 2 f {d}/log '
                  f'"pgrep -f \\"sleep 300\\" >/dev/null && echo alive >{d}/ev"', d)
            self.assertEqual(pathlib.Path(d, "ev").read_text().strip(), "alive")

    def test_grandchildren_of_a_hung_phase_are_killed(self):
        with tempfile.TemporaryDirectory() as d:
            _bash(f'f() {{ bash -c "sleep 301"; }}; run_bounded 2 f {d}/log true', d)
            time.sleep(0.5)
            alive = subprocess.run(["pgrep", "-f", "sleep 301"], capture_output=True, text=True).stdout
            self.assertEqual(alive, "")


class TestClockGuard(unittest.TestCase):
    def _check(self, host_epoch):
        return subprocess.run(["bash", "-c", f"source {LIB}; check_clock {host_epoch} 120; echo \"rc=$?\""],
                              capture_output=True, text=True, timeout=10).stdout

    def test_a_clock_within_the_limit_passes_silently(self):
        self.assertEqual(self._check(int(time.time()) - 5).strip(), "rc=0")

    def test_a_clock_behind_the_host_fails_with_the_skew(self):
        out = self._check(int(time.time()) + 64957)
        self.assertIn("rc=1", out)
        self.assertRegex(out, r"clock skew -6495[6-8]s")

    def test_a_clock_ahead_of_the_host_fails_too(self):
        self.assertRegex(self._check(int(time.time()) - 600), r"clock skew 60[01]s\nrc=1")

    def test_a_missing_host_epoch_is_not_judged(self):
        self.assertEqual(self._check('""').strip(), "rc=0")


if __name__ == "__main__":
    unittest.main()
