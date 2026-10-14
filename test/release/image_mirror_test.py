"""Offline behavioral contracts for immutable multi-platform fixture mirrors."""

import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "tools/image_mirror.py"
REPOSITORY = "ghcr.io/vterdunov/external-postgres-control-operator"
INDEX = "sha256:" + "a" * 64
OTHER = "sha256:" + "b" * 64
AMD64 = "sha256:" + "c" * 64
ARM64 = "sha256:" + "d" * 64

CRANE = r'''import json
import os
from pathlib import Path
import signal
import sys

path = Path(os.environ["MIRROR_FIXTURE_STATE"])
state = json.loads(path.read_text())
args = sys.argv[1:]
state["commands"].append(args)
path.write_text(json.dumps(state))
if not args:
    raise SystemExit("missing crane operation")
if args[0] == "digest":
    reference = args[-1]
    error = state.get("errors", {}).get(reference)
    if error and reference not in state.get("copied_destinations", []):
        print(error, file=sys.stderr)
        raise SystemExit(17)
    if reference not in state["images"]:
        print("MANIFEST_UNKNOWN: manifest unknown", file=sys.stderr)
        raise SystemExit(18)
    print(state["images"][reference])
elif args[0] == "copy":
    source, destination = args[-2:]
    if state.get("cancel_copy"):
        os.kill(os.getppid(), signal.SIGTERM)
        raise SystemExit(19)
    if state.get("copy_error"):
        print(state["copy_error"], file=sys.stderr)
        raise SystemExit(23)
    digest = state["images"][source]
    if any(argument.startswith("--platform") for argument in args):
        digest = state["platforms"]["linux/amd64"]
    state["images"][destination] = state.get("copied_digest", digest)
    state.setdefault("copied_destinations", []).append(destination)
    state["copied_platforms"] = state["platforms"]
    path.write_text(json.dumps(state))
else:
    raise SystemExit("unexpected crane operation: " + args[0])
'''


class ImageMirrorBehaviorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-image-mirror-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.catalog = self.root / "catalog.json"
        self.state_path = self.root / "state.json"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        crane = self.bin / "crane"
        crane.write_text(f"#!{sys.executable}\n" + CRANE)
        crane.chmod(0o755)
        self.entry = {"name": "postgres-18", "source": "docker.io/library/postgres:18.6-bookworm@" + INDEX,
                      "destination": REPOSITORY + ":mirror-postgres-18.6-bookworm"}
        self.entries = [self.entry]
        self.state = {"images": {self.entry["source"]: INDEX}, "commands": [],
                      "platforms": {"linux/amd64": AMD64, "linux/arm64": ARM64}}

    def invoke(self, command="sync", name=None):
        self.catalog.write_text(json.dumps({"images": self.entries}))
        self.state_path.write_text(json.dumps(self.state))
        env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                   MIRROR_FIXTURE_STATE=str(self.state_path))
        arguments = [sys.executable, str(HELPER), command, "--catalog", str(self.catalog)]
        if name is not None:
            arguments.append(name)
        result = subprocess.run(arguments, env=env, capture_output=True, text=True, timeout=5)
        self.state = json.loads(self.state_path.read_text())
        return result

    def copies(self):
        return [command for command in self.state["commands"] if command[0] == "copy"]

    def reject_without_copy(self):
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.copies(), [], "Failed inspection must never authorize registry mutation")
        return result

    def test_missing_destination_copies_and_verifies_the_entire_index(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.copies()), 1, "A missing pinned image must be mirrored")
        self.assertEqual(self.copies()[0][-2:], [self.entry["source"], self.entry["destination"]])
        self.assertEqual(self.state["images"][self.entry["destination"]], INDEX)
        self.assertEqual(self.state["copied_platforms"], {"linux/amd64": AMD64, "linux/arm64": ARM64})
        self.assertFalse(any(arg.startswith("--platform") for arg in self.copies()[0]),
                         "The mirror must retain the full multi-platform index")
        commands = self.state["commands"]
        copy_index = commands.index(self.copies()[0])
        self.assertTrue(any(command[0] == "digest" and command[-1] == self.entry["source"]
                            for command in commands[:copy_index]), "Verify the source before copying")
        self.assertTrue(any(command[0] == "digest" and command[-1] == self.entry["destination"]
                            for command in commands[copy_index + 1:]), "Verify the destination after copying")

    def test_equal_immutable_destination_is_skipped(self):
        self.state["images"][self.entry["destination"]] = INDEX
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.copies(), [])
        self.assertTrue(self.state["commands"], "A skip requires observed registry identity")

    def test_mismatched_immutable_destination_is_never_overwritten(self):
        self.state["images"][self.entry["destination"]] = OTHER
        self.reject_without_copy()
        self.assertEqual(self.state["images"][self.entry["destination"]], OTHER)

    def test_changed_source_digest_is_rejected_before_copy(self):
        self.state["images"][self.entry["source"]] = OTHER
        self.reject_without_copy()

    def test_destination_digest_is_checked_after_copy(self):
        self.state["copied_digest"] = AMD64
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0, "A platform manifest cannot replace the accepted index")
        self.assertEqual(len(self.copies()), 1)

    def test_only_explicit_registry_absence_allows_publication(self):
        for diagnostic in ("DENIED: requested access to the resource is denied", "UNAUTHORIZED: login required",
                           "dial tcp: connection refused", "unexpected EOF", "HTTP 404 proxy route missing",
                           "DENIED: MANIFEST_UNKNOWN appears only in an unauthorized detail"):
            with self.subTest(diagnostic=diagnostic):
                self.state["commands"] = []
                self.state["errors"] = {self.entry["destination"]: diagnostic}
                self.reject_without_copy()

    def test_explicit_name_unknown_is_also_an_absent_destination(self):
        self.state["errors"] = {self.entry["destination"]: "NAME_UNKNOWN: repository name not known"}
        # The fake registry loses its original absence once the copy creates it.
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.copies()), 1, result.stdout + result.stderr)

    def verbose_ghcr_fixture(self):
        self.entry.update(name="postgres-15", source="docker.io/library/postgres:15.19-bookworm@" + INDEX,
                          destination=REPOSITORY + ":mirror-postgres-15.19-bookworm")
        self.state["images"] = {self.entry["source"]: INDEX}
        return (
            "2026/10/14 23:31:10 HEAD request failed, falling back on GET: HEAD "
            "https://ghcr.io/v2/vterdunov/external-postgres-control-operator/manifests/"
            "mirror-postgres-15.19-bookworm: unexpected status code 404 Not Found "
            "(HEAD responses have no body, use GET for details)\n"
            "Error: GET https://ghcr.io/v2/vterdunov/external-postgres-control-operator/manifests/"
            "mirror-postgres-15.19-bookworm: "
        )

    def test_verbose_ghcr_head_fallback_preserves_explicit_manifest_absence(self):
        diagnostic = self.verbose_ghcr_fixture() + "MANIFEST_UNKNOWN: manifest unknown"
        self.state["errors"] = {self.entry["destination"]: diagnostic}
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.copies()), 1, "Verbose HEAD/GET diagnostics must not hide definitive absence")
        self.assertEqual(self.state["images"][self.entry["destination"]], INDEX)

    def test_verbose_ghcr_head_fallback_does_not_hide_authorization_failures(self):
        prefix = self.verbose_ghcr_fixture()
        for rejection in ("DENIED: requested access to the resource is denied",
                          "UNAUTHORIZED: authentication required"):
            with self.subTest(rejection=rejection):
                self.state["commands"] = []
                self.state["errors"] = {self.entry["destination"]: prefix + rejection}
                self.reject_without_copy()

    def test_source_inspection_error_is_never_treated_as_an_absent_destination(self):
        self.state["errors"] = {self.entry["source"]: "MANIFEST_UNKNOWN: source unavailable"}
        self.reject_without_copy()

    def add_second_entry(self):
        second = {"name": "postgres-16", "source": "docker.io/library/postgres:16.15-bookworm@" + OTHER,
                  "destination": REPOSITORY + ":mirror-postgres-16.15-bookworm"}
        self.entries.append(second)
        self.state["images"][second["source"]] = OTHER
        return second

    def test_copy_failure_stops_before_later_catalog_entries(self):
        second = self.add_second_entry()
        self.state["copy_error"] = "registry upload failed"
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.copies()), 1)
        self.assertFalse(any(second["source"] in command for command in self.state["commands"]))

    def test_cancellation_stops_before_later_catalog_entries(self):
        second = self.add_second_entry()
        self.state["cancel_copy"] = True
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(second["source"] in command for command in self.state["commands"]))

    def test_reference_returns_an_immutable_destination_without_registry_access(self):
        result = self.invoke("reference", self.entry["name"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), self.entry["destination"] + "@" + INDEX)
        self.assertEqual(self.state["commands"], [])

    def test_unknown_reference_is_rejected_without_registry_access(self):
        result = self.invoke("reference", "unknown")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.state["commands"], [])

    def test_catalog_validation_precedes_all_registry_access(self):
        for mutation in ("duplicate name", "duplicate destination", "unpinned source", "invalid digest",
                         "foreign destination", "release tag destination", "ECR source"):
            with self.subTest(mutation=mutation):
                self.entries = [dict(self.entry)]
                self.state["commands"] = []
                if mutation.startswith("duplicate"):
                    duplicate = dict(self.entry)
                    duplicate["name" if mutation == "duplicate destination" else "destination"] += "-duplicate"
                    self.entries.append(duplicate)
                elif mutation == "unpinned source":
                    self.entries[0]["source"] = self.entry["source"].split("@")[0]
                elif mutation == "invalid digest":
                    self.entries[0]["source"] = self.entry["source"].split("@")[0] + "@sha256:invalid"
                elif mutation == "foreign destination":
                    self.entries[0]["destination"] = "ghcr.io/other/package:mirror-postgres"
                elif mutation == "release tag destination":
                    self.entries[0]["destination"] = REPOSITORY + ":0.1.6"
                else:
                    self.entries[0]["source"] = "public.ecr.aws/docker/library/postgres:18.6-bookworm@" + INDEX
                result = self.invoke()
                self.assertNotEqual(result.returncode, 0, mutation)
                self.assertEqual(self.state["commands"], [], mutation + " was discovered after registry access")


class ImageMirrorVisibilityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-mirror-visibility-")
        self.addCleanup(temporary.cleanup)
        self.catalog = Path(temporary.name) / "catalog.json"
        self.source = "docker.io/library/postgres:18.6-bookworm@" + INDEX
        self.destination = REPOSITORY + ":mirror-postgres-18.6-bookworm"
        self.catalog.write_text(json.dumps({"images": [{"name": "postgres-18", "source": self.source,
                                                        "destination": self.destination}]}))
        specification = importlib.util.spec_from_file_location("epco_image_mirror_visibility", HELPER)
        self.helper = importlib.util.module_from_spec(specification)
        specification.loader.exec_module(self.helper)
        self.post_copy_reads = 0
        self.copy_calls = []
        self.waits = []

    def run_visibility(self, responses):
        pending = iter(responses)

        def digest(reference, *, allow_absent=False, **_options):
            if reference == self.source:
                return INDEX
            self.assertEqual(reference, self.destination)
            if not self.copy_calls:
                return None
            self.post_copy_reads += 1
            self.assertLessEqual(self.post_copy_reads, 6, "Post-copy visibility work must remain bounded")
            response = next(pending)
            if isinstance(response, Exception):
                raise response
            if response is None and not allow_absent:
                raise RuntimeError("MANIFEST_UNKNOWN: manifest unknown")
            return response

        def copy(*arguments, **_options):
            self.assertEqual(arguments[0], "copy")
            self.assertEqual(arguments[-2:], (self.source, self.destination))
            self.copy_calls.append(arguments)
            return ""

        with patch.object(self.helper, "registry_digest", side_effect=digest), \
                patch.object(self.helper, "crane", side_effect=copy), \
                patch("time.sleep", side_effect=self.waits.append):
            self.helper.sync(self.catalog)

    def assert_phase_error(self, error, phase, reference):
        self.assertIn(phase, str(error), "Registry failure must identify its synchronization phase")
        self.assertIn(reference, str(error), "Registry failure must identify the inspected source or destination")

    def test_post_copy_absence_converges_without_copying_again(self):
        self.run_visibility([None, None, INDEX])
        self.assertEqual(self.post_copy_reads, 3)
        self.assertEqual(len(self.copy_calls), 1, "Visibility lag must not republish the index")
        self.assertEqual(len(self.waits), 2)
        self.assertTrue(all(delay > 0 for delay in self.waits))

    def test_post_copy_absence_exhausts_a_bounded_retry_budget(self):
        with self.assertRaises((RuntimeError, ValueError)) as raised:
            self.run_visibility([None] * 6)
        self.assertEqual(self.post_copy_reads, 6)
        self.assertEqual(len(self.copy_calls), 1)
        self.assertEqual(len(self.waits), 5)
        self.assertLessEqual(sum(self.waits), 31, "Visibility lag cannot cause unbounded waiting")
        self.assertTrue(all(delay > 0 for delay in self.waits))
        self.assert_phase_error(raised.exception, "post-copy", self.destination)

    def test_post_copy_digest_mismatch_fails_immediately(self):
        with self.assertRaises((RuntimeError, ValueError)) as raised:
            self.run_visibility([OTHER, INDEX])
        self.assertEqual(self.post_copy_reads, 1)
        self.assertEqual(len(self.copy_calls), 1)
        self.assertEqual(self.waits, [], "A different immutable index cannot be retried as visibility lag")
        self.assert_phase_error(raised.exception, "post-copy", self.destination)

    def test_post_copy_authorization_network_and_unknown_errors_are_never_retried(self):
        for message in ("DENIED: requested access is denied", "UNAUTHORIZED: authentication required",
                        "dial tcp: connection refused", "unexpected registry response"):
            with self.subTest(message=message):
                self.post_copy_reads = 0
                self.copy_calls = []
                self.waits = []
                with self.assertRaises((RuntimeError, ValueError)) as raised:
                    self.run_visibility([RuntimeError(message), INDEX])
                self.assertEqual(self.post_copy_reads, 1)
                self.assertEqual(len(self.copy_calls), 1)
                self.assertEqual(self.waits, [])
                self.assertIn(message, str(raised.exception))
                self.assert_phase_error(raised.exception, "post-copy", self.destination)

    def test_inspection_errors_identify_source_or_destination_phase(self):
        for phase, reference in (("source", self.source), ("destination", self.destination)):
            with self.subTest(phase=phase):
                def digest(image, **_options):
                    if image == reference:
                        raise RuntimeError("registry inspection unavailable")
                    return INDEX

                with patch.object(self.helper, "registry_digest", side_effect=digest), \
                        patch.object(self.helper, "crane") as copy:
                    with self.assertRaises((RuntimeError, ValueError)) as raised:
                        self.helper.sync(self.catalog)
                    copy.assert_not_called()
                self.assert_phase_error(raised.exception, phase, reference)


