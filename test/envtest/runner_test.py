"""Fault checks for the runner without requiring a Kubernetes control plane."""

import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest


FAKE_MISE = """#!/usr/bin/env python3
import os
from pathlib import Path
import shutil
import sys
if os.environ['RUNNER_SCENARIO'] == 'compile-failure':
    print('compile failure diagnostic', flush=True)
    sys.exit(9)
target = Path(sys.argv[sys.argv.index('-o') + 1])
shutil.copyfile(os.environ['RUNNER_BINARY'], target)
target.chmod(0o700)
"""

FAKE_SUITE = """#!/usr/bin/env python3
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
directory = Path(os.environ['RUNNER_STATE'])
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(300)'])
(directory / 'descendant.pid').write_text(str(child.pid))
def interrupted(signum, frame):
    print('failure diagnostics', flush=True)
    print('cleanup completed', flush=True)
    sys.exit(1)
signal.signal(signal.SIGINT, interrupted)
(directory / 'ready').touch()
scenario = os.environ['RUNNER_SCENARIO']
if scenario == 'success':
    print('suite passed', flush=True)
    sys.exit(0)
if scenario == 'failure':
    print('failure diagnostics', flush=True)
    sys.exit(7)
while True:
    time.sleep(0.05)
"""


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="epco-envtest-runner-check-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        mise = self.directory / "mise"
        mise.write_text(FAKE_MISE)
        mise.chmod(0o700)
        suite = self.directory / "suite.py"
        suite.write_text(FAKE_SUITE)
        self.artifacts = self.directory / "artifacts"
        self.environment = {
            **os.environ,
            "PATH": str(self.directory) + os.pathsep + os.environ["PATH"],
            "RUNNER_BINARY": str(suite),
            "RUNNER_STATE": str(self.directory),
            "EPCO_ARTIFACTS_DIR": str(self.artifacts),
            "EPCO_TEST_SEED": "42",
        }

    def start(self, scenario):
        process = subprocess.Popen(
            [sys.executable, str(Path(__file__).with_name("run.py"))],
            env={**self.environment, "RUNNER_SCENARIO": scenario},
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )

        def cleanup():
            if process.poll() is None:
                process.terminate()
                process.communicate(timeout=10)

        self.addCleanup(cleanup)
        return process

    def assert_descendant_stopped(self):
        pid = int((self.directory / "descendant.pid").read_text())
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            status = subprocess.run(
                ["ps", "-o", "stat=", "-p", str(pid)],
                capture_output=True, text=True, check=False,
            ).stdout.strip()
            # Linux containers may retain a dead orphan as a zombie until PID 1 reaps it.
            if not status or status.startswith("Z"):
                return
            time.sleep(0.05)
        self.fail(f"runner leaked descendant PID {pid}")

    def test_success_preserves_seed_and_removes_owned_descendants(self):
        process = self.start("success")
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0, output)
        self.assertEqual((self.artifacts / "seed.txt").read_text(), "42\n")
        self.assertIn("suite passed", (self.artifacts / "suite.log").read_text())
        self.assert_descendant_stopped()

    def test_failure_preserves_exit_code_and_diagnostics_and_removes_descendants(self):
        process = self.start("failure")
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 7, output)
        self.assertIn("failure diagnostics", (self.artifacts / "suite.log").read_text())
        self.assert_descendant_stopped()

    def test_compile_failure_preserves_exit_code_and_diagnostics(self):
        process = self.start("compile-failure")
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 9, output)
        self.assertIn("compile failure diagnostic", (self.artifacts / "compile.log").read_text())
        self.assertFalse((self.artifacts / "suite.log").exists())

    def test_interrupt_and_termination_complete_diagnostics_and_cleanup(self):
        for signum in (signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=signum):
                ready = self.directory / "ready"
                ready.unlink(missing_ok=True)
                process = self.start("interrupt")
                deadline = time.monotonic() + 5
                while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(ready.exists(), "fake suite did not become ready")
                process.send_signal(signum)
                output, _ = process.communicate(timeout=10)
                self.assertEqual(process.returncode, 128 + signum, output)
                log = (self.artifacts / "suite.log").read_text()
                self.assertLess(log.index("failure diagnostics"), log.index("cleanup completed"))
                self.assert_descendant_stopped()


if __name__ == "__main__":
    unittest.main()
