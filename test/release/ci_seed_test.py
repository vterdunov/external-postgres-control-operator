"""Replay the actual CI seed loops with isolated test-command substitutes."""

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
CLOCK = 1700000000
MAXIMUM_FIRST_SEED = 9223372036854775805


class CISeedReplayTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        helm = shutil.which("helm")
        if helm is None:
            raise AssertionError("Run workflow tests with pinned mise tools")
        with tempfile.TemporaryDirectory(prefix="epco-ci-workflow-reader-") as temporary:
            chart = Path(temporary)
            (chart / "templates").mkdir()
            (chart / "inputs").mkdir()
            (chart / "Chart.yaml").write_text("apiVersion: v2\nname: ci-seed-reader\nversion: 0.0.0\n")
            shutil.copyfile(ROOT / ".github/workflows/test.yml", chart / "inputs/workflow.yaml")
            (chart / "templates/parsed.yaml").write_text('{{ .Files.Get "inputs/workflow.yaml" | fromYaml | toJson }}')
            result = subprocess.run([helm, "template", "ci-seed-reader", str(chart)],
                                    capture_output=True, text=True, timeout=30)
            if result.returncode:
                raise AssertionError("Cannot parse the Tests workflow with pinned Helm")
            cls.workflow = json.loads(result.stdout[result.stdout.index("{"):])
            if "on" not in cls.workflow and "true" in cls.workflow:
                cls.workflow["on"] = cls.workflow.pop("true")

    def execute(self, job_name, event, seed="", suite="test-unit"):
        job = self.workflow["jobs"][job_name]
        step_name = "Run suite and repeated random seeds" if job_name == "fast" else "PostgreSQL integration"
        step = next(step for step in job["steps"] if step.get("name") == step_name)
        environment = {**self.workflow.get("env", {}), **job.get("env", {}), **step.get("env", {})}
        rendered = {}
        for name, value in environment.items():
            if value == "${{ matrix.suite }}":
                value = suite
            elif value == "${{ matrix.pg }}":
                value = "18"
            elif value == "${{ github.event_name == 'schedule' || github.event_name == 'workflow_dispatch' }}":
                value = str(event in ("schedule", "workflow_dispatch")).lower()
            elif value in ("${{ inputs.seed }}", "${{ github.event.inputs.seed }}"):
                value = seed if event == "workflow_dispatch" else ""
            self.assertNotIn("${{", str(value), "The runner fixture must render every step environment expression")
            rendered[name] = str(value)
        # GitHub interpolates env values, never the untrusted input into shell code.
        self.assertNotRegex(step["run"], r"\$\{\{[^}]*inputs\.seed",
                            "Pass the seed through environment instead of interpolating shell source")
        with tempfile.TemporaryDirectory(prefix="epco-ci-seed-") as temporary:
            directory = Path(temporary)
            commands = directory / "commands.jsonl"
            marker = directory / "injected"
            fake_mise = directory / "mise"
            fake_mise.write_text(f"#!{sys.executable}\n"
                                 "import json, os, sys\nfrom pathlib import Path\n"
                                 "with Path(os.environ['COMMAND_TRACE']).open('a') as trace:\n"
                                 "    trace.write(json.dumps({'args': sys.argv[1:], "
                                 "'seed': os.environ.get('EPCO_TEST_SEED'), "
                                 "'artifacts': os.environ.get('EPCO_ARTIFACTS_DIR')}) + '\\n')\n")
            fake_mise.chmod(0o755)
            fake_date = directory / "date"
            fake_date.write_text(f"#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = +%s ] || exit 91\nprintf '%s\\n' {CLOCK}\n")
            fake_date.chmod(0o755)
            env = {key: value for key, value in os.environ.items()
                   if not key.startswith("EPCO_TEST_SEED")}
            env.update(rendered)
            env.update({"PATH": str(directory) + os.pathsep + os.environ["PATH"],
                        "COMMAND_TRACE": str(commands), "INJECTION_MARKER": str(marker),
                        "GITHUB_EVENT_NAME": event})
            result = subprocess.run(["bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", step["run"]],
                                    cwd=directory, env=env, capture_output=True, text=True, timeout=5)
            self.assertFalse(marker.exists(), "A seed must never execute shell input")
            observed = [json.loads(line) for line in commands.read_text().splitlines()] if commands.exists() else []
            return result, observed

    def assert_seeds(self, job, event, seed, expected, suite="test-unit"):
        result, observed = self.execute(job, event, seed, suite)
        self.assertEqual(result.returncode, 0, "Valid seed input must run the existing test loop")
        self.assertEqual([entry["seed"] for entry in observed], [str(value) for value in expected],
                         "The first recorded test seed must match the requested replay seed exactly")
        expected_suite = suite if job == "fast" else "test-integration"
        self.assertEqual([entry["args"] for entry in observed], [["run", expected_suite]] * len(expected))
        for index, entry in enumerate(observed, 1):
            self.assertTrue(entry["artifacts"].endswith(f"/{suite if job == 'fast' else 'integration'}/{index}"),
                            "Keep each test iteration's existing artifact directory")

    def test_dispatch_seed_is_optional_and_exposes_no_test_selection_controls(self):
        dispatch = self.workflow["on"].get("workflow_dispatch") or {}
        inputs = dispatch.get("inputs", {})
        self.assertEqual(set(inputs), {"seed"}, "Only the optional replay seed is a dispatch input")
        self.assertEqual(inputs["seed"].get("type"), "string")
        self.assertFalse(inputs["seed"].get("required", False))
        self.assertEqual(inputs["seed"].get("default", ""), "")

    def test_dispatch_replays_exact_first_seed_for_every_existing_suite(self):
        for job, suite in (("postgres", "test-integration"), ("fast", "test-unit"), ("fast", "test-envtest")):
            with self.subTest(job=job, suite=suite):
                self.assert_seeds(job, "workflow_dispatch", "1793726485", range(1793726485, 1793726488), suite)
        self.assert_seeds("fast", "workflow_dispatch", "1793726485", [1793726485], "check-generated")

    def test_decimal_leading_zeros_zero_and_int64_boundary(self):
        for seed, first in (("0001793726485", 1793726485), ("000", 0), (str(MAXIMUM_FIRST_SEED), MAXIMUM_FIRST_SEED)):
            for job in ("fast", "postgres"):
                with self.subTest(job=job, seed=seed):
                    self.assert_seeds(job, "workflow_dispatch", seed, range(first, first + 3))

    def test_blank_seed_preserves_event_profiles_and_iteration_counts(self):
        for event in ("push", "pull_request", "schedule", "workflow_dispatch"):
            runs = 3 if event in ("schedule", "workflow_dispatch") else 1
            for job, suite in (("postgres", "test-integration"), ("fast", "test-unit"),
                               ("fast", "test-envtest"), ("fast", "check-generated")):
                with self.subTest(event=event, job=job, suite=suite):
                    count = 1 if suite == "check-generated" else runs
                    self.assert_seeds(job, event, "", range(CLOCK + 1, CLOCK + count + 1), suite)

    def test_invalid_seed_fails_before_any_test_command_without_echoing_input(self):
        invalid = ("-1", "+1", "1.0", "1e3", " 1", "1 ", "1\n2", "١٢", "１２",
                   str(MAXIMUM_FIRST_SEED + 1), "9" * 100,
                   'seed-payload-$(touch "$INJECTION_MARKER")', 'seed-payload-`touch "$INJECTION_MARKER"`')
        for job in ("fast", "postgres"):
            for seed in invalid:
                with self.subTest(job=job, category="rejected-decimal-input"):
                    result, observed = self.execute(job, "workflow_dispatch", seed)
                    self.assertNotEqual(result.returncode, 0, "Invalid replay seed must fail the workflow step")
                    self.assertEqual(observed, [], "Reject invalid input before the first test command")
                    output = result.stdout + result.stderr
                    self.assertIn("seed", output.lower(), "Refusal must identify the invalid seed configuration")
                    self.assertNotIn(seed, output, "Refusal must not echo untrusted seed input")
                    self.assertLess(len(output), 512, "Invalid input should produce a bounded safe diagnostic")


if __name__ == "__main__":
    unittest.main()