class ImageMirrorCatalogContractTests(unittest.TestCase):
    def catalog(self):
        path = ROOT / "test/images/catalog.json"
        self.assertTrue(path.is_file(), "Commit the single pinned image catalog")
        return json.loads(path.read_text())["images"]

    def test_catalog_pins_every_required_upstream_and_isolates_mirror_tags(self):
        expected = {
            "postgres-15": "docker.io/library/postgres:15.19-bookworm",
            "postgres-16": "docker.io/library/postgres:16.15-bookworm",
            "postgres-17": "docker.io/library/postgres:17.11-bookworm",
            "postgres-18": "docker.io/library/postgres:18.6-bookworm",
            "golang": "docker.io/library/golang:1.27.1",
            "distroless": "gcr.io/distroless/static:nonroot",
            "kind-node": "docker.io/kindest/node:v1.36.1",
            "buildkit": "docker.io/moby/buildkit:buildx-stable-1",
            "binfmt": "docker.io/tonistiigi/binfmt:latest",
            "sbom-scanner": "docker.io/docker/buildkit-syft-scanner:stable-1",
        }
        entries = self.catalog()
        names = [entry["name"] for entry in entries]
        destinations = [entry["destination"] for entry in entries]
        self.assertEqual(len(names), len(set(names)), "Catalog names must be unique")
        self.assertEqual(len(destinations), len(set(destinations)), "Immutable mirror tags must be unique")
        indexed = {entry["name"]: entry for entry in entries}
        self.assertTrue(set(expected).issubset(indexed), "All build and fixture images require accepted pins")
        for name, upstream in expected.items():
            with self.subTest(name=name):
                entry = indexed[name]
                self.assertRegex(entry["source"], "^" + re.escape(upstream) + r"@sha256:[0-9a-f]{64}$")
        for entry in entries:
            self.assertNotIn("ecr", entry["source"].lower())
            self.assertRegex(entry["source"], r"@sha256:[0-9a-f]{64}$")
            self.assertTrue(entry["destination"].startswith(REPOSITORY + ":mirror-"), entry)

    def test_default_catalog_reference_works_outside_the_checkout_without_registry_access(self):
        with tempfile.TemporaryDirectory(prefix="epco-mirror-reference-") as temporary:
            for entry in self.catalog():
                with self.subTest(name=entry["name"]):
                    result = subprocess.run([sys.executable, str(HELPER), "reference", entry["name"]],
                                            cwd=temporary, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.strip(),
                                     entry["destination"] + "@" + entry["source"].rsplit("@", 1)[1])

    def test_dockerfile_and_devcontainer_defaults_match_the_catalog(self):
        references = {entry["name"]: entry["destination"] + "@" + entry["source"].rsplit("@", 1)[1]
                      for entry in self.catalog()}
        dockerfile = (ROOT / "Dockerfile").read_text()
        base = re.search(r"(?m)^ARG BASE_IMAGE=(\S+)$", dockerfile)
        self.assertIsNotNone(base)
        self.assertEqual(base.group(1), references["golang"])
        runtime = re.findall(r"(?m)^FROM (\S+)(?: AS \S+)?$", dockerfile)
        self.assertIn(references["distroless"], runtime)
        devcontainer = json.loads((ROOT / ".devcontainer/devcontainer.json").read_text())
        self.assertEqual(devcontainer["image"], references["golang"])

    def test_daily_workflow_uses_authenticated_package_writes_and_scoped_push_triggers(self):
        candidates = [path for path in (ROOT / ".github/workflows").glob("*.yml")
                      if "tools/image_mirror.py" in path.read_text() and "sync" in path.read_text()]
        self.assertEqual(len(candidates), 1, "Provide one daily/manual pinned image mirror workflow")
        workflow_path = candidates[0]
        checker = shutil.which("helm")
        self.assertIsNotNone(checker, "Use pinned mise tools to parse GitHub workflow YAML")
        with tempfile.TemporaryDirectory(prefix="epco-mirror-workflow-") as temporary:
            chart = Path(temporary)
            (chart / "templates").mkdir()
            (chart / "inputs").mkdir()
            (chart / "Chart.yaml").write_text("apiVersion: v2\nname: mirror-contract-reader\nversion: 0.0.0\n")
            shutil.copyfile(workflow_path, chart / "inputs/workflow.yaml")
            (chart / "templates/parsed.yaml").write_text('{{ .Files.Get "inputs/workflow.yaml" | fromYaml | toJson }}')
            result = subprocess.run([checker, "template", "mirror-reader", str(chart)],
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            workflow = json.loads(result.stdout[result.stdout.index("{"):])
        triggers = workflow.get("on", workflow.get("true", {}))
        self.assertIn("workflow_dispatch", triggers)
        schedules = triggers.get("schedule", [])
        self.assertTrue(schedules, "The catalog must be synchronized daily")
        for schedule in schedules:
            fields = schedule["cron"].split()
            self.assertEqual(len(fields), 5)
            self.assertTrue(fields[0].isdigit() and fields[1].isdigit(), "Use one bounded daily sync")
            self.assertEqual(fields[2:], ["*", "*", "*"])
        paths = triggers.get("push", {}).get("paths", [])
        for changed in ("tools/image_mirror.py", "test/images/catalog.json", str(workflow_path.relative_to(ROOT))):
            self.assertIn(changed, paths, "Initial branch sync must follow changes to accepted mirror inputs")
        sync_jobs = [job for job in workflow["jobs"].values()
                     if any("image_mirror.py" in step.get("run", "") and "sync" in step.get("run", "")
                            for step in job.get("steps", []))]
        self.assertTrue(sync_jobs)
        for job in sync_jobs:
            permissions = job.get("permissions", workflow.get("permissions", {}))
            self.assertEqual(permissions.get("packages"), "write")
            steps = json.dumps(job.get("steps", []))
            self.assertTrue("GITHUB_TOKEN" in steps or "github.token" in steps)
            self.assertTrue("crane auth login" in steps or "docker login" in steps)
            self.assertIn("--password-stdin", steps)
            self.assertNotIn("public.ecr.aws", steps)


if __name__ == "__main__":
    unittest.main()
