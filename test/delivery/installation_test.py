"""Fail-closed packaged-schema installation gates, before any Kubernetes write."""

import copy
import json
from runpy import run_path
import sys
import tarfile
import unittest

from support import CHART, DeliveryCase


DATABASE_ROLE_CRD = "databaseroles.postgresql.epco.io"
V020_DATABASE_ROLE_FINGERPRINT = "sha256:f7dbe4e93edeab04012aba92efd061d025084ff7659b1c5caa0981f9f47a71ca"


class InstallationTests(DeliveryCase):
    def package_fixture(self):
        self.assert_success(self.run_cli(sys.executable, "tools/chart.py", "sync", "--chart-dir", str(CHART), "--inputs-dir", "config"))
        generated = self.repo / CHART / "generated/crds.json"
        self.assertTrue(generated.exists(), "packaged generated source CRD objects are required")
        objects = json.loads(generated.read_text())
        self.assertEqual(len(objects), 3)
        self.assertTrue((self.repo / CHART / "crd-compatibility.json").exists(), "package must declare its explicit supported schema fingerprints")
        self.assert_success(self.helm("package", str(CHART), "--destination", str(self.directory)))
        packages = list(self.directory.glob("*.tgz"))
        self.assertEqual(len(packages), 1)
        with tarfile.open(packages[0]) as archive:
            names = archive.getnames()
            self.assertTrue(any(name.endswith("/crd-compatibility.json") for name in names))
            self.assertTrue(any(name.endswith("/generated/crds.json") for name in names))
        return packages[0], objects

    def install(self, package, scenario="known"):
        return self.run_cli(sys.executable, "tools/chart.py", "install", "--package", str(package),
                            "--release", "epco", "--namespace", "epco-system",
                            "--context", "kind-epco-delivery-owned", environment={"DELIVERY_SCHEMA_SCENARIO": scenario})

    def test_unknown_retained_schema_refuses_before_any_write(self):
        package, objects = self.package_fixture()
        objects = copy.deepcopy(objects)
        schema = objects[0]["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
        schema.setdefault("properties", {})["unknownDeliveryField"] = {"type": "string"}
        (self.directory / "schema-objects.json").write_text(json.dumps(objects))
        result = self.install(package)
        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(result.stdout + result.stderr, r"(?i)(unknown|incompatible|unsupported).*(schema|fingerprint)|(schema|fingerprint).*(unknown|incompatible|unsupported)")
        self.assert_no_mutations()

    def test_additive_password_status_upgrade_accepts_only_the_exact_v020_role_schema(self):
        package, objects = self.package_fixture()
        role = next(obj for obj in objects if obj["metadata"]["name"] == DATABASE_ROLE_CRD)
        version = next(version for version in role["spec"]["versions"] if version["name"] == "v1alpha1")
        status = version["schema"]["openAPIV3Schema"]["properties"]["status"]
        self.assertNotIn("passwordWrite", status.get("required", []))
        del status["properties"]["passwordWrite"]
        crd_fingerprint = run_path(self.repo / "tools/chart.py")["crd_fingerprint"]
        self.assertEqual(crd_fingerprint(role["spec"]), V020_DATABASE_ROLE_FINGERPRINT)

        retained = self.directory / "schema-objects.json"
        dry_run_failure = "intentional schema upgrade dry-run stop"
        self.environment["DELIVERY_SERVER_DRY_RUN_FAILURE"] = dry_run_failure
        with self.subTest(schema="exact v0.2.0 DatabaseRole"):
            retained.write_text(json.dumps(objects))
            result = self.install(package)
            self.assertNotEqual(result.returncode, 0, "the simulated admission failure must stop the installer")
            self.assert_no_mutations()
            self.assertEqual(sum(call["serverDryRun"] for call in self.calls()), 1, result.stdout + result.stderr)
            self.assertIn(dry_run_failure, result.stderr)

        with self.subTest(schema="unrecognized variant of v0.2.0 DatabaseRole"):
            (self.directory / "calls.jsonl").write_text("")
            status["properties"]["unknownDeliveryField"] = {"type": "string"}
            retained.write_text(json.dumps(objects))
            result = self.install(package)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(f"unknown or incompatible retained schema fingerprint for CRD {DATABASE_ROLE_CRD}", result.stderr)
            self.assertFalse(any(call["serverDryRun"] for call in self.calls()))
            self.assert_no_mutations()

    def test_schema_read_failure_refuses_before_any_write(self):
        package, objects = self.package_fixture()
        (self.directory / "schema-objects.json").write_text(json.dumps(objects))
        result = self.install(package, "read-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(result.stdout + result.stderr, r"(?i)(forbidden|read|inspect).*(crd|schema)|(crd|schema).*(forbidden|read|inspect)")
        self.assert_no_mutations()

    def test_legacy_sentinel_ownership_refuses_before_any_write(self):
        package, objects = self.package_fixture()
        (self.directory / "schema-objects.json").write_text(json.dumps(objects))
        result = self.install(package, "legacy-ownership")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ownership", (result.stdout + result.stderr).lower())
        self.assert_no_mutations()

    def test_missing_local_package_and_empty_context_fail_without_writes(self):
        result = self.install(self.directory / "missing.tgz")
        self.assertNotEqual(result.returncode, 0)
        self.assertRegex(result.stdout + result.stderr, r"(?i)(package|file|archive)")
        self.assert_no_mutations()
        result = self.run_cli(sys.executable, "tools/chart.py", "install", "--package", str(self.directory / "missing.tgz"),
                              "--release", "epco", "--namespace", "epco-system", "--context", "")
        self.assertNotEqual(result.returncode, 0)
        self.assert_no_mutations()


if __name__ == "__main__":
    unittest.main(verbosity=2)
