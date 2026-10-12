"""The two publication streams share one repository-wide release lock."""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class WorkflowReleaseContractTests(unittest.TestCase):
    def workflows(self):
        helm = shutil.which("helm")
        self.assertIsNotNone(helm, "Run workflow structure tests with pinned mise tools")
        with tempfile.TemporaryDirectory(prefix="epco-release-workflows-") as temporary:
            chart = Path(temporary)
            (chart / "templates").mkdir()
            (chart / "inputs").mkdir()
            (chart / "Chart.yaml").write_text("apiVersion: v2\nname: release-workflow-reader\nversion: 0.0.0\n")
            for stream in ("image", "chart"):
                shutil.copyfile(ROOT / f".github/workflows/release-{stream}.yml", chart / "inputs" / f"{stream}.yaml")
            (chart / "templates/parsed.yaml").write_text('{"image":{{ .Files.Get "inputs/image.yaml" | fromYaml | toJson }},"chart":{{ .Files.Get "inputs/chart.yaml" | fromYaml | toJson }}}')
            result = subprocess.run([helm, "template", "workflow-reader", str(chart)], capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            workflows = json.loads(result.stdout[result.stdout.index("{"):])
            # Helm's YAML 1.1 conversion treats GitHub's unquoted "on" key as true.
            for workflow in workflows.values():
                if "on" not in workflow and "true" in workflow:
                    workflow["on"] = workflow.pop("true")
            return workflows

    def test_entire_image_and_chart_streams_share_repository_wide_queued_lock(self):
        workflows = self.workflows()
        locks = [workflows[stream].get("concurrency") for stream in ("image", "chart")]
        self.assertTrue(all(isinstance(lock, dict) for lock in locks), "Lock both entire release workflows")
        self.assertEqual(locks[0]["group"], locks[1]["group"], "Independent streams must serialize registry and immutable-release preparation")
        for lock in locks:
            self.assertEqual(set(lock), {"group", "queue", "cancel-in-progress"}, "Only the validated concurrency contract keys are permitted")
            self.assertEqual(lock.get("queue"), "max", "Retain pending versions instead of silently replacing them")
            self.assertIs(lock.get("cancel-in-progress"), False)
            for variable in ("github.ref", "github.workflow", "github.run_id"):
                self.assertNotIn(variable, lock["group"], "Release lock must not vary by tag, stream or run")

    def test_release_workflows_validate_with_the_pinned_actionlint(self):
        checker = shutil.which("actionlint")
        self.assertIsNotNone(checker)
        result = subprocess.run([checker, str(ROOT / ".github/workflows/release-image.yml"),
                                 str(ROOT / ".github/workflows/release-chart.yml")],
                                cwd=ROOT, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_actions_artifact_upload_and_download_names_are_attempt_scoped(self):
        for stream, workflow in self.workflows().items():
            for job in workflow["jobs"].values():
                for step in job.get("steps", []):
                    action = step.get("uses", "")
                    if not action.startswith(("actions/upload-artifact@", "actions/download-artifact@")):
                        continue
                    options = step.get("with", {})
                    selection = options.get("name", options.get("pattern"))
                    self.assertIsInstance(selection, str, f"{stream} artifact selection must be explicit")
                    self.assertIn("github.run_attempt", selection,
                                  f"{stream} rerun can collide with or select stale artifacts: {selection}")

    def test_durable_checkpoint_prepare_can_write_draft_but_preflight_and_runtime_are_read_only(self):
        for stream, workflow in self.workflows().items():
            inherited = workflow.get("permissions", {})
            prepare = workflow["jobs"]["prepare"].get("permissions", inherited)
            self.assertEqual(prepare.get("contents"), "write", f"{stream} preparation must checkpoint candidate draft/assets before runtime")
            self.assertEqual(inherited.get("contents"), "read", "Keep default workflow token read-only")
            for name in ("preflight", "runtime", "lifecycle"):
                permissions = workflow["jobs"][name].get("permissions", inherited)
                self.assertEqual(permissions.get("contents"), "read", f"{stream} {name} must not write release contents")

    def test_trusted_tag_preflight_can_read_private_registry_without_write_authority(self):
        for stream, workflow in self.workflows().items():
            with self.subTest(stream=stream):
                self.assertEqual(set(workflow["on"]), {"push"}, "Registry credentials belong only to trusted tag publication")
                expected = "v*" if stream == "image" else "chart-v*"
                self.assertEqual(workflow["on"]["push"]["tags"], [expected])
                permissions = workflow["jobs"]["preflight"].get("permissions", workflow["permissions"])
                self.assertEqual(permissions.get("packages"), "read", "Anonymous DENIED cannot establish whether a version exists")
                self.assertEqual(permissions.get("contents"), "read")
                self.assertEqual(permissions.get("actions"), "read")
                self.assertNotEqual(permissions.get("id-token"), "write")

    def test_preflight_authenticates_crane_keychain_before_registry_gates_without_publication(self):
        for stream, workflow in self.workflows().items():
            with self.subTest(stream=stream):
                steps = workflow["jobs"]["preflight"]["steps"]
                gates = [index for index, step in enumerate(steps) if "--phase preflight" in step.get("run", "")]
                logins = [(index, step) for index, step in enumerate(steps) if "docker login" in step.get("run", "")]
                self.assertEqual(len(gates), 1)
                self.assertEqual(len(logins), 1, "GH_TOKEN alone does not populate crane's Docker keychain")
                index, login = logins[0]
                self.assertLess(index, gates[0], "Authenticate before any registry existence/recovery gates")
                self.assertIn("printf", login["run"])
                self.assertEqual(login.get("env", {}).get("ACTOR"), "${{ github.actor }}")
                self.assertEqual(workflow.get("env", {}).get("GH_TOKEN"), "${{ github.token }}")
                commands = "\n".join(step.get("run", "") for step in steps)
                self.assertIsNone(re.search(r"\b(?:docker\s+(?:build|push)|buildx\s+build|crane\s+(?:push|copy|tag)|cosign\s+sign(?:-blob)?|gh\s+release\s+(?:create|upload|edit)|helm\s+push)\b", commands),
                                  "Preflight must never build, sign or publish candidate/release artifacts")
                with tempfile.TemporaryDirectory(prefix="epco-preflight-login-") as temporary:
                    directory = Path(temporary)
                    trace = directory / "login.json"
                    docker = directory / "docker"
                    docker.write_text(f"#!{sys.executable}\nimport json,os,sys\nfrom pathlib import Path\nPath(os.environ['LOGIN_TRACE']).write_text(json.dumps({{'args':sys.argv[1:],'input':sys.stdin.read()}}))\n")
                    docker.chmod(0o755)
                    result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", login["run"]],
                                            env={**os.environ, "PATH": str(directory) + os.pathsep + os.environ["PATH"],
                                                 "GH_TOKEN": "fixture-readonly-token", "ACTOR": "fixture-owner", "LOGIN_TRACE": str(trace)},
                                            capture_output=True, text=True, timeout=15)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    observed = json.loads(trace.read_text())
                    self.assertEqual(observed, {"args": ["login", "ghcr.io", "--username", "fixture-owner", "--password-stdin"],
                                                "input": "fixture-readonly-token"})
                    self.assertNotIn("fixture-readonly-token", result.stdout + result.stderr)

    def test_ordinary_pull_request_delivery_has_no_registry_credentials(self):
        delivery = (ROOT / ".github/workflows/delivery.yml").read_text()
        self.assertNotRegex(delivery, r"(?m)^\s*packages:")
        self.assertNotRegex(delivery, r"GH_TOKEN|docker login|registry login")

    def test_release_workflows_do_not_require_manual_owner_precheck(self):
        for stream, workflow in self.workflows().items():
            with self.subTest(stream=stream):
                encoded = json.dumps(workflow)
                self.assertNotIn("EPCO_RELEASE_IMMUTABILITY_PRECHECK", encoded)
                self.assertNotIn("owner_preflight", encoded)


if __name__ == "__main__":
    unittest.main()
