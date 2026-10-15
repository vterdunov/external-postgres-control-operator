"""Exercise project Python selection against isolated, conflicting mise globals."""

import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import tomllib
import unittest


ROOT = Path(__file__).resolve().parents[2]


class PythonRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.config = tomllib.loads((ROOT / "mise.toml").read_text())
        self.version = self.config["vars"]["python_version"]
        self.mise = shutil.which("mise")
        self.uv = shutil.which("uv")
        self.assertIsNotNone(self.mise, "run delivery tests through the project's mise toolchain")
        self.assertIsNotNone(self.uv, "the pinned project uv must be installed")
        temporary = tempfile.TemporaryDirectory(prefix="epco-python-runtime-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name).resolve()
        for name in ("mise.toml", "mise.lock"):
            shutil.copy2(ROOT / name, self.directory / name)
        self.global_directory = self.directory / "global"
        self.global_directory.mkdir()
        self.global_config = self.global_directory / "config.toml"
        self.bin = self.directory / "ambient-bin"
        self.bin.mkdir()
        (self.bin / "mise").symlink_to(Path(self.mise).resolve())
        self.ambient_marker = self.directory / "ambient-python-executed"
        for name in ("python", "python3"):
            poison = self.bin / name
            poison.write_text("#!/bin/sh\n: > " + shlex.quote(str(self.ambient_marker))
                              + "\necho AmbientPythonMustNotExecute >&2\nexit 97\n")
            poison.chmod(0o700)
        # Do not inherit user settings, activated virtualenvs, credentials or
        # internal mise state. Existing tool/runtime stores remain read-only inputs.
        self.environment = {
            "HOME": str(Path.home()),
            "PATH": str(self.bin) + os.pathsep + os.pathsep.join(os.defpath.split(os.pathsep)),
            "MISE_CONFIG_DIR": str(self.global_directory),
            "MISE_GLOBAL_CONFIG_FILE": str(self.global_config),
            "MISE_TRUSTED_CONFIG_PATHS": str(self.directory),
            "MISE_CACHE_DIR": str(self.directory / "mise-cache"),
            "MISE_AUTO_INSTALL": "false",
            "MISE_YES": "true",
            "UV_CACHE_DIR": str(self.directory / "uv-cache"),
        }
        for name in ("MISE_DATA_DIR", "UV_PYTHON_INSTALL_DIR"):
            if name in os.environ:
                self.environment[name] = os.environ[name]
        managed = subprocess.run(
            [self.uv, "python", "find", "--managed-python", "cpython@" + self.version],
            env=self.environment, capture_output=True, text=True, timeout=30,
        )
        self.assertEqual(managed.returncode, 0, managed.stdout + managed.stderr)
        self.managed_base = Path(managed.stdout.strip()).resolve().parent.parent
        self.venv = self.directory / "bin" / ("python-" + self.version)
        self.assertFalse(self.venv.exists(), "each test must bootstrap a fresh project virtualenv")

    def assert_managed_runtime(self, global_python, *, stdlib_venv=False):
        global_config = '[tools]\npython = ' + json.dumps(global_python) + '\n'
        if stdlib_venv:
            global_config += '\n[settings]\npython.venv_stdlib = true\n'
        self.global_config.write_text(global_config)
        inspect = (
            'import json, sys; print(json.dumps({"version": list(sys.version_info[:3]), '
            '"base_prefix": sys.base_prefix, "executable": sys.executable}))'
        )
        outer = (
            'import json, subprocess, sys; '
            'nested = subprocess.run(["mise", "exec", "--", "python3", "-c", '
            + repr(inspect) + '], capture_output=True, text=True); '
            'assert nested.returncode == 0, nested.stdout + nested.stderr; '
            'print(json.dumps({"outer": {"version": list(sys.version_info[:3]), '
            '"base_prefix": sys.base_prefix, "executable": sys.executable}, '
            '"nested": json.loads(nested.stdout)}))'
        )
        result = subprocess.run(
            [self.mise, "exec", "--", "python3", "-c", outer],
            cwd=self.directory, env=self.environment, capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(result.returncode, 0,
                         "fresh project bootstrap must ignore conflicting global/ambient Python:\n"
                         + result.stdout + result.stderr)
        observations = json.loads(result.stdout)
        expected_version = [int(part) for part in self.version.split(".")]
        for invocation, observation in observations.items():
            with self.subTest(invocation=invocation):
                self.assertEqual(observation["version"], expected_version)
                self.assertEqual(Path(observation["base_prefix"]).resolve(), self.managed_base,
                                 "Python must be owned by uv, including when mise has the same version")
                self.assertEqual(Path(observation["executable"]).parent, self.venv / "bin",
                                 "outer and nested mise must activate the versioned project virtualenv")
        self.assertFalse(self.ambient_marker.exists(), "ambient Python must never execute")

    def test_global_system_python_does_not_override_project_uv_runtime(self):
        self.assert_managed_runtime("system")

    def test_global_same_version_mise_python_does_not_replace_uv_runtime(self):
        mise_data = Path(self.environment.get("MISE_DATA_DIR", Path.home() / ".local/share/mise"))
        if not (mise_data / "installs/python" / self.version).is_dir():
            self.skipTest("same-version mise Python backend is absent; do not install it for this test")
        self.assert_managed_runtime(self.version)

    def test_global_stdlib_venv_setting_does_not_bypass_uv(self):
        self.assert_managed_runtime("system", stdlib_venv=True)

    def test_python_pin_and_virtualenv_interpreter_stay_in_sync(self):
        venv = self.config["env"]["_"]["python"]["venv"]
        self.assertEqual(venv.get("python"), "cpython@" + self.version,
                         "a Python pin bump must update the explicit uv interpreter request")


if __name__ == "__main__":
    unittest.main()
