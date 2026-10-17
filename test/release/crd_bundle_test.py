"""CRD-only release downloads use the selected application's generated inputs."""

import copy
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import unittest

from helper_support import PreparationFixture, measured_runtime
from support import IMAGE_SHA, checksum


CRD_NAMES = {"postgresclusters.postgresql.epco.io", "databaseroles.postgresql.epco.io", "databases.postgresql.epco.io"}


class CRDBundleTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-crd-bundle-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)

    def prepare(self, fixture):
        seed = fixture.seed_path()
        fixture.save()
        result = subprocess.run(
            [sys.executable, str(fixture.helper), "prepare", "--manifest", str(seed), "--output", str(fixture.output)],
            cwd=fixture.source, env=fixture.env, capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fixture.state = json.loads(fixture.state_path.read_text())
        return json.loads((fixture.output / "manifest.json").read_text())

    def bundle(self, fixture, manifest):
        entries = [entry for entry in manifest["assets"] if entry["path"] == "crds.yaml"]
        self.assertEqual(len(entries), 1, "New releases must inventory one downloadable crds.yaml asset")
        path = fixture.output / "crds.yaml"
        self.assertTrue(path.is_file(), "New releases must provide the CRD-only crds.yaml download")
        self.assertEqual(entries[0]["sha256"], checksum(path.read_bytes()))
        return path, entries[0]

    def yaml_objects(self, fixture, contents):
        # Reuse the pinned tool's YAML parser without depending on the production
        # bundle implementation or adding a Python YAML package to the suite.
        with tempfile.TemporaryDirectory(prefix="epco-bundle-yaml-") as temporary:
            chart = Path(temporary)
            (chart / "inputs").mkdir()
            (chart / "templates").mkdir()
            (chart / "Chart.yaml").write_text("apiVersion: v2\nname: crd-bundle-reader\nversion: 0.0.0\n")
            documents = [document for document in re.split(r"(?m)^---[ \t]*(?:\r?\n|$)", contents) if document.strip()]
            self.assertTrue(documents, "The bundle must contain YAML documents")
            paths = []
            for index, document in enumerate(documents):
                name = "inputs/" + str(index) + ".yaml"
                (chart / name).write_text(document)
                paths.append(json.dumps(name))
            template = ('{"apiVersion":"v1","kind":"List","items":['
                        '{{ range $index, $path := list ' + " ".join(paths) + ' }}'
                        '{{ if $index }},{{ end }}{{ $.Files.Get $path | fromYaml | toJson }}'
                        '{{ end }}]}\n')
            (chart / "templates/parsed.yaml").write_text(template)
            result = subprocess.run([fixture.state["real_helm"], "template", "crd-bundle-reader", str(chart)],
                                    capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, "Bundle is not valid YAML: " + result.stderr)
            objects = json.loads(result.stdout[result.stdout.index("{"):])["items"]
            self.assertTrue(all(isinstance(obj, dict) and "Error" not in obj for obj in objects), objects)
            return objects

    def assert_selected_crds(self, fixture, manifest, bundle_path):
        actual = self.yaml_objects(fixture, bundle_path.read_text())
        self.assertEqual(len(actual), 3, "Bundle must contain exactly the three CRDs and no controller or RBAC")
        self.assertEqual({obj.get("kind") for obj in actual}, {"CustomResourceDefinition"})
        self.assertEqual({obj.get("apiVersion") for obj in actual}, {"apiextensions.k8s.io/v1"})
        self.assertEqual({obj.get("metadata", {}).get("name") for obj in actual}, CRD_NAMES)
        expected = []
        for entry in manifest["image"]["generated_inputs"]:
            self.assertNotEqual(entry["path"], "crds.yaml", "The bundle must not replace individual image inputs")
            self.assertEqual(entry["source_sha"], IMAGE_SHA)
            source = fixture.output / entry["path"]
            self.assertEqual(checksum(source.read_bytes()), entry["sha256"])
            if entry["kind"] == "crds":
                expected.extend(self.yaml_objects(fixture, source.read_text()))
        self.assertEqual(len(expected), 3)
        by_name = lambda objects: {obj["metadata"]["name"]: obj for obj in objects}
        self.assertEqual(by_name(actual), by_name(expected), "Bundle must preserve the selected generated CRD schemas")

    def test_new_application_and_chart_releases_publish_signed_crd_bundle(self):
        for stream in ("image", "chart"):
            with self.subTest(stream=stream):
                fixture = PreparationFixture(self.directory / stream, stream)
                manifest = self.prepare(fixture)
                path, entry = self.bundle(fixture, manifest)
                self.assert_selected_crds(fixture, manifest, path)
                sums = (fixture.output / "SHA256SUMS").read_text().splitlines()
                self.assertIn(entry["sha256"] + "  crds.yaml", sums)
                self.assertTrue((fixture.output / manifest["checksum_bundle"]).is_file())
                evidence = json.loads((fixture.output / "release-evidence.json").read_text())
                self.assertEqual([asset for asset in evidence["assets"] if asset["path"] == "crds.yaml"], [entry])
                self.assertTrue(any(call["tool"] == "cosign" and call["args"][:1] == ["sign-blob"]
                                    and call["args"][-1] == str(fixture.output / "SHA256SUMS") for call in fixture.calls()))
                with tarfile.open(fixture.output / "release-candidate.tar.gz", "r:gz") as checkpoint:
                    self.assertEqual(checkpoint.extractfile("candidate/crds.yaml").read(), path.read_bytes())

                fixture.state["runtime_payload"] = measured_runtime(manifest, fixture.output)
                manifest["runtime_evidence"] = {"run_id": 900, "run_attempt": 1, "artifact_id": 901}
                manifest_path = fixture.output / "manifest.json"
                manifest_path.write_text(json.dumps(manifest))
                fixture.save()
                result = subprocess.run(
                    [sys.executable, str(fixture.source / "tools/release.py"), stream,
                     "--manifest", str(manifest_path), "--phase", "publish"],
                    env=fixture.env, capture_output=True, text=True, timeout=60,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                state = json.loads(fixture.state_path.read_text())
                published = state["releases"][manifest["tag"]]
                self.assertFalse(published["draft"])
                self.assertEqual([asset["digest"] for asset in published["assets"] if asset["name"] == "crds.yaml"],
                                 ["sha256:" + entry["sha256"]])

    def test_chart_bundle_uses_verified_older_application_without_existing_bundle(self):
        fixture = PreparationFixture(self.directory / "legacy-app-chart")
        original = fixture.root / "accepted-image"
        old = json.loads((original / "manifest.json").read_text())
        self.assertNotIn("crds.yaml", {entry["path"] for entry in old["assets"]})
        self.assertFalse((original / "crds.yaml").exists())
        for source in (fixture.source / "config/crd/bases").glob("*.yaml"):
            source.write_text(source.read_text().replace("  annotations:\n", '  annotations:\n    release-test.epco.io/chart-head: "must-not-be-bundled"\n', 1))
        manifest = self.prepare(fixture)
        path, _ = self.bundle(fixture, manifest)
        self.assertEqual(manifest["image"]["generated_inputs"], old["image"]["generated_inputs"])
        self.assert_selected_crds(fixture, manifest, path)
        self.assertNotIn("must-not-be-bundled", path.read_text())
        for entry in old["image"]["generated_inputs"]:
            self.assertEqual((fixture.output / entry["path"]).read_bytes(), (original / entry["path"]).read_bytes())
        self.assertTrue(any(call["tool"] == "gh" and call["args"][:2] == ["release", "verify-asset"] for call in fixture.calls()))

    def test_bundle_bytes_do_not_depend_on_verified_generated_input_inventory_order(self):
        prepared = []
        for index, reverse in enumerate((False, True)):
            fixture = PreparationFixture(self.directory / ("order-" + str(index)))
            original = fixture.root / "accepted-image"
            old = json.loads((original / "manifest.json").read_text())
            if reverse:
                old["image"]["generated_inputs"].reverse()
                evidence = copy.deepcopy(old)
                evidence["assets"] = [entry for entry in evidence["assets"] if entry["kind"] != "evidence"]
                evidence.update({"image_source_sha": IMAGE_SHA, "image_index_digest": old["image"]["index_digest"], "chart_digest": None})
                evidence_path = original / "release-evidence.json"
                evidence_path.write_text(json.dumps(evidence))
                next(entry for entry in old["assets"] if entry["kind"] == "evidence")["sha256"] = checksum(evidence_path.read_bytes())
                (original / "SHA256SUMS").write_text("".join(entry["sha256"] + "  " + entry["path"] + "\n" for entry in old["assets"]))
                (original / "manifest.json").write_text(json.dumps(old))
                fixture.state["releases"]["v0.1.0"] = fixture.release(old, original)
                fixture.manifest["image"] = copy.deepcopy(old["image"])
            manifest = self.prepare(fixture)
            prepared.append((fixture, manifest))
        outputs = []
        for fixture, manifest in prepared:
            path, _ = self.bundle(fixture, manifest)
            self.assert_selected_crds(fixture, manifest, path)
            outputs.append(path.read_bytes())
        self.assertEqual(outputs[0], outputs[1], "Equivalent selected inputs must produce byte-identical bundle ordering")


if __name__ == "__main__":
    unittest.main()
