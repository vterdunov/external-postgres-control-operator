"""Command-boundary contracts for reusing immutable image runtime evidence."""

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from helper_support import attach_image_runtime, measured_runtime
from support import CHART, CHART_DIGEST, CHART_SHA, ReleaseFixture, checksum

ROOT = Path(__file__).resolve().parents[2]




class ChartRuntimeReuseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-chart-reuse-")
        self.addCleanup(temporary.cleanup)
        self.fixture = ReleaseFixture(Path(temporary.name) / "fixture")
        self.marker = attach_image_runtime(self.fixture)
        self.fixture.state["versions"].pop(CHART + ":0.2.1")
        self.fixture.manifest["reused_image_runtime"] = self.marker
        self.reports = Path(temporary.name) / "reports"
        self.reports.mkdir()
        self.lifecycle = self.reports / "delivery-lifecycle.json"
        self.lifecycle.write_text(json.dumps(measured_runtime(self.fixture.manifest, self.fixture.manifest_path.parent)["observed_lifecycle"]))
        self.output = Path(temporary.name) / "runtime.json"

    def command(self, script, *args):
        self.fixture.save()
        return subprocess.run([sys.executable, str(ROOT / "tools" / script), *map(str, args)],
                              env=self.fixture.env, capture_output=True, text=True, timeout=60)

    def aggregate(self):
        return self.command("release_ci.py", "aggregate", "--manifest", self.fixture.manifest_path,
                            "--reports", self.reports, "--output", self.output)

    def test_chart_aggregate_uses_signed_image_proof_and_fresh_lifecycle_without_matrix(self):
        result = self.aggregate()
        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(self.output.read_text())
        self.assertEqual(payload["reused_image_runtime"], self.marker)
        self.assertNotIn("observed_runtime", payload)
        self.assertEqual(payload["gates"], {"platforms": "reused", "runtime": "reused", "pg15-18": "reused", "lifecycle": "success"})
        self.assertEqual(payload["observed_lifecycle"]["chartSourceSHA"], CHART_SHA)
        self.assertTrue(any(call["tool"] == "gh" and call["args"][:2] == ["release", "verify-asset"] for call in self.fixture.calls()))

    def test_aggregate_rejects_forged_marker_and_missing_or_wrong_lifecycle(self):
        original = json.loads(self.lifecycle.read_text())
        for mutation in ("marker", "missing", "source", "archive"):
            with self.subTest(mutation=mutation):
                self.fixture.manifest["reused_image_runtime"] = dict(self.marker)
                self.lifecycle.write_text(json.dumps(original))
                if mutation == "marker":
                    self.fixture.manifest["reused_image_runtime"]["runtime_sha256"] = "f" * 64
                elif mutation == "missing":
                    self.lifecycle.unlink()
                else:
                    value = dict(original)
                    value["chartSourceSHA" if mutation == "source" else "chartArchiveSHA256"] = "f" * 64
                    self.lifecycle.write_text(json.dumps(value))
                self.assertNotEqual(self.aggregate().returncode, 0)
                self.assertFalse(self.output.exists())

    def test_preflight_verifies_original_before_writes_and_fails_without_durable_asset(self):
        self.fixture.manifest.pop("reused_image_runtime")
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "preflight")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["reused_image_runtime"], self.marker)
        self.assertTrue(any(call["tool"] == "gh" and call["args"][:2] == ["release", "verify-asset"] for call in self.fixture.calls()))
        (self.fixture.root / "accepted-image" / "release-runtime.json").write_text("{}")
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "preflight")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_forged_measured_runtime_cannot_bypass_missing_original_proof(self):
        directory = self.fixture.root / "accepted-image"
        original = copy.deepcopy(self.fixture.state["releases"]["v0.1.0"]["_manifest"])
        original.pop("measured_runtime")
        original["assets"] = [entry for entry in original["assets"] if entry["kind"] not in ("runtime_evidence", "evidence")]
        (directory / "release-runtime.json").unlink()
        evidence = {**original, "image_source_sha": original["source_sha"],
                    "image_index_digest": original["image"]["index_digest"], "chart_digest": None}
        evidence_file = directory / "release-evidence.json"
        evidence_file.write_text(json.dumps(evidence))
        original["assets"].append({"kind": "evidence", "path": evidence_file.name, "sha256": checksum(evidence_file.read_bytes())})
        (directory / "SHA256SUMS").write_text("".join(f'{entry["sha256"]}  {entry["path"]}\n' for entry in original["assets"]))
        (directory / "manifest.json").write_text(json.dumps(original))
        self.fixture.state["releases"]["v0.1.0"] = self.fixture.release(original, directory)
        self.fixture.manifest.pop("reused_image_runtime")
        self.fixture.manifest["measured_runtime"] = {}
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "preflight")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("image release lacks complete durable runtime proof", result.stderr)
        self.assertTrue(any(call["tool"] == "gh" and call["args"][:2] == ["release", "verify-asset"] for call in self.fixture.calls()))
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_prepare_rejects_forged_preflight_marker_before_registry_writes(self):
        from helper_support import PreparationFixture
        prepared = PreparationFixture(self.fixture.root / "preparation")
        marker = attach_image_runtime(prepared)
        marker["runtime_sha256"] = "f" * 64
        seed = json.loads(prepared.seed_path().read_text())
        seed["reused_image_runtime"] = marker
        path = prepared.root / "forged-preflight.json"
        path.write_text(json.dumps(seed))
        prepared.save()
        result = subprocess.run([sys.executable, str(prepared.helper), "prepare", "--manifest", str(path), "--output", str(prepared.output)],
                                cwd=prepared.source, env=prepared.env, capture_output=True, text=True, timeout=60)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse([call for call in prepared.calls() if call["mutation"]])

    def test_publish_persists_reused_marker_and_fresh_lifecycle(self):
        aggregate = self.aggregate()
        self.assertEqual(aggregate.returncode, 0, aggregate.stderr)
        self.fixture.state["runtime_payload"] = json.loads(self.output.read_text())
        self.fixture.state["versions"][CHART + ":0.2.1"] = CHART_DIGEST
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "publish")
        self.assertEqual(result.returncode, 0, result.stderr)
        released = json.loads(result.stdout)
        self.assertEqual(released["measured_runtime"]["reused_image_runtime"], self.marker)
        self.assertNotIn("observed_runtime", released["measured_runtime"])
        self.assertIn("observed_lifecycle", released["measured_runtime"])

    def test_publish_rejects_forged_current_runtime_artifact_marker(self):
        aggregate = self.aggregate()
        self.assertEqual(aggregate.returncode, 0, aggregate.stderr)
        self.fixture.state["runtime_payload"] = json.loads(self.output.read_text())
        self.fixture.state["runtime_payload"]["reused_image_runtime"]["runtime_sha256"] = "f" * 64
        self.fixture.state["versions"][CHART + ":0.2.1"] = CHART_DIGEST
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "publish")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("chart runtime evidence must bind the preflight image proof", result.stderr)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_publish_revalidates_original_and_rejects_tampering(self):
        result = self.aggregate()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.fixture.state["runtime_payload"] = json.loads(self.output.read_text())
        (self.fixture.root / "accepted-image" / "release-runtime.json").write_text("{}")
        self.fixture.state["versions"][CHART + ":0.2.1"] = CHART_DIGEST
        result = self.command("release.py", "chart", "--manifest", self.fixture.manifest_path, "--phase", "publish")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("release asset checksum mismatch: release-runtime.json", result.stderr)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])
