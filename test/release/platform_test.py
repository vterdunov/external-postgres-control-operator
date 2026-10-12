"""Native manager startup must recognize real Go flag help without false positives."""

import copy
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SOURCE = "a" * 40
VERSION = "0.0.0-validation"
IMAGE = "epco/platform-validation:current"
GO_HELP = """Usage of /manager:
  -health-probe-bind-address string
        The address the probe endpoint binds to. (default ":8081")
  -leader-elect
        Enable leader election for controller manager.
"""


class PlatformStartupTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-native-help-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.state_path = self.directory / "docker.json"
        self.trace = self.directory / "commands.jsonl"
        self.architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
        self.inspected = {"Architecture": self.architecture, "Os": "linux", "Config": {
            "User": "65532:65532", "Labels": {
                "org.opencontainers.image.source": "https://github.com/vterdunov/external-postgres-control-operator",
                "org.opencontainers.image.revision": SOURCE,
                "org.opencontainers.image.version": VERSION,
            }}}
        self.state = {"inspected": self.inspected, "stdout": "", "stderr": GO_HELP, "exit_code": 0}
        docker = self.directory / "docker"
        docker.write_text(f"#!{sys.executable}\n" + """import json, os, sys
from pathlib import Path
state = json.loads(Path(os.environ['NATIVE_FIXTURE_STATE']).read_text())
with open(os.environ['NATIVE_FIXTURE_TRACE'], 'a') as output:
    output.write(json.dumps(sys.argv[1:]) + '\\n')
if sys.argv[1:3] == ['image', 'inspect']:
    print(json.dumps([state['inspected']]))
elif sys.argv[1] == 'run':
    print(state['stdout'], end='')
    print(state['stderr'], file=sys.stderr, end='')
    sys.exit(state['exit_code'])
else:
    sys.exit('Unexpected Docker operation')
""")
        docker.chmod(0o755)
        self.environment = {**os.environ, "PATH": str(self.directory) + os.pathsep + os.environ["PATH"],
                            "NATIVE_FIXTURE_STATE": str(self.state_path), "NATIVE_FIXTURE_TRACE": str(self.trace)}

    def check(self):
        self.state_path.write_text(json.dumps(self.state))
        return subprocess.run([sys.executable, str(ROOT / "tools/platform_check.py"), "--image", IMAGE,
                               "--version", VERSION, "--source", SOURCE], env=self.environment,
                              capture_output=True, text=True, timeout=15)

    def commands(self):
        return [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []

    def assert_rejected(self):
        result = self.check()
        self.assertNotEqual(result.returncode, 0, "Invalid native image/startup became successful delivery evidence")
        self.assertEqual(result.stdout, "", "Failure must not emit startup success evidence")

    def test_go_standard_single_dash_stderr_help_accepts_both_required_flags(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), {"architecture": self.architecture, "image": IMAGE,
                         "version": VERSION, "source_sha": SOURCE, "user": "65532:65532", "startup": "success"})
        self.assertEqual(self.commands(), [["image", "inspect", IMAGE], ["run", "--rm", "--network=none", IMAGE, "--help"]])

    def test_double_dash_stdout_help_remains_supported(self):
        self.state.update(stdout=GO_HELP.replace("  -", "  --"), stderr="")
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["startup"], "success")

    def test_each_required_manager_flag_must_be_present(self):
        for help_text in ("Usage of /manager:\n  --leader-elect\n",
                          "Usage of /manager:\n  --health-probe-bind-address string\n"):
            with self.subTest(help=help_text):
                self.state.update(stdout=help_text, stderr="")
                self.assert_rejected()

    def test_lookalike_flags_and_prose_cannot_stand_in_for_exact_flag_declarations(self):
        for help_text in ("Usage of /manager:\n  --leader-election\n  --health-probe-bind-address-backup string\n",
                          "Documentation mentions --leader-elect and --health-probe-bind-address.\n",
                          "Usage of /manager:\n  --leader-elect-disabled\n  --health-probe-bind-address string\n"):
            with self.subTest(help=help_text):
                self.state.update(stdout=help_text, stderr="")
                self.assert_rejected()

    def test_nonzero_entrypoint_exit_is_failure_even_with_complete_help(self):
        self.state.update(stdout=GO_HELP.replace("  -", "  --"), stderr="entrypoint failure", exit_code=2)
        self.assert_rejected()

    def test_foreign_architecture_is_rejected_before_manager_startup(self):
        self.state["inspected"]["Architecture"] = "arm64" if self.architecture == "amd64" else "amd64"
        self.assert_rejected()
        self.assertEqual(self.commands(), [["image", "inspect", IMAGE]])

    def test_nonlinux_root_and_wrong_source_revision_or_version_are_rejected_before_startup(self):
        variants = [("os", "windows"), ("user", "0"), ("source", "https://github.com/untrusted/operator"),
                    ("revision", "b" * 40), ("version", "0.1.0")]
        for field, value in variants:
            with self.subTest(field=field):
                self.state["inspected"] = copy.deepcopy(self.inspected)
                if field == "os":
                    self.state["inspected"]["Os"] = value
                elif field == "user":
                    self.state["inspected"]["Config"]["User"] = value
                else:
                    self.state["inspected"]["Config"]["Labels"]["org.opencontainers.image." + field] = value
                count = len(self.commands())
                self.assert_rejected()
                self.assertEqual(self.commands()[count:], [["image", "inspect", IMAGE]])


if __name__ == "__main__":
    unittest.main()
