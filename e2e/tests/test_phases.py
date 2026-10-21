import pathlib
import re
import subprocess
import unittest

PHASES = pathlib.Path(__file__).resolve().parents[1] / "phases.sh"


def tier_order(tier: str) -> list[str]:
    """The phase names a tier's case arm runs, in order, read from phases.sh (it cannot run outside the container)."""
    text = PHASES.read_text()
    arm = re.search(rf"^\s*{tier}\)(.*?);;", text, re.S | re.M).group(1)
    out = []
    for m in re.finditer(r"for ph in ([^;]+);|phase (\w+) p_\w+", arm):
        out += m.group(1).split() if m.group(1) else [m.group(2)]
    return out


def fn(name: str) -> str:
    """One one-line function's text out of phases.sh."""
    return next(l for l in PHASES.read_text().splitlines() if l.startswith(f"{name}() {{"))


class TestAssertPhase(unittest.TestCase):
    """check.py exits 0 (PASS/INCOMPLETE) or 1 (FAIL): both are results. Anything else is a crashed judge."""

    def _rc(self, judge_rc):
        script = f"python3() {{ return {judge_rc}; }}\nR=/x; TIER=smoke\n{fn('p_assert')}\np_assert"
        return subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=10).returncode

    def test_pass_and_fail_are_results(self):
        self.assertEqual((self._rc(0), self._rc(1)), (0, 0))

    def test_a_judge_traceback_crashes_the_phase(self):
        self.assertEqual((self._rc(3), self._rc(2)), (3, 2))


class TestPhaseOrder(unittest.TestCase):
    def test_full_tier_runs_preflight_before_the_suite(self):
        # The suite is ten minutes of `make test`; a skewed clock or a busy port must stop the run before it.
        order = tier_order("full")
        self.assertEqual(order[:2], ["preflight", "suite"], order)

    def test_full_tier_runs_every_smoke_phase(self):
        self.assertEqual([p for p in tier_order("full") if p != "suite"], tier_order("smoke"))


if __name__ == "__main__":
    unittest.main()
