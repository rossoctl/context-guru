import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

RUN = pathlib.Path(__file__).resolve().parents[1] / "run.sh"


def heredoc(opener: str) -> str:
    """The Python heredoc that follows `opener` in run.sh (run.sh itself needs docker)."""
    text = RUN.read_text()
    body = text[text.index(opener) + len(opener):]
    return body[body.index("\n") + 1:body.index("\nPY\n")]


class TestTimings(unittest.TestCase):
    def test_stress_endpoint_seconds_are_reported_in_milliseconds(self):
        with tempfile.TemporaryDirectory() as full, tempfile.TemporaryDirectory() as stress:
            (pathlib.Path(full) / "phases.jsonl").write_text("")
            (pathlib.Path(stress) / "phases.jsonl").write_text('{"phase":"import","status":"ok","seconds":7}\n')
            (pathlib.Path(stress) / "stress-endpoints.jsonl").write_text(
                '{"endpoint":"/api/history","s":0.000572}\n{"endpoint":"/api/facets?measured=1","s":1.064748}\n')
            subprocess.run([sys.executable, "-", full, stress], input=heredoc('python3 - "$FULL" "$STRESS" <<'),
                           text=True, check=True)
            st = json.loads((pathlib.Path(full) / "timings.json").read_text())["stress"]
        self.assertEqual((st["import_s"], st["history_api_ms"], st["facets_ms"], st["insights_pooled_ms"]),
                         (7, 0.6, 1064.7, None))


if __name__ == "__main__":
    unittest.main()
