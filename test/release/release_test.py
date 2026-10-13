"""Release requirements exercised through the production CLI and tool boundary."""

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from helper_support import PreparationFixture, measured_runtime
from support import (
    AMD64, ARM64, CHART, CHART_DIGEST, CHART_SHA, GATES, IMAGE, IMAGE_SHA,
    INDEX, ISSUER, ReleaseFixture,
)


ROOT = Path(__file__).resolve().parents[2]
CLI = ROOT / "tools" / "release.py"


class ReleaseBehaviorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-release-tests-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.fixture = ReleaseFixture(self.directory / "chart")

    def image_fixture(self):
        self.fixture = ReleaseFixture(self.directory / "image", "image")

    def invoke(self, phase="validate", seed=False):
        self.assertTrue(CLI.is_file(), f"Missing production release behavior: {CLI}")
        self.fixture.save()
        path = self.fixture.manifest_path
        if seed:
            manifest = {key: self.fixture.manifest[key] for key in ("schema_version", "stream", "repository", "tag", "source_sha")}
            if self.fixture.stream == "chart":
                manifest["image"] = {key: self.fixture.manifest["image"][key] for key in ("repository", "version", "release_tag")}
            path = self.fixture.root / "seed.json"
            path.write_text(json.dumps(manifest))
        return subprocess.run(
            [sys.executable, str(CLI), self.fixture.stream, "--manifest", str(path), "--phase", phase],
            cwd=self.fixture.root, env=self.fixture.env, text=True, capture_output=True, timeout=15,
        )

    def accepted(self, phase="validate", seed=False):
        result = self.invoke(phase, seed)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        try:
            return json.loads(result.stdout)
        except json.JSONDecodeError:
            self.fail("Release success must return JSON: " + result.stdout + result.stderr)

    def rejected(self, phase="validate", seed=False, no_writes=True):
        result = self.invoke(phase, seed)
        self.assertNotEqual(result.returncode, 0, "Unsafe release accepted: " + result.stdout)
        self.assertTrue((result.stdout + result.stderr).strip(), "Missing rejection diagnostic")
        if no_writes:
            self.assert_write_free()
        return result

    def assert_write_free(self):
        self.assertFalse([call for call in self.fixture.calls() if call["mutation"]], self.fixture.calls())

    def assert_no_image_writes(self):
        forbidden = []
        for call in self.fixture.calls():
            joined = " ".join(call["args"])
            if call["tool"] == "docker" or (call["mutation"] and IMAGE in joined):
                forbidden.append(call)
        self.assertFalse(forbidden, "Chart-only release mutated the application: " + repr(forbidden))

    def own_immutable_release(self):
        self.fixture.state["releases"][self.fixture.manifest["tag"]] = self.fixture.release(
            self.fixture.manifest, self.fixture.manifest_path.parent)

    def before_new_version(self):
        reference = CHART + ":0.2.1" if self.fixture.stream == "chart" else IMAGE + ":0.1.0"
        self.fixture.state["versions"].pop(reference, None)

    def packaging_only_ancestor(self, files=None):
        self.before_new_version()
        ancestor = "3" * 40
        for run in self.fixture.state["runs"]:
            if run["path"] in (".github/workflows/test.yml", ".github/workflows/test-e2e.yml"):
                run["head_sha"] = ancestor
        if files is None:
            files = ["tools/release.py", "test/release/release_test.py", "charts/external-postgres-control-operator/Chart.yaml",
                     "docs/roadmap.md", ".github/workflows/release-image.yml", ".github/workflows/test.yml"]
        comparison = {"status": "ahead", "merge_base_commit": {"sha": ancestor}, "ahead_by": 1, "behind_by": 0,
                      "total_commits": 1, "files": [{"filename": path, "status": "modified"} for path in files]}
        self.fixture.state["comparisons"] = {ancestor + "..." + self.fixture.manifest["source_sha"]: comparison}
        return ancestor, comparison

    def prepared_image_checkpoint(self):
        self.fixture = PreparationFixture(self.directory / "prepared-image", "image")
        seed = self.fixture.seed_path()
        self.fixture.save()
        result = subprocess.run(
            [sys.executable, str(self.fixture.helper), "prepare", "--manifest", str(seed),
             "--output", str(self.fixture.output)],
            cwd=self.fixture.source, env=self.fixture.env, text=True, capture_output=True, timeout=45,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        prepared = self.fixture.output / "manifest.json"
        self.assertTrue(prepared.is_file())
        return prepared, json.loads(prepared.read_text())

    def test_independent_chart_patch_keeps_original_application(self):
        output = self.accepted()
        self.assertEqual(output["chart"]["version"], "0.2.1")
        self.assertEqual(output["chart"]["app_version"], "0.1.0")
        self.assertEqual(output["image"]["index_digest"], INDEX)
        self.assertEqual(output["image"]["source_sha"], IMAGE_SHA)
        self.assertEqual(output["source_sha"], CHART_SHA)
        self.assertEqual(self.fixture.calls(), [], "validate must be pure")

    def test_application_validation_is_pure(self):
        self.image_fixture()
        output = self.accepted()
        self.assertEqual(output["image"]["version"], "0.1.0")
        self.assertEqual(self.fixture.calls(), [])

    def test_strict_release_versions_and_stream_tags(self):
        baseline = copy.deepcopy(self.fixture.manifest)
        for tag in ("v0.2.1", "chart-v01.2.1", "chart-v0.2", "chart-v0.2.1+build", "chart-v0.2.1-01", "chart-v0.2.1-rc..1", "chart-v0.2.1-", "chart-v0.2.1\n", "chart-v0.2.1;echo bad"):
            with self.subTest(tag=tag):
                self.fixture.manifest = copy.deepcopy(baseline)
                self.fixture.manifest["tag"] = tag
                self.rejected()

    def test_image_stream_rejects_chart_tag(self):
        self.image_fixture()
        self.fixture.manifest["tag"] = "chart-v0.1.0"
        self.rejected()

    def test_full_semver_prerelease_is_supported(self):
        self.fixture = ReleaseFixture(self.directory / "prerelease", chart_version="0.2.1-rc.2")
        output = self.accepted()
        self.assertTrue(output["prerelease"])
        self.assertFalse(output["make_latest"])

    def test_chart_version_mapping_cannot_be_inferred_from_app_version(self):
        self.fixture.manifest["chart"]["version"] = "0.1.0"
        self.rejected()

    def test_invalid_source_and_registry_coordinates_are_rejected(self):
        for field, value in (("source_sha", "main"), ("repository", "attacker/project")):
            with self.subTest(field=field):
                original = self.fixture.manifest[field]
                self.fixture.manifest[field] = value
                self.rejected()
                self.fixture.manifest[field] = original
        self.fixture.manifest["chart"]["repository"] = IMAGE
        self.rejected()

    def test_local_asset_checksums_are_authoritative(self):
        (self.fixture.manifest_path.parent / self.fixture.manifest["chart"]["archive"]).write_bytes(b"changed archive")
        self.rejected()

    def test_checksum_inventory_cannot_omit_content_or_include_itself(self):
        sums = self.fixture.manifest_path.parent / "SHA256SUMS"
        original = sums.read_text()
        for content in ("", original.splitlines()[0] + "\n", original + "0" * 64 + "  SHA256SUMS\n"):
            with self.subTest(content=content):
                sums.write_text(content)
                self.rejected()

    def test_assets_cannot_escape_manifest_directory(self):
        self.fixture.manifest["assets"][0]["path"] = "../outside.yaml"
        self.rejected()

    def test_missing_subject_bundle_is_rejected(self):
        (self.fixture.manifest_path.parent / "chart.sigstore.json").unlink()
        self.rejected()

    def test_wrong_generated_source_is_rejected(self):
        self.fixture.manifest["image"]["generated_inputs"][0]["source_sha"] = CHART_SHA
        self.rejected()

    def test_preflight_requires_only_seed_and_returns_original_image_proof(self):
        self.before_new_version()
        output = self.accepted("preflight", seed=True)
        self.assertFalse(output["completed"])
        self.assertEqual(output["image"]["source_sha"], IMAGE_SHA)
        self.assertEqual(output["image"]["index_digest"], INDEX)
        self.assert_write_free()

    def test_image_preflight_does_not_wait_for_chart_package(self):
        self.image_fixture()
        self.before_new_version()
        self.fixture.state["private"] = [CHART]
        self.fixture.fail("crane", [CHART], "chart package does not exist")
        self.accepted("preflight", seed=True)
        self.assertFalse([call for call in self.fixture.calls() if CHART in " ".join(call["args"])])
        self.assert_write_free()

    def test_caller_cannot_disable_trusted_gates(self):
        self.before_new_version()
        self.fixture.manifest["required_workflows"] = {}
        self.fixture.state["runs"] = []
        self.rejected("preflight")

    def test_unrelated_green_workflow_does_not_satisfy_missing_gate(self):
        self.before_new_version()
        self.fixture.state["runs"][0]["path"] = ".github/workflows/untrusted.yml"
        self.rejected("preflight", seed=True)

    def test_exact_source_push_main_is_required(self):
        self.before_new_version()
        baseline = copy.deepcopy(self.fixture.state["runs"][0])
        for field, value in (("head_sha", IMAGE_SHA), ("event", "pull_request"), ("head_branch", "feature"), ("head_repository", {"full_name": "attacker/fork"})):
            with self.subTest(field=field):
                self.fixture.state["runs"][0] = {**baseline, field: value}
                self.rejected("preflight", seed=True)

    def test_latest_failed_run_cannot_reuse_older_success(self):
        self.before_new_version()
        latest = {**self.fixture.state["runs"][0], "id": 500, "conclusion": "failure", "created_at": "2026-10-12T01:00:00Z"}
        self.fixture.state["runs"].append(latest)
        self.fixture.state["jobs"]["500"] = [{"name": name, "status": "completed", "conclusion": "failure"} for name in GATES["test.yml"]]
        self.rejected("preflight", seed=True)

    def test_packaging_only_ancestor_source_gates_reuse_green_main(self):
        ancestor, _ = self.packaging_only_ancestor()
        output = self.accepted("preflight", seed=True)
        self.assertFalse(output["completed"])
        self.assertTrue(any(call["tool"] == "gh" and ancestor + "..." + CHART_SHA in " ".join(call["args"])
                            for call in self.fixture.calls()), "Reuse must verify the complete ancestor-to-source diff")
        self.assert_write_free()

    def test_fixture_runner_change_reuses_packaging_ancestor(self):
        self.packaging_only_ancestor(files=["test/e2e/run.sh", "test/e2e/runner_test.sh"])
        self.assertFalse(self.accepted("preflight", seed=True)["completed"])
        self.assert_write_free()

    def test_cancelled_or_skipped_exact_gate_can_reuse_packaging_ancestor(self):
        for outcome in ("cancelled", "skipped"):
            with self.subTest(outcome=outcome):
                self.fixture = ReleaseFixture(self.directory / ("packaging-" + outcome))
                self.packaging_only_ancestor()
                exact = {**self.fixture.state["runs"][0], "id": 500, "head_sha": CHART_SHA,
                         "created_at": "2026-10-12T01:00:00Z", "status": "completed", "conclusion": outcome}
                self.fixture.state["runs"].append(exact)
                self.assertFalse(self.accepted("preflight", seed=True)["completed"])
                self.assert_write_free()

    def test_failed_exact_gate_cannot_be_masked_by_packaging_ancestor(self):
        self.packaging_only_ancestor()
        exact = {**self.fixture.state["runs"][0], "id": 500, "head_sha": CHART_SHA,
                 "created_at": "2026-10-12T01:00:00Z", "status": "completed", "conclusion": "failure"}
        self.fixture.state["runs"].append(exact)
        self.rejected("preflight", seed=True)

    def test_runtime_or_renamed_runtime_file_requires_exact_source_gates(self):
        for index, files in enumerate(([{"filename": "internal/controller/database_controller.go", "status": "modified"}],
                                       [{"filename": "test/e2e/r6_applications_test.go", "status": "modified"}],
                                       [{"filename": "docs/renamed.md", "previous_filename": "test/e2e/r6_applications_test.go", "status": "renamed"}])):
            with self.subTest(files=files):
                self.fixture = ReleaseFixture(self.directory / f"unsafe-diff-{index}")
                _, comparison = self.packaging_only_ancestor()
                comparison["files"] = files
                self.rejected("preflight", seed=True)

    def test_ancestor_run_and_current_attempt_jobs_must_be_trusted(self):
        for defect in ("fork", "wrong_branch", "failed_job", "wrong_attempt"):
            with self.subTest(defect=defect):
                self.fixture = ReleaseFixture(self.directory / ("untrusted-" + defect))
                self.packaging_only_ancestor()
                run = self.fixture.state["runs"][0]
                if defect == "fork":
                    run["head_repository"] = {"full_name": "attacker/fork"}
                elif defect == "wrong_branch":
                    run["head_branch"] = "feature"
                elif defect == "failed_job":
                    self.fixture.state["jobs"]["100"][0]["conclusion"] = "failure"
                else:
                    run["run_attempt"] = 2
                    self.fixture.state["jobs_by_attempt"] = {"100:1": copy.deepcopy(self.fixture.state["jobs"]["100"]),
                                                             "100:2": [{"name": "test-unit", "status": "completed", "conclusion": "failure"}]}
                self.rejected("preflight", seed=True)

    def test_ancestor_comparison_must_be_complete_bounded_and_unambiguous(self):
        for defect in ("missing_files", "empty_files", "duplicate_file", "truncated", "diverged", "wrong_merge_base"):
            with self.subTest(defect=defect):
                self.fixture = ReleaseFixture(self.directory / ("bad-comparison-" + defect))
                _, comparison = self.packaging_only_ancestor()
                if defect == "missing_files":
                    comparison.pop("files")
                elif defect == "empty_files":
                    comparison["files"] = []
                elif defect == "duplicate_file":
                    comparison["files"].append(copy.deepcopy(comparison["files"][0]))
                elif defect == "truncated":
                    comparison["files"] = [{"filename": f"docs/change-{index}.md", "status": "modified"}
                                           for index in range(300)]
                elif defect == "diverged":
                    comparison["status"] = "diverged"
                else:
                    comparison["merge_base_commit"]["sha"] = "f" * 40
                self.rejected("preflight", seed=True)

    def test_current_attempt_failure_cannot_reuse_old_attempt(self):
        self.before_new_version()
        old_jobs = copy.deepcopy(self.fixture.state["jobs"]["100"])
        self.fixture.state["runs"][0]["run_attempt"] = 2
        self.fixture.state["jobs"]["100"][0]["conclusion"] = "failure"
        self.fixture.state["jobs_by_attempt"] = {"100:1": old_jobs, "100:2": self.fixture.state["jobs"]["100"]}
        self.rejected("preflight", seed=True)

    def test_required_job_cannot_be_missing_skipped_pending_or_failed(self):
        self.before_new_version()
        original = copy.deepcopy(self.fixture.state["jobs"]["100"])
        for outcome in ("missing", "skipped", "failure", "cancelled", "in_progress"):
            with self.subTest(outcome=outcome):
                self.fixture.state["jobs"]["100"] = copy.deepcopy(original)
                if outcome == "missing":
                    self.fixture.state["jobs"]["100"].pop()
                else:
                    job = self.fixture.state["jobs"]["100"][0]
                    job["conclusion"] = outcome if outcome != "in_progress" else None
                    job["status"] = "in_progress" if outcome == "in_progress" else "completed"
                self.rejected("preflight", seed=True)

    def test_tag_must_still_resolve_to_source_on_main(self):
        self.before_new_version()
        self.fixture.state["tag_sha"] = "f" * 40
        self.rejected("preflight", seed=True)
        self.fixture.state["tag_sha"] = CHART_SHA
        self.fixture.state["on_main"] = False
        self.rejected("preflight", seed=True)

    def test_api_permission_failure_is_not_absence(self):
        self.before_new_version()
        for message in ("gh: Resource not accessible by integration (HTTP 403)",
                        "gh: Requires authentication (HTTP 401)", "DENIED: requested access to the resource is denied",
                        "gh: Service Unavailable (HTTP 503)", "error connecting to api.github.com"):
            with self.subTest(error=message):
                self.fixture.state["failures"] = []
                self.fixture.fail("gh", ["api", "releases/tags/chart-v0.2.1"], message)
                self.rejected("preflight", seed=True)

    def test_actual_gh_404_is_absence_for_new_release_preflight_in_both_streams(self):
        for stream in ("image", "chart"):
            with self.subTest(stream=stream):
                self.fixture = ReleaseFixture(self.directory / (stream + "-gh404"), stream)
                self.before_new_version()
                output = self.accepted("preflight", seed=True)
                self.assertFalse(output["completed"])
                self.assertEqual(output["stream"], stream)
                self.assert_write_free()

    def test_actual_gh_404_for_required_source_lookup_remains_failure(self):
        self.image_fixture()
        self.before_new_version()
        # Required main ancestry is not an optional resource. The same real gh
        # error must fail here even when the first-release lookup was absent.
        self.fixture.fail("gh", ["api", "compare/"], "gh: Not Found (HTTP 404)")
        result = self.rejected("preflight", seed=True)
        self.assertIn("compare/", result.stderr, "Failure must come from the required source lookup, not the optional release")

    def test_hidden_draft_is_found_by_exact_tag_on_later_release_page(self):
        self.image_fixture()
        tag = self.fixture.manifest["tag"]
        draft = self.fixture.release(self.fixture.manifest, self.fixture.manifest_path.parent,
                                     immutable=False, draft=True)
        unrelated = {}
        for index in range(100):
            other = "v0.1.0-rc.1" if index == 0 else f"v9.0.{index}"
            unrelated[other] = {"id": 1000 + index, "tag_name": other, "draft": False,
                                "immutable": True, "assets": []}
        self.fixture.state["releases"] = {**unrelated, tag: draft}
        output = self.accepted("preflight", seed=True)
        self.assertFalse(output["completed"])
        self.assertEqual(output["tag"], tag)
        self.assertTrue(any(call["tool"] == "gh" and any("/releases?" in arg for arg in call["args"])
                            for call in self.fixture.calls()), "A draft hidden by the tag endpoint needs the release list")
        self.assert_write_free()

    def test_duplicate_exact_draft_tags_in_release_list_fail_closed(self):
        self.image_fixture()
        tag = self.fixture.manifest["tag"]
        draft = self.fixture.release(self.fixture.manifest, self.fixture.manifest_path.parent,
                                     immutable=False, draft=True)
        self.fixture.state["releases"] = {tag: draft, "duplicate-list-record": {**draft, "id": 799}}
        result = self.rejected("preflight", seed=True)
        self.assertRegex(result.stderr.lower(), r"ambig|duplicate|multiple|non-unique")

    def test_release_list_authentication_and_transport_failures_are_not_absence(self):
        for index, message in enumerate(("gh: Requires authentication (HTTP 401)",
                                         "gh: Resource not accessible by integration (HTTP 403)",
                                         "gh: Service Unavailable (HTTP 503)",
                                         "error connecting to api.github.com")):
            with self.subTest(error=message):
                self.fixture = ReleaseFixture(self.directory / f"list-failure-{index}", "image")
                self.before_new_version()
                self.fixture.state["release_list_failure"] = message
                result = self.rejected("preflight", seed=True)
                self.assertIn(message, result.stderr)

    def test_hidden_checkpoint_draft_recovers_and_publishes_same_release(self):
        prepared_path, prepared = self.prepared_image_checkpoint()
        tag = prepared["tag"]
        draft = self.fixture.state["releases"][tag]
        self.assertTrue(draft["draft"])
        self.assertEqual({entry["name"] for entry in draft["assets"]},
                         {"release-candidate.tar.gz", "release-candidate.sigstore.json"})
        original_id = draft["id"]
        checkpoint_digests = {entry["name"]: entry["digest"] for entry in draft["assets"]}
        before_preflight = len(self.fixture.calls())
        recovered = self.accepted("preflight", seed=True)
        self.assertFalse(recovered["completed"])
        self.assertEqual(recovered["image"], prepared["image"])
        self.assertFalse([call for call in self.fixture.calls()[before_preflight:] if call["mutation"]],
                         "Preflight must recover the signed candidate without new writes")

        self.fixture.state = json.loads(self.fixture.state_path.read_text())
        self.fixture.manifest_path = prepared_path
        self.fixture.manifest = prepared
        self.fixture.manifest["runtime_evidence"] = {"run_id": 900, "run_attempt": 1, "artifact_id": 901}
        self.fixture.state["runtime_payload"] = measured_runtime(prepared, prepared_path.parent)
        before_publish = len(self.fixture.calls())
        output = self.accepted("publish")
        self.assertTrue(output["completed"])
        state = json.loads(self.fixture.state_path.read_text())
        published = state["releases"][tag]
        self.assertEqual(published["id"], original_id, "Publication must reuse the one prepared draft")
        self.assertFalse(published["draft"])
        final_digests = {entry["name"]: entry["digest"] for entry in published["assets"]}
        self.assertEqual({name: final_digests.get(name) for name in checkpoint_digests}, checkpoint_digests)
        new_calls = self.fixture.calls()[before_publish:]
        self.assertFalse([call for call in new_calls if call["tool"] == "gh" and call["args"][:2] == ["release", "create"]],
                         "Publication must not create another draft")
        self.assertFalse([call for call in new_calls if call["tool"] == "cosign" and call["args"][0] == "sign"],
                         "Checkpoint recovery must not re-sign the image")
        self.assertFalse([call for call in new_calls if call["tool"] == "docker" and call["mutation"]],
                         "Checkpoint recovery must not rebuild the image")

    def test_chart_requires_original_immutable_image_release(self):
        self.before_new_version()
        image = self.fixture.state["releases"]["v0.1.0"]
        image["immutable"] = False
        self.rejected("preflight", seed=True)

    def test_chart_cannot_substitute_original_image_source(self):
        self.fixture.manifest["image"]["source_sha"] = CHART_SHA
        self.rejected("verify")

    def test_chart_reuses_verified_original_generated_inputs(self):
        directory = Path(self.fixture.state["releases"]["v0.1.0"]["_directory"])
        (directory / "application-rbac.yaml").write_bytes(b"new unverified RBAC")
        self.rejected("verify")

    def test_chart_verifies_original_image_and_new_chart_identities(self):
        self.accepted("verify")
        verification = [call for call in self.fixture.calls() if call["tool"] == "cosign" and call["args"][0] == "verify"]
        for reference, source, workflow, tag in ((IMAGE + "@" + INDEX, IMAGE_SHA, "release-image.yml", "v0.1.0"), (CHART + "@" + CHART_DIGEST, CHART_SHA, "release-chart.yml", "chart-v0.2.1")):
            calls = [call for call in verification if reference in call["args"]]
            self.assertTrue(calls, reference)
            arguments = " ".join(calls[0]["args"])
            for expected in (ISSUER, source, f"{workflow}@refs/tags/{tag}", "--certificate-github-workflow-trigger", "push"):
                self.assertIn(expected, arguments)
        self.assert_no_image_writes()

    def test_invalid_original_image_signature_blocks_chart_without_repair(self):
        self.fixture.fail("cosign", ["verify", IMAGE + "@" + INDEX], "No signatures found")
        self.rejected("publish")
        self.assert_no_image_writes()

    def test_wrong_issuer_workflow_sha_and_subject_signature_fail_closed(self):
        subject = IMAGE + "@" + INDEX
        original = copy.deepcopy(self.fixture.state["subjects"][subject])
        for field, value in (("issuer", "https://attacker.example"), ("identity", "https://github.com/attacker/fork/.github/workflows/release-image.yml@refs/tags/v0.1.0"), ("source_sha", CHART_SHA), ("ref", "refs/heads/main")):
            with self.subTest(field=field):
                self.fixture.state["subjects"][subject] = {**original, field: value}
                self.rejected("verify")
        self.fixture.state["subjects"].pop(subject)
        self.rejected("verify")

    def test_platform_subjects_must_belong_to_signed_index(self):
        self.fixture.state["image_manifest"]["manifests"][1]["digest"] = "sha256:" + "f" * 64
        self.rejected("verify")

    def test_attestation_manifest_is_not_a_runnable_platform(self):
        self.accepted("verify")
        calls = self.fixture.calls()
        for digest in (INDEX, AMD64, ARM64):
            self.assertTrue(any(call["tool"] == "cosign" and IMAGE + "@" + digest in call["args"] for call in calls))
        self.assertFalse(any(IMAGE + "@sha256:" + "e" * 64 in call["args"] for call in calls))

    def test_real_tool_failure_is_not_successful_proof(self):
        for tool, contains in (("cosign", ["verify-blob"]), ("gh", ["release", "verify"]), ("crane", ["manifest"])):
            with self.subTest(tool=tool):
                self.fixture.state["failures"] = []
                self.fixture.fail(tool, contains, "transport or verification failed", 23)
                self.rejected("verify")

    def test_unverified_runtime_boolean_cannot_authorize_publication(self):
        self.fixture.manifest["runtime_evidence"] = {"passed": True}
        self.rejected("publish")

    def test_runtime_proof_must_match_release_source_subject_and_owner(self):
        original = copy.deepcopy(self.fixture.state["runtime_payload"])
        for field, value in (("source_sha", IMAGE_SHA), ("image_source_sha", CHART_SHA), ("image_index_digest", "sha256:" + "f" * 64), ("chart_digest", INDEX)):
            with self.subTest(field=field):
                self.fixture.state["runtime_payload"] = {**original, field: value}
                self.rejected("publish")
        self.fixture.state["runtime_payload"] = original
        self.fixture.state["runtime_artifact"]["workflow_run"]["id"] = 999
        self.rejected("publish")

    def test_runtime_job_failure_blocks_publication(self):
        self.fixture.state["jobs"]["900"][0]["conclusion"] = "failure"
        self.rejected("publish")

    def test_chart_publish_signs_only_chart_and_checksums_then_publishes_complete_draft(self):
        output = self.accepted("publish")
        self.assertTrue(output["completed"])
        calls = self.fixture.calls()
        self.assert_no_image_writes()
        self.assertTrue(any(call["tool"] == "cosign" and call["args"][0] == "sign" and CHART + "@" + CHART_DIGEST in call["args"] for call in calls))
        self.assertTrue(any(call["tool"] == "cosign" and call["args"][0] == "sign-blob" for call in calls))
        publication = next(index for index, call in enumerate(calls) if call["tool"] == "gh" and call["args"][:2] == ["release", "edit"])
        self.assertTrue(any(call["tool"] == "gh" and call["args"][:2] == ["release", "download"] and "chart-v0.2.1" in call["args"] for call in calls[:publication]))
        self.assertTrue(any(call["tool"] == "cosign" and call["args"][0] == "verify-blob" for call in calls[:publication]))
        self.assertFalse([call for call in calls[publication + 1:] if call["mutation"]])
        state = json.loads(self.fixture.state_path.read_text())
        required = {asset["path"] for asset in self.fixture.manifest["assets"]} | {"SHA256SUMS", "SHA256SUMS.sigstore.json"}
        self.assertTrue(required.issubset(set(state["uploads"])), state["uploads"])
        edits = calls[publication]["args"]
        self.assertIn("--latest=false", edits)
        self.assertIn("--prerelease=false", edits)

    def test_stable_image_publish_uses_latest_boolean_flag(self):
        self.image_fixture()
        output = self.accepted("publish")
        self.assertTrue(output["completed"])
        self.assertTrue(output["make_latest"])
        edits = [call["args"] for call in self.fixture.calls()
                 if call["tool"] == "gh" and call["args"][:2] == ["release", "edit"]]
        self.assertEqual(len(edits), 1)
        self.assertIn("--latest=true", edits[0])
        self.assertIn("--prerelease=false", edits[0])

    def test_prerelease_chart_publish_does_not_mark_latest(self):
        self.fixture = ReleaseFixture(self.directory / "prerelease-publish", chart_version="0.2.1-rc.2")
        output = self.accepted("publish")
        self.assertTrue(output["completed"])
        self.assertTrue(output["prerelease"])
        self.assertFalse(output["make_latest"])
        edits = [call["args"] for call in self.fixture.calls()
                 if call["tool"] == "gh" and call["args"][:2] == ["release", "edit"]]
        self.assertEqual(len(edits), 1)
        self.assertIn("--latest=false", edits[0])
        self.assertIn("--prerelease=true", edits[0])

    def test_chart_only_partial_retry_never_re_signs_existing_image(self):
        self.fixture.state["releases"][self.fixture.manifest["tag"]] = self.fixture.release(
            self.fixture.manifest, self.fixture.manifest_path.parent, immutable=False, draft=True)
        self.accepted("publish")
        self.assert_no_image_writes()

    def test_publication_requires_anonymous_chart_and_image_proof(self):
        self.accepted("publish")
        for repository in (IMAGE, CHART):
            self.assertTrue(any(call["anonymous"] and call["tool"] in ("crane", "cosign") and repository in " ".join(call["args"]) for call in self.fixture.calls()), repository)

    def test_private_chart_cannot_publish_github_release(self):
        self.fixture.state["private"] = [CHART]
        self.rejected("publish", no_writes=False)
        self.assertFalse(any(call["tool"] == "gh" and call["args"][:2] == ["release", "edit"] for call in self.fixture.calls()))
        self.assert_no_image_writes()

    def test_published_immutable_recovery_is_read_only_before_preflight_build(self):
        self.own_immutable_release()
        output = self.accepted("preflight", seed=True)
        self.assertTrue(output["completed"])
        self.assert_write_free()
        self.assertFalse(any(call["tool"] == "gh" and any("/releases?" in arg for arg in call["args"])
                             for call in self.fixture.calls()), "Published release should resolve through the by-tag API")

    def test_published_immutable_rerun_is_read_only(self):
        self.own_immutable_release()
        output = self.accepted("publish")
        self.assertTrue(output["completed"])
        self.assert_write_free()

    def test_bad_locked_release_cannot_be_repaired(self):
        self.own_immutable_release()
        self.fixture.fail("gh", ["release", "verify-asset"], "asset attestation failed")
        self.rejected("publish")

    def test_published_mutable_release_is_rejected(self):
        self.own_immutable_release()
        self.fixture.state["releases"][self.fixture.manifest["tag"]]["immutable"] = False
        self.rejected("preflight", seed=True)

    def test_recovery_must_not_rebuild_lost_or_mismatched_subject(self):
        self.fixture.state["versions"][CHART + ":0.2.1"] = INDEX
        self.rejected("publish")
        self.assert_no_image_writes()


if __name__ == "__main__":
    unittest.main()
