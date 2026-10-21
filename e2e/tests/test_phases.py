import pathlib
import re
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


class TestPhaseOrder(unittest.TestCase):
    def test_full_tier_runs_preflight_before_the_suite(self):
        # The suite is ten minutes of `make test`; a skewed clock or a busy port must stop the run before it.
        order = tier_order("full")
        self.assertEqual(order[:2], ["preflight", "suite"], order)

    def test_full_tier_runs_every_smoke_phase(self):
        self.assertEqual([p for p in tier_order("full") if p != "suite"], tier_order("smoke"))


if __name__ == "__main__":
    unittest.main()
