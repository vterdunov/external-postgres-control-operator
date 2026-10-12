"""Observed runtime evidence must describe the bytes actually tested."""

import copy
import datetime
import io
import json
from pathlib import Path
import subprocess
import shutil
import sys
import tempfile
import tarfile
import unittest

from support import AMD64, ARM64, CHART_SHA, IMAGE, IMAGE_SHA, INDEX, ReleaseFixture, checksum
from helper_support import CNI_GATE, CNI_IMAGE, PSA_GATE, PreparationFixture, measured_runtime, security_lifecycle_gates


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "tools/release_ci.py"
LIFECYCLE_GATES = (
    "CR readiness, real application SQL and credential controls",
    "second release refused in same and other namespace",
    "same-UID removed named-table grant revoked in real SQL; unrelated access preserved",
    "unknown schema refused before schema or manager writes; known schema reinstalls",
    "same-package Helm upgrade and controller revision rollback mechanics",
    "live, terminating and real list-denied retirement refusal",
    "delivery lifecycle complete",
)


class RuntimeAggregateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-release-aggregate-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.fixture = ReleaseFixture(self.directory / "fixture")
        self.reports = self.directory / "reports"
        self.paths = []
        self.output = self.directory / "release-runtime.json"
        subjects = [IMAGE + "@" + INDEX, IMAGE + "@" + AMD64, IMAGE + "@" + ARM64,
                    self.fixture.manifest["chart"]["repository"] + "@" + self.fixture.manifest["chart"]["digest"]]
        for architecture, digest in (("amd64", AMD64), ("arm64", ARM64)):
            for postgres in (15, 16, 17, 18):
                path = self.reports / f"{architecture}-pg{postgres}" / "release-installed.json"
                path.parent.mkdir(parents=True)
                path.write_text(json.dumps({
                    "source_sha": CHART_SHA, "image_source_sha": IMAGE_SHA, "subjects": subjects,
                    "pg_major": str(postgres), "context": f"kind-epco-e2e-{architecture}-{postgres}",
                    "observations": [{"imageID": "ghcr.io/example@" + digest, "architecture": architecture,
                                      "platform_digest": digest, "podUID": f"pod-{architecture}-{postgres}"}],
                    "leaseUID": f"lease-{architecture}-{postgres}",
                    "fixture_observed": "server version: " + {15: "15.19", 16: "16.15", 17: "17.11", 18: "18.6"}[postgres] + "\nclient version: 16.15\n",
                }))
                self.paths.append(path)
        self.lifecycle = self.reports / "lifecycle" / "delivery-lifecycle.json"
        self.lifecycle.parent.mkdir()
        self.lifecycle.write_text(json.dumps({
            "scope": "selected artifact lifecycle", "imageSourceSHA": IMAGE_SHA, "chartSourceSHA": CHART_SHA,
            "imageReference": IMAGE + ":0.1.0@" + INDEX,
            "chartArchiveSHA256": checksum((self.fixture.manifest_path.parent / self.fixture.manifest["chart"]["archive"]).read_bytes()),
            "gates": [{"gate": gate, "result": "passed", **({"podImageID": "ghcr.io/example@" + AMD64} if index == 0 else {})}
                      for index, gate in enumerate(LIFECYCLE_GATES)] + security_lifecycle_gates(),
        }))

    def mutate(self, path, change):
        payload = json.loads(path.read_text())
        change(payload)
        path.write_text(json.dumps(payload))

    def run_aggregate(self):
        self.fixture.save()
        return subprocess.run([sys.executable, str(HELPER), "aggregate", "--manifest", str(self.fixture.manifest_path),
                               "--reports", str(self.reports), "--output", str(self.output)],
                              env=self.fixture.env, capture_output=True, text=True, timeout=15)

    def reject(self):
        result = self.run_aggregate()
        self.assertNotEqual(result.returncode, 0, "Unverified measured evidence became successful runtime proof: " + result.stdout)
        self.assertFalse(self.output.exists(), "Failure wrote successful aggregate evidence")

    def test_complete_matrix_preserves_independent_source_subjects_and_observations(self):
        result = self.run_aggregate()
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(self.output.read_text())
        self.assertEqual(output["source_sha"], CHART_SHA)
        self.assertEqual(output["image_source_sha"], IMAGE_SHA)
        self.assertEqual(len(output["observed_runtime"]), 8)
        self.assertEqual(output["gates"], {"platforms": "success", "runtime": "success", "lifecycle": "success", "pg15-18": "success"})
        self.assertEqual(self.fixture.calls(), [], "aggregate must not query or mutate registry/cluster")

    def test_installed_actual_image_id_must_be_a_selected_signed_subject(self):
        self.mutate(self.paths[0], lambda report: report["observations"][0].update(imageID="ghcr.io/example@sha256:" + "f" * 64))
        self.reject()

    def test_installed_actual_manifest_must_match_observed_architecture(self):
        self.mutate(self.paths[0], lambda report: report["observations"][0].update(imageID="ghcr.io/example@" + ARM64))
        self.reject()

    def test_installed_platform_digest_must_match_selected_architecture(self):
        self.mutate(self.paths[0], lambda report: report["observations"][0].update(platform_digest=ARM64))
        self.reject()

    def test_missing_installed_pod_and_lease_identity_cannot_claim_observed_acceptance(self):
        self.mutate(self.paths[0], lambda report: (report.update(leaseUID=""), report["observations"][0].update(podUID="")))
        self.reject()

    def test_unmeasured_server_client_versions_are_not_runtime_proof(self):
        self.mutate(self.paths[0], lambda report: report.update(fixture_observed=""))
        self.reject()

    def test_saved_production_context_is_not_an_owned_runtime_fixture(self):
        self.mutate(self.paths[0], lambda report: report.update(context="production"))
        self.reject()

    def test_missing_matrix_member_cannot_pass(self):
        self.paths[-1].unlink()
        self.reject()

    def test_duplicate_matrix_slot_cannot_replace_other_architecture(self):
        self.mutate(self.paths[-1], lambda report: report.update(pg_major="17"))
        self.reject()

    def test_cross_stream_source_or_subjects_cannot_pass(self):
        original = self.paths[0].read_text()
        for field, value in (("source_sha", IMAGE_SHA), ("image_source_sha", CHART_SHA), ("subjects", [IMAGE + "@" + INDEX])):
            with self.subTest(field=field):
                self.paths[0].write_text(original)
                self.mutate(self.paths[0], lambda report: report.update({field: value}))
                self.reject()

    def test_failed_or_missing_lifecycle_gate_cannot_pass(self):
        self.mutate(self.lifecycle, lambda report: report["gates"][0].update(result="failed"))
        self.reject()

    def test_missing_actual_psa_or_cni_security_gate_cannot_pass(self):
        original = self.lifecycle.read_text()
        for gate in (PSA_GATE, CNI_GATE):
            with self.subTest(gate=gate):
                self.lifecycle.write_text(original)
                self.mutate(self.lifecycle, lambda report: report.update(gates=[entry for entry in report["gates"] if entry["gate"] != gate]))
                self.reject()

    def test_failed_actual_psa_or_cni_security_gate_cannot_pass(self):
        original = self.lifecycle.read_text()
        for gate in (PSA_GATE, CNI_GATE):
            with self.subTest(gate=gate):
                self.lifecycle.write_text(original)
                self.mutate(self.lifecycle, lambda report: next(entry for entry in report["gates"] if entry["gate"] == gate).update(result="failed"))
                self.reject()

    def test_security_gate_label_without_actual_admission_or_cni_observations_cannot_pass(self):
        original = self.lifecycle.read_text()
        invalid = ((PSA_GATE, {"admission": "template lint"}),
                   (CNI_GATE, {"cniDaemonImages": []}),
                   (CNI_GATE, {"cniDaemonImages": ["invalid\nimage"]}),
                   (CNI_GATE, {"metricsSameNamespace": "denied"}),
                   (CNI_GATE, {"metricsOtherNamespace": "allowed"}),
                   (CNI_GATE, {"podHealthPort": "allowed"}))
        for gate, values in invalid:
            with self.subTest(gate=gate, values=values):
                self.lifecycle.write_text(original)
                self.mutate(self.lifecycle, lambda report: next(entry for entry in report["gates"] if entry["gate"] == gate).update(values))
                self.reject()

    def test_archive_bytes_must_match_lifecycle_observation(self):
        self.mutate(self.lifecycle, lambda report: report.update(chartArchiveSHA256="0" * 64))
        self.reject()


class DraftRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-release-finalize-")
        self.addCleanup(temporary.cleanup)
        self.fixture = ReleaseFixture(temporary.name)

    def publish(self):
        self.fixture.save()
        return subprocess.run([sys.executable, str(ROOT / "tools/release.py"), "chart", "--manifest",
                               str(self.fixture.manifest_path), "--phase", "publish"],
                              env=self.fixture.env, text=True, capture_output=True, timeout=30)

    def test_verified_complete_draft_reuses_original_nondeterministic_signature_bytes(self):
        snapshot = self.fixture.root / "remote-draft"
        shutil.copytree(self.fixture.manifest_path.parent, snapshot)
        self.fixture.state["releases"][self.fixture.manifest["tag"]] = self.fixture.release(
            self.fixture.manifest, snapshot, immutable=False, draft=True)
        self.fixture.state["fresh_signature_nonces"] = True
        before = {path.name: path.read_bytes() for path in snapshot.iterdir()}
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["completed"])
        resigns = [call for call in self.fixture.calls() if call["tool"] == "cosign" and call["args"][0] in ("sign", "sign-blob")]
        self.assertEqual(resigns, [], "Verified matching draft signatures must be reused, not regenerated")
        self.assertEqual(before, {path.name: path.read_bytes() for path in snapshot.iterdir()})
        self.assertFalse(any("--clobber" in call["args"] for call in self.fixture.calls()))

    def test_owner_checkpoint_must_still_be_recent_at_actual_publication(self):
        self.fixture.manifest["owner_preflight"]["checked_at"] = (
            datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=2)).isoformat()
        result = self.publish()
        self.assertNotEqual(result.returncode, 0, "Stale preparation checkpoint authorized actual publication")
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]], "Reject stale owner checkpoint before signing or publication")

    def test_tag_movement_during_proof_signing_and_upload_blocks_final_publish(self):
        self.fixture.state["move_own_tag_after_reads"] = 1
        result = self.publish()
        self.assertNotEqual(result.returncode, 0, "Tag movement during release preparation was ignored")
        self.assertFalse(any(call["tool"] == "gh" and call["args"][:2] == ["release", "edit"] for call in self.fixture.calls()))

    def test_published_tag_must_still_bind_the_signed_application_source(self):
        self.fixture.state["move_tag_after_publish"] = True
        result = self.publish()
        self.assertNotEqual(result.returncode, 0, "Postpublication release proof ignored actual tag source")
        self.assertFalse(result.stdout.strip(), "Wrong-source release must not be returned as completed")

    def test_recursive_annotated_tag_resolution_binds_the_actual_commit(self):
        self.fixture.state["tag_objects"] = {"chart-v0.2.1": {"type": "tag", "sha": "7" * 40}}
        self.fixture.state["annotated_tag_objects"] = {"7" * 40: {"type": "tag", "sha": "8" * 40},
                                                       "8" * 40: {"type": "commit", "sha": CHART_SHA}}
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["completed"])


class RuntimePersistenceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-measured-release-")
        self.addCleanup(temporary.cleanup)
        self.fixture = ReleaseFixture(temporary.name)
        payload = self.fixture.state["runtime_payload"]
        payload["observed_runtime"] = []
        for architecture, digest in (("amd64", AMD64), ("arm64", ARM64)):
            for postgres in (15, 16, 17, 18):
                payload["observed_runtime"].append({
                    "source_sha": CHART_SHA, "image_source_sha": IMAGE_SHA, "subjects": payload["subjects"],
                    "pg_major": str(postgres), "context": f"kind-epco-e2e-{architecture}-pg{postgres}",
                    "observations": [{"imageID": "ghcr.io/example@" + digest, "architecture": architecture,
                                      "platform_digest": digest, "podUID": f"pod-{architecture}-pg{postgres}"}],
                    "leaseUID": f"lease-{architecture}-pg{postgres}",
                    "fixture_observed": "server observed version: " + {15: "15.19", 16: "16.15", 17: "17.11", 18: "18.6"}[postgres] + "\nclient observed version: 16.15\n",
                })
        payload["observed_lifecycle"] = {
            "scope": "selected artifact lifecycle", "imageSourceSHA": IMAGE_SHA, "chartSourceSHA": CHART_SHA,
            "imageReference": IMAGE + ":0.1.0@" + INDEX,
            "chartArchiveSHA256": checksum((self.fixture.manifest_path.parent / self.fixture.manifest["chart"]["archive"]).read_bytes()),
            "gates": [{"gate": gate, "result": "passed", **({"podImageID": "ghcr.io/example@" + AMD64} if index == 0 else {})}
                      for index, gate in enumerate(LIFECYCLE_GATES)] + security_lifecycle_gates(),
        }

    def invoke(self):
        self.fixture.save()
        return subprocess.run([sys.executable, str(ROOT / "tools/release.py"), "chart", "--manifest",
                               str(self.fixture.manifest_path), "--phase", "publish"],
                              env=self.fixture.env, text=True, capture_output=True, timeout=30)

    def evidence(self):
        state = json.loads(self.fixture.state_path.read_text())
        directory = Path(state["releases"][self.fixture.manifest["tag"]]["_directory"])
        return json.loads((directory / "release-evidence.json").read_text())

    def test_signed_immutable_evidence_preserves_measured_pods_leases_versions_and_test_sources(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        content = json.dumps(self.evidence())
        for value in ("pod-amd64-pg15", "pod-arm64-pg18", "lease-amd64-pg16", "lease-arm64-pg17",
                      AMD64, ARM64, IMAGE_SHA, CHART_SHA, "server observed version", "client observed version", "16.15"):
            self.assertIn(value, content, "Expiring Actions coordinates cannot replace durable measured release evidence")

    def test_signed_immutable_runtime_preserves_actual_psa_and_cni_security_observations(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = self.evidence()
        gates = evidence["measured_runtime"]["observed_lifecycle"]["gates"]
        for expected in security_lifecycle_gates():
            matches = [entry for entry in gates if entry["gate"] == expected["gate"]]
            self.assertEqual(matches, [expected], "Signed evidence lost measured admission or CNI enforcement facts")
        self.assertIn(CNI_IMAGE, json.dumps(evidence))
        self.assertNotIn("egressIsolated", json.dumps(evidence), "Default ingress-only proof must not claim egress isolation")

    def test_security_observation_sanitization_excludes_unrecognized_sensitive_fields(self):
        gates = self.fixture.state["runtime_payload"]["observed_lifecycle"]["gates"]
        for entry in gates:
            if entry["gate"] in (PSA_GATE, CNI_GATE):
                entry.update(token="security-secret-sentinel", rawDiagnostics="security-secret-sentinel")
        result = self.invoke()
        if result.returncode == 0:
            content = json.dumps(self.evidence())
            self.assertNotIn("security-secret-sentinel", content)
            for expected in security_lifecycle_gates():
                self.assertIn(expected, self.evidence()["measured_runtime"]["observed_lifecycle"]["gates"])
        else:
            self.assertFalse(any(call["tool"] == "gh" and call["args"][:2] == ["release", "edit"] for call in self.fixture.calls()))

    def test_sensitive_unrecognized_runtime_fields_are_rejected_or_not_attached(self):
        payload = self.fixture.state["runtime_payload"]
        payload["secrets"] = "sensitive-sentinel-do-not-publish"
        payload["observed_runtime"][0]["password"] = "sensitive-sentinel-do-not-publish"
        payload["observed_lifecycle"]["gates"][0]["token"] = "sensitive-sentinel-do-not-publish"
        result = self.invoke()
        if result.returncode == 0:
            self.assertNotIn("sensitive-sentinel-do-not-publish", json.dumps(self.evidence()))
        else:
            self.assertFalse(any(call["tool"] == "gh" and call["args"][:2] == ["release", "edit"] for call in self.fixture.calls()))

    def test_immutable_recovery_does_not_depend_on_expired_actions_artifact(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        self.fixture.fail("gh", ["api", "actions/"], "HTTP 410: evidence artifact expired")
        old_calls = len(self.fixture.calls())
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        subsequent = self.fixture.calls()[old_calls:]
        self.assertFalse([call for call in subsequent if call["mutation"]])
        self.assertFalse([call for call in subsequent if "actions/" in " ".join(call["args"])])

    def test_bind_runtime_records_current_attempt(self):
        self.fixture.env["GITHUB_RUN_ATTEMPT"] = "2"
        self.fixture.save()
        result = subprocess.run([sys.executable, str(HELPER), "bind-runtime", "--manifest", str(self.fixture.manifest_path), "--artifact-id", "902"],
                                env=self.fixture.env, capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        proof = json.loads(self.fixture.manifest_path.read_text())["runtime_evidence"]
        self.assertEqual(proof, {"run_id": 900, "run_attempt": 2, "artifact_id": 902})

    def test_runtime_artifact_name_must_match_recorded_attempt(self):
        self.fixture.manifest["runtime_evidence"]["run_attempt"] = 2
        self.fixture.env["GITHUB_RUN_ATTEMPT"] = "2"
        self.fixture.state["runtime_run"]["run_attempt"] = 2
        self.fixture.state["runtime_artifact"]["name"] = "release-runtime-1"
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0, "Current attempt accepted previous attempt's artifact")
        self.assertFalse(any(call["tool"] == "gh" and call["args"][:2] == ["release", "edit"] for call in self.fixture.calls()))

    def save_finalized_draft(self):
        manifest = self.fixture.manifest
        manifest["measured_runtime"] = copy.deepcopy(self.fixture.state["runtime_payload"])
        runtime_file = self.fixture.manifest_path.parent / "release-runtime.json"
        runtime_file.write_text(json.dumps(manifest["measured_runtime"], sort_keys=True))
        manifest["assets"].append({"kind": "runtime_evidence", "path": runtime_file.name, "sha256": checksum(runtime_file.read_bytes())})
        evidence = {**manifest, "assets": [entry for entry in manifest["assets"] if entry["kind"] != "evidence"]}
        evidence_file = self.fixture.manifest_path.parent / "release-evidence.json"
        evidence_file.write_text(json.dumps(evidence, sort_keys=True))
        next(entry for entry in manifest["assets"] if entry["kind"] == "evidence")["sha256"] = checksum(evidence_file.read_bytes())
        (self.fixture.manifest_path.parent / "SHA256SUMS").write_text("".join(entry["sha256"] + "  " + entry["path"] + "\n" for entry in manifest["assets"]))
        self.fixture.save()
        snapshot = self.fixture.root / "finalized-draft"
        shutil.copytree(self.fixture.manifest_path.parent, snapshot)
        self.fixture.state["releases"][self.fixture.manifest["tag"]] = self.fixture.release(self.fixture.manifest, snapshot, immutable=False, draft=True)
        return snapshot

    def test_current_owner_refresh_allows_historical_finalized_draft_without_rewriting_assets(self):
        fresh = copy.deepcopy(self.fixture.manifest["owner_preflight"])
        self.fixture.manifest["owner_preflight"]["checked_at"] = (
            datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=2)).isoformat()
        snapshot = self.save_finalized_draft()
        original = {path.name: path.read_bytes() for path in snapshot.iterdir()}
        self.fixture.env["EPCO_RELEASE_IMMUTABILITY_PRECHECK"] = json.dumps(fresh)
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["completed"])
        self.assertEqual(original, {path.name: path.read_bytes() for path in snapshot.iterdir()})
        self.assertFalse([call for call in self.fixture.calls() if
                          (call["tool"] == "cosign" and call["args"][0] in ("sign", "sign-blob")) or
                          (call["tool"] == "gh" and call["args"][:2] == ["release", "upload"])])

    def test_stale_current_owner_checkpoint_blocks_even_fresh_historical_finalized_draft(self):
        stale = copy.deepcopy(self.fixture.manifest["owner_preflight"])
        stale["checked_at"] = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=2)).isoformat()
        self.save_finalized_draft()
        self.fixture.env["EPCO_RELEASE_IMMUTABILITY_PRECHECK"] = json.dumps(stale)
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0, "Fresh historical evidence overrode stale current owner authorization")
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_complete_finalized_draft_reuses_recorded_successful_attempt_after_a_later_failure(self):
        self.save_finalized_draft()
        first = copy.deepcopy(self.fixture.state["runtime_run"])
        self.fixture.state["runtime_attempts"] = {"1": first}
        self.fixture.state["runtime_run"]["run_attempt"] = 2
        self.fixture.state["jobs_by_attempt"] = {"900:1": [{"name": "Runtime acceptance", "status": "completed", "conclusion": "success"}],
                                                 "900:2": [{"name": "Runtime acceptance", "status": "completed", "conclusion": "failure"}]}
        self.fixture.state["runtime_artifact"]["name"] = "release-runtime-1"
        self.fixture.env["GITHUB_RUN_ATTEMPT"] = "2"
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["completed"])
        self.assertFalse([call for call in self.fixture.calls() if call["tool"] == "cosign" and call["args"][0] in ("sign", "sign-blob")])


class PreparationBehaviorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-release-prepare-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.fixture = PreparationFixture(self.directory / "fixture")

    def helper(self, action, *arguments):
        self.fixture.save()
        return subprocess.run([sys.executable, str(self.fixture.helper), action, *map(str, arguments)],
                              cwd=self.fixture.source, env=self.fixture.env, capture_output=True, text=True, timeout=60)

    def prepare(self):
        seed = self.fixture.seed_path()
        result = self.helper("prepare", "--manifest", seed, "--output", self.fixture.output)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        path = self.fixture.output / "manifest.json"
        return path, json.loads(path.read_text())

    def application(self):
        path = self.directory / "application-source"
        path.mkdir()
        (path / "test").mkdir()
        (path / "test/original-behavior.txt").write_text("original application assertions")
        return path

    def test_prepare_rejects_unrelated_existing_output_before_build_or_external_writes(self):
        self.fixture.output.mkdir()
        unrelated = self.fixture.output / "unrelated-private-input.txt"
        unrelated.write_text("unrelated content must never enter a release checkpoint")
        result = self.helper("prepare", "--manifest", self.fixture.seed_path(), "--output", self.fixture.output)
        self.assertNotEqual(result.returncode, 0, "Nonempty output was archived into the signed release checkpoint")
        self.assertEqual(unrelated.read_text(), "unrelated content must never enter a release checkpoint")
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"] or call["tool"] == "docker"])

    def test_chart_prepare_materializes_old_signed_inputs_and_actual_independent_defaults(self):
        path, manifest = self.prepare()
        self.assertEqual(manifest["chart"]["version"], "0.2.1")
        self.assertEqual(manifest["chart"]["app_version"], "0.1.0")
        self.assertEqual(manifest["source_sha"], CHART_SHA)
        self.assertEqual(manifest["image"]["source_sha"], IMAGE_SHA)
        self.assertEqual(manifest["image"]["index_digest"], INDEX)
        original = self.fixture.root / "accepted-image"
        with tarfile.open(path.parent / manifest["chart"]["archive"], "r:gz") as package:
            metadata = package.extractfile("external-postgres-control-operator/Chart.yaml").read().decode()
            defaults = package.extractfile("external-postgres-control-operator/values.yaml").read().decode()
            self.assertIn("version: 0.2.1", metadata)
            self.assertRegex(metadata, r'(?m)^appVersion:\s*["\x27]?0\.1\.0["\x27]?\s*$')
            for value in (IMAGE, '"0.1.0"', INDEX):
                self.assertIn(value, defaults)
            for entry in manifest["image"]["generated_inputs"]:
                relative = "generated/manager-role.yaml" if entry["kind"] == "rbac" else "crds/" + Path(entry["input_path"]).name
                content = package.extractfile("external-postgres-control-operator/" + relative).read()
                self.assertEqual(content, (original / entry["path"]).read_bytes())
                self.assertEqual(checksum(content), entry["sha256"])
                self.assertNotIn(b"fixture chart HEAD", content)
        self.assertFalse([call for call in self.fixture.calls() if call["tool"] == "docker" or (call["mutation"] and IMAGE in " ".join(call["args"]))])
        self.assertTrue(any(call["tool"] == "helm" and call["args"][0] == "package" for call in self.fixture.calls()))

    def test_image_prepare_binds_generated_inventory_to_built_subject_source(self):
        self.fixture = PreparationFixture(self.directory / "image", "image")
        path, manifest = self.prepare()
        self.assertEqual(manifest["image"]["source_sha"], IMAGE_SHA)
        self.assertEqual(manifest["source_sha"], IMAGE_SHA)
        self.assertEqual({entry["architecture"] for entry in manifest["image"]["platforms"]}, {"amd64", "arm64"})
        records = {entry["path"]: entry for entry in manifest["assets"]}
        self.assertTrue({"application_config", "installer", "controller_removal", "build_metadata", "evidence"}.issubset({entry["kind"] for entry in records.values()}))
        for entry in manifest["image"]["generated_inputs"]:
            self.assertEqual(entry["source_sha"], IMAGE_SHA)
            self.assertEqual((path.parent / entry["path"]).read_bytes(), (self.fixture.source / "config" / entry["input_path"]).read_bytes())
            self.assertEqual(records[entry["path"]]["sha256"], checksum((path.parent / entry["path"]).read_bytes()))
        built = [call for call in self.fixture.calls() if call["tool"] == "docker" and call["args"][:2] == ["buildx", "build"]]
        self.assertEqual(len(built), 1)
        self.assertIn("REVISION=" + IMAGE_SHA, built[0]["args"])
        self.assertFalse(any(call["tool"] == "helm" and call["args"][0] == "push" for call in self.fixture.calls()), "Image validation chart must remain local")

    def test_chart_seed_rejects_tag_that_differs_from_source_metadata(self):
        self.fixture.env["GITHUB_REF"] = "refs/tags/chart-v0.2.2"
        result = self.helper("seed", "--stream", "chart", "--output", self.directory / "seed.json")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_chart_prepare_rejects_original_signature_failure_before_chart_write(self):
        self.fixture.fail("cosign", ["verify", IMAGE + "@" + INDEX], "original image proof invalid")
        result = self.helper("prepare", "--manifest", self.fixture.seed_path(), "--output", self.fixture.output)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])
        self.assertFalse((self.fixture.output / "manifest.json").exists())

    def test_chart_prepare_rejects_tampered_original_generated_asset(self):
        original = self.fixture.root / "accepted-image"
        entry = self.fixture.manifest["image"]["generated_inputs"][0]
        (original / entry["path"]).write_bytes(b"unverified newer schema")
        result = self.helper("prepare", "--manifest", self.fixture.seed_path(), "--output", self.fixture.output)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]])

    def test_workspace_preserves_original_behavior_and_materializes_signed_config(self):
        manifest_path, manifest = self.prepare()
        application = self.application()
        result = self.helper("workspace", "--manifest", manifest_path, "--application", application)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((application / "test/original-behavior.txt").read_text(), "original application assertions")
        for entry in manifest["image"]["generated_inputs"]:
            self.assertEqual(checksum((application / "config" / entry["input_path"]).read_bytes()), entry["sha256"])

    def test_workspace_rejects_failed_original_signature_before_materialization(self):
        manifest_path, _ = self.prepare()
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        self.fixture.fail("cosign", ["verify", IMAGE + "@" + INDEX], "image signature no longer verifies")
        application = self.application()
        result = self.helper("workspace", "--manifest", manifest_path, "--application", application)
        self.assertNotEqual(result.returncode, 0, "Workspace consumed config without authenticating selected image proof")
        self.assertFalse((application / "config").exists())

    def test_workspace_rejects_a_different_application_checkout(self):
        manifest_path, _ = self.prepare()
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        self.fixture.state["application_checkout_sha"] = CHART_SHA
        application = self.application()
        result = self.helper("workspace", "--manifest", manifest_path, "--application", application)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((application / "config").exists())

    def test_workspace_rejects_untrusted_config_archive_paths(self):
        manifest_path, manifest = self.prepare()
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        entry = next(asset for asset in manifest["assets"] if asset["kind"] == "application_config")
        archive_path = manifest_path.parent / entry["path"]
        with tarfile.open(archive_path, "w:gz") as archive:
            header = tarfile.TarInfo("config/../../escape.txt")
            content = b"outside workspace config"
            header.size = len(content)
            archive.addfile(header, io.BytesIO(content))
        entry["sha256"] = checksum(archive_path.read_bytes())
        manifest_path.write_text(json.dumps(manifest))
        (manifest_path.parent / "SHA256SUMS").write_text("".join(asset["sha256"] + "  " + asset["path"] + "\n" for asset in manifest["assets"]))
        application = self.application()
        result = self.helper("workspace", "--manifest", manifest_path, "--application", application)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.directory / "escape.txt").exists())

    def test_complete_image_draft_recovery_retains_or_regenerates_verified_local_validation_chart(self):
        self.fixture = PreparationFixture(self.directory / "image-resume", "image")
        manifest_path, manifest = self.prepare()
        snapshot = self.directory / "remote-image-draft"
        snapshot.mkdir()
        for name in {entry["path"] for entry in manifest["assets"]} | {"SHA256SUMS", "SHA256SUMS.sigstore.json"}:
            shutil.copyfile(manifest_path.parent / name, snapshot / name)
        self.fixture.state["releases"]["v0.1.0"] = self.fixture.release(manifest, snapshot, immutable=False, draft=True)
        seed = self.directory / "resume.json"
        seed.write_text(json.dumps({key: manifest[key] for key in ("schema_version", "stream", "repository", "tag", "source_sha")} | {"completed": False}))
        recovered = self.directory / "recovered"
        result = self.helper("prepare", "--manifest", seed, "--output", recovered)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        self.fixture.env["GITHUB_ENV"] = str(self.directory / "github-env")
        result = self.helper("environment", "--manifest", recovered / "manifest.json")
        self.assertEqual(result.returncode, 0, "Verified image draft recovery cannot start lifecycle acceptance: " + result.stderr)
        validation = recovered / "external-postgres-control-operator-0.0.0-image-validation.tgz"
        self.assertTrue(validation.is_file())
        with tarfile.open(validation, "r:gz") as archive:
            values = archive.extractfile("external-postgres-control-operator/values.yaml").read().decode()
            self.assertIn(INDEX, values)
            for entry in manifest["image"]["generated_inputs"]:
                if entry["kind"] == "crds":
                    content = archive.extractfile("external-postgres-control-operator/crds/" + Path(entry["input_path"]).name).read()
                    self.assertEqual(checksum(content), entry["sha256"])
        # Only the original image preparation may build it. A complete matching
        # draft recovery cannot add a second build or re-sign the image.
        builds = [call for call in self.fixture.calls() if call["tool"] == "docker" and call["args"][:2] == ["buildx", "build"]]
        self.assertEqual(len(builds), 1)

    def test_candidate_is_durable_before_runtime_and_recovers_after_local_artifacts_are_lost(self):
        self.fixture = PreparationFixture(self.directory / "image-checkpoint", "image")
        self.fixture.state["fresh_signature_nonces"] = True
        manifest_path, manifest = self.prepare()
        checkpoint = self.fixture.state["releases"].get("v0.1.0")
        self.assertIsNotNone(checkpoint, "Prepared OCI version has no durable draft recovery before runtime")
        self.assertTrue(checkpoint["draft"])
        self.assertTrue(checkpoint["assets"], "An empty draft cannot recover nondeterministic candidate bytes")
        bundles = {subject["bundle"]: (manifest_path.parent / subject["bundle"]).read_bytes()
                   for subject in [manifest["image"], *manifest["image"]["platforms"]]}
        writes_before = [call for call in self.fixture.calls() if call["mutation"]]
        # Simulate expiration of the runtime job's Actions artifact and a private
        # first package. Owner visibility changes must not require a new image.
        shutil.rmtree(self.fixture.output)
        self.fixture.state["private"] = [IMAGE]
        seed = self.directory / "lost-candidate-seed.json"
        seed.write_text(json.dumps({key: manifest[key] for key in ("schema_version", "stream", "repository", "tag", "source_sha")}))
        self.fixture.save()
        result = subprocess.run([sys.executable, str(self.fixture.source / "tools/release.py"), "image", "--manifest", str(seed), "--phase", "preflight"],
                                env=self.fixture.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)
        resumed = json.loads(result.stdout)
        self.assertFalse(resumed["completed"])
        self.assertEqual(resumed["image"], manifest["image"])
        seed.write_text(json.dumps(resumed))
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        recovered = self.directory / "durably-recovered"
        result = self.helper("prepare", "--manifest", seed, "--output", recovered)
        self.assertEqual(result.returncode, 0, result.stderr)
        for name, content in bundles.items():
            self.assertEqual((recovered / name).read_bytes(), content, "Recovery must reuse exact verified signature bytes")
        self.assertEqual([call for call in self.fixture.calls() if call["mutation"]], writes_before,
                         "Recovery must not rebuild, republish, re-sign or overwrite candidate bytes")

    def test_prepared_checkpoint_can_finalize_after_runtime_without_overwriting_checkpoint_bytes(self):
        manifest_path, manifest = self.prepare()
        checkpoint = self.fixture.state["releases"].get(manifest["tag"])
        self.assertIsNotNone(checkpoint, "Candidate must have a durable early draft")
        before = {entry["name"]: entry["digest"] for entry in checkpoint["assets"]}
        self.fixture.state["runtime_payload"] = measured_runtime(manifest, manifest_path.parent)
        manifest["runtime_evidence"] = {"run_id": 900, "run_attempt": 1, "artifact_id": 901}
        manifest_path.write_text(json.dumps(manifest))
        self.fixture.save()
        result = subprocess.run([sys.executable, str(self.fixture.source / "tools/release.py"), "chart", "--manifest", str(manifest_path), "--phase", "publish"],
                                env=self.fixture.env, capture_output=True, text=True, timeout=45)
        self.assertEqual(result.returncode, 0, "An early checkpoint must support final measured acceptance: " + result.stderr)
        self.assertTrue(json.loads(result.stdout)["completed"])
        state = json.loads(self.fixture.state_path.read_text())
        final = {entry["name"]: entry["digest"] for entry in state["releases"][manifest["tag"]]["assets"]}
        self.assertEqual({name: final.get(name) for name in before}, before, "Finalization overwrote durable checkpoint bytes")
        self.assertFalse(any("--clobber" in call["args"] for call in self.fixture.calls()))


class NativePlatformTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-platform-check-")
        self.addCleanup(temporary.cleanup)
        self.fixture = PreparationFixture(temporary.name)
        self.reference = IMAGE + ":0.1.0@" + INDEX

    def check(self):
        self.fixture.save()
        return subprocess.run([sys.executable, str(ROOT / "tools/platform_check.py"), "--image", self.reference,
                               "--version", "0.1.0", "--source", IMAGE_SHA], env=self.fixture.env,
                              capture_output=True, text=True, timeout=15)

    def test_native_execution_uses_exact_requested_digest_and_nonroot_metadata(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence["image"], self.reference)
        self.assertEqual(evidence["source_sha"], IMAGE_SHA)
        self.assertEqual(evidence["user"], "65532:65532")
        launched = [call for call in self.fixture.calls() if call["tool"] == "docker" and call["args"][0] == "run"]
        self.assertEqual(len(launched), 1)
        self.assertIn(self.reference, launched[0]["args"])
        self.assertIn("--network=none", launched[0]["args"])
        self.assertNotIn("probes", evidence, "CLI startup cannot claim live probe evidence")

    def test_wrong_native_architecture_cannot_be_reported_as_supported(self):
        import platform
        architecture = "amd64" if platform.machine() in ("arm64", "aarch64") else "arm64"
        self.fixture.state["docker_inspect"] = {"Architecture": architecture, "Os": "linux", "Config": {"User": "65532:65532", "Labels": {}}}
        result = self.check()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(call["tool"] == "docker" and call["args"][0] == "run" for call in self.fixture.calls()))

    def test_failed_or_wrong_manager_entrypoint_is_not_startup_success(self):
        self.fixture.state["docker_help"] = "unrelated executable"
        result = self.check()
        self.assertNotEqual(result.returncode, 0)
        self.fixture.state["docker_run_failure"] = True
        result = self.check()
        self.assertNotEqual(result.returncode, 0)


class InstalledObservationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-release-observe-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.fixture = PreparationFixture(self.directory / "fixture")
        self.kubeconfig = self.directory / "kubeconfig"
        self.kubeconfig.write_text("fixture kubeconfig, never sent to a real cluster")
        self.context = "kind-epco-e2e-owned-fixture"
        self.output = self.directory / "reports/release-installed.json"
        self.output.parent.mkdir()
        (self.output.parent / "fixture-observed.txt").write_text("server version: 16.15\nclient version: 16.15\n")
        self.fixture.env.update(KUBECONFIG=str(self.kubeconfig), EPCO_E2E_CONTEXT=self.context, EPCO_PG_VERSION="16")
        self.fixture.state.update({
            "kube_pods": {"items": [{"metadata": {"name": "manager", "uid": "pod-uid", "labels": {"control-plane": "controller-manager"}},
                                      "spec": {"nodeName": "owned-node", "containers": [{"image": IMAGE + ":0.1.0@" + INDEX}]},
                                      "status": {"containerStatuses": [{"ready": True, "imageID": "ghcr.io/example@" + AMD64}]}}]},
            "kube_nodes": {"items": [{"metadata": {"name": "owned-node"}, "status": {"nodeInfo": {"architecture": "amd64"}}}]},
            "kube_lease": {"metadata": {"uid": "lease-uid"}, "spec": {"holderIdentity": "manager_instance"}},
        })

    def observe(self):
        self.fixture.save()
        return subprocess.run([sys.executable, str(self.fixture.helper), "observe", "--manifest", str(self.fixture.manifest_path),
                               "--output", str(self.output)], env=self.fixture.env, capture_output=True, text=True, timeout=15)

    def test_observation_uses_only_owned_explicit_context_and_records_exact_platform(self):
        result = self.observe()
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads(self.output.read_text())
        self.assertEqual(evidence["image_source_sha"], IMAGE_SHA)
        self.assertEqual(evidence["observations"][0]["platform_digest"], AMD64)
        self.assertEqual(evidence["observations"][0]["podUID"], "pod-uid")
        self.assertEqual(evidence["leaseUID"], "lease-uid")
        commands = [call for call in self.fixture.calls() if call["tool"] == "kubectl"]
        self.assertEqual(len(commands), 3)
        for call in commands:
            self.assertIn(self.context, call["args"])
            self.assertIn(str(self.kubeconfig), call["args"])
            self.assertIn("--request-timeout=30s", call["args"])
            self.assertIn("get", call["args"])

    def test_nonowned_context_is_rejected_before_cluster_reads(self):
        self.fixture.env["EPCO_E2E_CONTEXT"] = "production"
        result = self.observe()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.fixture.calls(), [])

    def test_unready_wrong_image_or_wrong_architecture_cannot_produce_runtime_report(self):
        original = copy.deepcopy(self.fixture.state["kube_pods"])
        for case in ("not-ready", "wrong-image", "wrong-architecture"):
            with self.subTest(case=case):
                self.fixture.state["kube_pods"] = copy.deepcopy(original)
                pod = self.fixture.state["kube_pods"]["items"][0]
                if case == "not-ready":
                    pod["status"]["containerStatuses"][0]["ready"] = False
                elif case == "wrong-image":
                    pod["spec"]["containers"][0]["image"] = IMAGE + ":latest"
                else:
                    pod["status"]["containerStatuses"][0]["imageID"] = "ghcr.io/example@" + ARM64
                result = self.observe()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
