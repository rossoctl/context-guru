import contextlib
import io
import json
import pathlib
import tempfile
import unittest

from e2e import mutation


def judge(red="", base_red="", crashed=0, base_crashed=0, expect="S7.skill_port"):
    return mutation.judge("m1", "dash/x.go", expect, "run", "base", red, base_red, crashed, base_crashed)


class TestJudge(unittest.TestCase):
    def test_caught_when_the_expected_check_turned_red(self):
        j = judge(red="S7.skill_port,H.x")
        self.assertEqual((j["caught"], j["expect_matched"], j["turned_red"]), (True, ["S7.skill_port"], ["S7.skill_port", "H.x"]))

    def test_a_prefix_expectation_matches_any_check_under_it(self):
        self.assertEqual(judge(red="C.money.total_is_sum./a", expect="C.money.")["caught"], True)

    def test_a_crashed_phase_is_recorded_and_not_caught(self):
        # A crash proves the break did something, not that the check aimed at it can see it.
        j = judge(red="", crashed=1)
        self.assertEqual((j["caught"], j["crashed_phase"]), (False, True))

    def test_a_crash_is_not_caught_even_if_the_check_is_red(self):
        self.assertEqual(judge(red="S7.skill_port", crashed=1)["caught"], False)

    def test_not_caught_when_the_expected_check_was_already_red_on_the_baseline(self):
        j = judge(red="S7.skill_port", base_red="S7.skill_port")
        self.assertEqual((j["caught"], j["expect_failed_in_baseline"]), (False, True))

    def test_not_caught_when_only_another_check_turned_red(self):
        self.assertEqual(judge(red="H.x")["caught"], False)


class TestSummarize(unittest.TestCase):
    def _run(self, items, n):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "mutation.json.items").write_text("".join(json.dumps(i) + "\n" for i in items))
            with contextlib.redirect_stdout(io.StringIO()) as out:
                rc = mutation.summarize(d, "base", n)
            return rc, out.getvalue(), json.loads((p / "mutation.json").read_text())

    def test_exits_zero_when_every_mutation_was_judged_and_caught(self):
        rc, out, doc = self._run([judge(red="S7.skill_port")] * 2, 2)
        self.assertEqual((rc, out, len(doc["mutations"])), (0, "missed: 0\n", 2))

    def test_exits_nonzero_when_a_mutation_was_not_judged(self):
        rc, out, _ = self._run([judge(red="S7.skill_port")], 2)
        self.assertEqual((rc != 0, out), (True, "judged 1 of 2 mutations\nmissed: 0\n"))

    def test_exits_nonzero_when_a_mutation_was_missed(self):
        rc, out, _ = self._run([judge(red="S7.skill_port"), judge(crashed=1)], 2)
        self.assertEqual((rc, out), (1, "missed: 1\n"))


if __name__ == "__main__":
    unittest.main()
