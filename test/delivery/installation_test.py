"""Fail-closed packaged-schema installation gates, before any Kubernetes write."""

import copy
import json
import sys
import tarfile
import unittest

from support import CHART, DeliveryCase


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
