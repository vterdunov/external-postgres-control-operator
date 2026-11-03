"""Fault checks for the runner without requiring a Kubernetes control plane."""

import json
import os
from pathlib import Path
import signal
import shutil
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
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
directory = Path(os.environ['RUNNER_STATE'])
scratch = Path(tempfile.mkdtemp(prefix='k8s_test_framework_'))
(scratch / 'fixture-state').write_text('owned synthetic fixture data')
(directory / 'scratch.json').write_text(json.dumps({'root': str(Path(__file__).resolve().parent), 'scratch': str(scratch)}))
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(300)'], start_new_session=True)
pending_pid = directory / f'descendant.{os.getpid()}.pending'
pending_pid.write_text(str(child.pid))
os.replace(pending_pid, directory / 'descendant.pid')
def interrupted(signum, frame):
    print('failure diagnostics', flush=True)
    print('cleanup completed', flush=True)
    sys.exit(1)
signal.signal(signal.SIGINT, interrupted)
(directory / 'ready').touch()
scenario = os.environ['RUNNER_SCENARIO']
if scenario in ('success', 'cleanup-failure'):
    print('suite passed', flush=True)
    sys.exit(0)
if scenario in ('failure', 'test-and-cleanup-failure'):
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
            "TMPDIR": str(self.directory),
        }

    def start(self, scenario):
        pid_path = self.directory / "descendant.pid"
        pid_path.unlink(missing_ok=True)
        state_path = self.directory / "scratch.json"
        state_path.unlink(missing_ok=True)
        descendant = None
        scratch = None
        runner = Path(__file__).with_name("run.py")
        if scenario in ("cleanup-failure", "test-and-cleanup-failure"):
            isolated = self.directory / "fault-runner/test/envtest"
            isolated.mkdir(parents=True, exist_ok=True)
            (isolated / "run.py").write_bytes(runner.read_bytes())
            (isolated / "processes_actual.py").write_bytes(Path(__file__).with_name("processes.py").read_bytes())
            (isolated / "processes.py").write_text(
                "from pathlib import Path\n"
                "from processes_actual import cleanup_owned as verified_cleanup\n"
                "def cleanup_owned(run_id, report_path):\n"
                "    if Path(report_path).name == 'suite-processes.json':\n"
                "        return False\n"
                "    return verified_cleanup(run_id, report_path)\n"
            )
            runner = isolated / "run.py"
        process = subprocess.Popen(
            [sys.executable, str(runner)],
            env={**self.environment, "RUNNER_SCENARIO": scenario},
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )

        def cleanup():
            try:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.communicate(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.communicate(timeout=3)
            finally:
                if descendant is not None:
                    try:
                        os.kill(descendant, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                if scratch is not None:
                    for value in (scratch["scratch"], scratch["root"]):
                        path = Path(value).resolve()
                        owned_base = self.directory.resolve()
                        if path != owned_base and path.is_relative_to(owned_base):
                            shutil.rmtree(path, ignore_errors=True)

        self.addCleanup(cleanup)
        # Capture each invocation's exact child PID before another signal subtest
        # reuses the state file, so even a failing RED run cannot leak its worker.
        deadline = time.monotonic() + 5
        while not pid_path.exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.01)
        if pid_path.exists():
            descendant = int(pid_path.read_text())
        if state_path.exists():
            scratch = json.loads(state_path.read_text())
        return process

    def assert_scratch_removed(self):
        state = json.loads((self.directory / "scratch.json").read_text())
        root, scratch = Path(state["root"]).resolve(), Path(state["scratch"]).resolve()
        self.assertTrue(scratch.is_relative_to(root), "default fixture scratch escaped the owned runner root")
        self.assertFalse(root.exists(), "verified cleanup must remove the owned runner root")
        self.assertFalse(scratch.exists(), "verified cleanup must remove nested default fixture scratch")
        self.assertTrue(self.artifacts.exists(), "owned scratch cleanup must retain external diagnostics")

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
        self.assert_scratch_removed()

    def test_failure_preserves_exit_code_and_diagnostics_and_removes_descendants(self):
        process = self.start("failure")
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 7, output)
        self.assertIn("failure diagnostics", (self.artifacts / "suite.log").read_text())
        self.assert_descendant_stopped()
        self.assert_scratch_removed()

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
                self.assert_scratch_removed()

    def test_unverified_process_cleanup_retains_owned_scratch_and_failure_status(self):
        for scenario, expected in (("cleanup-failure", 1), ("test-and-cleanup-failure", 7)):
            with self.subTest(scenario=scenario):
                process = self.start(scenario)
                output, _ = process.communicate(timeout=10)
                self.assertEqual(process.returncode, expected, output)
                state = json.loads((self.directory / "scratch.json").read_text())
                self.assertTrue(Path(state["root"]).is_dir(), "unverified cleanup must retain its exact owned root")
                self.assertTrue(Path(state["scratch"]).is_dir(), "unverified cleanup must retain recovery data")
                pid = int((self.directory / "descendant.pid").read_text())
                os.kill(pid, 0)


if __name__ == "__main__":
    unittest.main()
