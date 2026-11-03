"""Behavioral ownership checks for disposable suite-process cleanup."""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock
import uuid


HELPER = Path(__file__).with_name("processes.py")
SPEC = importlib.util.spec_from_file_location("epco_owned_processes", HELPER)
processes = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = processes
SPEC.loader.exec_module(processes)

WORKER = """import signal
import time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
print('ready', flush=True)
time.sleep(300)
"""

ANCESTOR = """import subprocess
import sys
result = subprocess.run(
    [sys.executable, sys.argv[1], '--run-id', sys.argv[2], '--report', sys.argv[3]],
    capture_output=True, text=True, timeout=8,
)
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
sys.exit(result.returncode)
"""


class ProcessOwnershipTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="epco-process-ownership-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.run_id = "epco-process-check-" + uuid.uuid4().hex
        self.marker = "EPCO_TEST_RUN_ID=" + self.run_id

    def child(self, additions=None, arguments=()):
        environment = dict(os.environ)
        environment.pop("EPCO_TEST_RUN_ID", None)
        environment.update(additions or {})
        process = subprocess.Popen(
            [sys.executable, "-c", WORKER, *arguments], env=environment,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, start_new_session=True,
        )

        def cleanup():
            if process.poll() is None:
                process.kill()
            process.wait(timeout=3)
            process.stdout.close()

        self.addCleanup(cleanup)
        self.assertEqual(process.stdout.readline().strip(), "ready", "owned test worker did not initialize")
        return process

    def test_exact_environment_ownership_across_groups_and_ancestor_exclusion(self):
        owned = self.child({"EPCO_TEST_RUN_ID": self.run_id})
        argv_only = self.child(arguments=(self.marker,))
        other_value = self.child({"UNRELATED_VALUE": self.marker})
        prefix_only = self.child({"EPCO_TEST_RUN_ID": self.run_id + "-another-run"})
        unrelated = self.child({"EPCO_TEST_RUN_ID": "unrelated-" + uuid.uuid4().hex})
        report = self.directory / "cleanup.json"
        # Both the helper and its immediate ancestor carry the exact marker.
        # Only the separate worker belongs to the cleanup target set.
        environment = {**os.environ, "EPCO_TEST_RUN_ID": self.run_id}
        started = time.monotonic()
        result = subprocess.run(
            [sys.executable, "-c", ANCESTOR, str(HELPER), self.run_id, str(report)],
            env=environment, capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, "cleanup must preserve itself and its marked ancestor")
        self.assertLess(time.monotonic() - started, 6, "TERM/KILL cleanup must stay bounded")
        owned.wait(timeout=3)
        for survivor in (argv_only, other_value, prefix_only, unrelated):
            self.assertIsNone(survivor.poll(), "cleanup signaled an unowned process")
        evidence = json.loads(report.read_text())
        self.assertEqual({entry["pid"] for entry in evidence["before"]}, {owned.pid})
        self.assertEqual({entry["pid"] for entry in evidence["signals"]}, {owned.pid})
        self.assertIn(int(signal.SIGKILL), {entry["signal"] for entry in evidence["signals"]},
                      "an owned TERM-ignoring worker must reach bounded KILL escalation")
        self.assertEqual(evidence["remaining"], [])
        self.assertEqual(evidence["errors"], [])
        self.assertEqual(stat.S_IMODE(report.stat().st_mode), 0o600)
        self.assertNotIn(self.marker, report.read_text(), "reports must contain process metadata only")

    def test_pid_reuse_before_environment_read_cannot_authorize_a_signal(self):
        original = processes.Process(987654, 987653, "original-birth", "S")
        replacement = original._replace(birth="replacement-birth")
        with mock.patch.object(processes, "snapshot", return_value={original.pid: replacement}), \
                mock.patch.object(processes, "environment") as environment, \
                mock.patch.object(processes.os, "kill") as kill:
            self.assertFalse(processes.signal_owned(original, self.run_id, signal.SIGTERM, set()))
            environment.assert_not_called()
            kill.assert_not_called()

    def test_pid_reuse_after_environment_read_cannot_authorize_a_signal(self):
        original = processes.Process(987654, 987653, "original-birth", "S")
        replacement = original._replace(birth="replacement-birth")
        with mock.patch.object(processes, "snapshot", side_effect=[
                {original.pid: original}, {original.pid: replacement}]), \
                mock.patch.object(processes, "environment", return_value=[os.fsencode(self.marker)]), \
                mock.patch.object(processes.os, "kill") as kill:
            self.assertFalse(processes.signal_owned(original, self.run_id, signal.SIGKILL, set()))
            kill.assert_not_called()

    def test_unreadable_environment_never_authorizes_a_signal(self):
        original = processes.Process(987654, 987653, "original-birth", "S")
        with mock.patch.object(processes, "snapshot", return_value={original.pid: original}), \
                mock.patch.object(processes, "environment", return_value=None), \
                mock.patch.object(processes.os, "kill") as kill:
            with self.assertRaises(RuntimeError):
                processes.signal_owned(original, self.run_id, signal.SIGTERM, set())
            kill.assert_not_called()

    def test_self_and_all_known_ancestors_are_excluded_before_inspection(self):
        identities = {
            40: processes.Process(40, 1, "grandparent", "S"),
            41: processes.Process(41, 40, "parent", "S"),
            42: processes.Process(42, 41, "self", "S"),
        }
        with mock.patch.object(processes.os, "getpid", return_value=42), \
                mock.patch.object(processes.os, "getppid", return_value=41):
            excluded = processes.ancestors(identities)
        self.assertEqual(excluded, {0, 1, 40, 41, 42})
        with mock.patch.object(processes, "snapshot") as snapshot, \
                mock.patch.object(processes.os, "kill") as kill:
            for identity in identities.values():
                self.assertFalse(processes.signal_owned(identity, self.run_id, signal.SIGTERM, excluded))
            snapshot.assert_not_called()
            kill.assert_not_called()

    def test_unavailable_os_inspection_fails_promptly_without_signaling(self):
        report = self.directory / "unavailable.json"
        for failure in (RuntimeError("synthetic private inspection detail"),
                        subprocess.TimeoutExpired("synthetic inspection", 2)):
            with self.subTest(error_type=type(failure).__name__):
                stderr = io.StringIO()
                started = time.monotonic()
                with mock.patch.object(processes, "snapshot", side_effect=failure), \
                        mock.patch.object(processes.os, "kill") as kill, \
                        mock.patch.object(sys, "argv", [str(HELPER), "--run-id", self.run_id,
                                                       "--report", str(report)]), \
                        contextlib.redirect_stderr(stderr):
                    self.assertEqual(processes.main(), 1)
                    kill.assert_not_called()
                self.assertLess(time.monotonic() - started, 1)
                self.assertNotIn("synthetic private", stderr.getvalue())
                self.assertNotIn(self.marker, stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
