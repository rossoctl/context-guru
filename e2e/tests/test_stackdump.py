import signal
import subprocess
import sys
import time
import unittest


class TestStackDumpOnSigusr1(unittest.TestCase):
    """A hung phase's evidence: `kill -USR1` makes the harness's Python processes print every thread's stack."""

    def _dumps(self, args):
        p = subprocess.Popen([sys.executable, "-m", *args], stderr=subprocess.PIPE, stdout=subprocess.DEVNULL, text=True)
        try:
            time.sleep(1.5)
            self.assertIsNone(p.poll(), "process must still be running to be dumped")
            p.send_signal(signal.SIGUSR1)
            time.sleep(0.5)
            self.assertIsNone(p.poll(), "SIGUSR1 must dump, not kill")
        finally:
            p.kill()
            err = p.communicate(timeout=10)[1]
        return err

    def test_upstream_dumps_its_threads(self):
        err = self._dumps(["e2e.upstream", "--port", "0"])
        self.assertIn("serve_forever", err)

    def test_replay_dumps_its_stack(self):
        # 10.255.255.1 is unroutable: the connect blocks, so the process is alive and inside the send loop.
        err = self._dumps(["e2e.replay", "--tier", "smoke", "--home", "/tmp/cg-e2e-dump", "--base", "http://10.255.255.1:9"])
        self.assertIn("in run", err)


if __name__ == "__main__":
    unittest.main()
