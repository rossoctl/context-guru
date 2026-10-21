import pathlib
import re
import sqlite3
import subprocess
import tempfile
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
    """One function's text out of phases.sh: a one-liner, or up to its closing brace."""
    lines = PHASES.read_text().splitlines()
    start = next(i for i, l in enumerate(lines) if l.startswith(f"{name}() {{"))
    if lines[start].rstrip().endswith("}"):
        return lines[start]
    return "\n".join(lines[start:next(i for i in range(start, len(lines)) if lines[i] == "}") + 1])


class TestAssertPhase(unittest.TestCase):
    """check.py exits 0 (PASS/INCOMPLETE) or 1 (FAIL): both are results. Anything else is a crashed judge."""

    def _rc(self, judge_rc):
        script = f"python3() {{ return {judge_rc}; }}\nR=/x; TIER=smoke\n{fn('p_assert')}\np_assert"
        return subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=10).returncode

    def test_pass_and_fail_are_results(self):
        self.assertEqual((self._rc(0), self._rc(1)), (0, 0))

    def test_a_judge_traceback_crashes_the_phase(self):
        self.assertEqual((self._rc(3), self._rc(2)), (3, 2))


class TestNoInterpolation(unittest.TestCase):
    def test_session_rows_binds_the_session_id(self):
        sid = "alpha-s01-x'; DROP TABLE tool_declarations; --"
        with tempfile.TemporaryDirectory() as d:
            db = f"{d}/x.db"
            c = sqlite3.connect(db)
            c.execute("CREATE TABLE tool_declarations (session_id TEXT, name TEXT)")
            c.execute("INSERT INTO tool_declarations VALUES (?, 'Bash')", (sid,))
            c.commit(); c.close()
            p = subprocess.run(["bash", "-c", f'DB="$1"\n{fn("session_rows")}\nsession_rows "$2"', "-", db, sid],
                               capture_output=True, text=True, timeout=10)
        self.assertEqual(p.stdout, "1\n")

    def test_no_inline_python_or_sql_interpolates_a_shell_variable(self):
        # Paths and ids go in as argv or bound parameters; a quote in one must not change the code that runs.
        e2e = PHASES.parent
        bad = [f"{f.name}:{i}" for f in (PHASES, e2e / "run.sh", e2e / "mutate.sh")
               for i, l in enumerate(f.read_text().splitlines(), 1)
               if re.search(r'python3 -c "[^"]*\$', l) or re.search(r"(WHERE|=)\s*'\$", l)]
        self.assertEqual(bad, [])


class TestPhaseOrder(unittest.TestCase):
    def test_full_tier_runs_preflight_before_the_suite(self):
        # The suite is ten minutes of `make test`; a skewed clock or a busy port must stop the run before it.
        order = tier_order("full")
        self.assertEqual(order[:2], ["preflight", "suite"], order)

    def test_full_tier_runs_every_smoke_phase(self):
        self.assertEqual([p for p in tier_order("full") if p != "suite"], tier_order("smoke"))


if __name__ == "__main__":
    unittest.main()
