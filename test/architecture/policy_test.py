"""Exercise the project's architecture policy with the real pinned checker."""

import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class ArchitecturePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.policy = ROOT / ".go-arch-lint.yml"
        if not cls.policy.is_file():
            raise AssertionError(f"Missing production architecture policy: {cls.policy}")
        cls.checker = shutil.which("go-arch-lint")
        if cls.checker is None:
            raise AssertionError("go-arch-lint is missing from PATH; run this suite with mise")
        cls.module = next(
            line.split()[1] for line in (ROOT / "go.mod").read_text().splitlines()
            if line.startswith("module ")
        )

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-architecture-")
        self.addCleanup(temporary.cleanup)
        self.project = Path(temporary.name)
        for name in ("go.mod", "go.sum", ".go-arch-lint.yml"):
            shutil.copyfile(ROOT / name, self.project / name)
        # Preserve actual packages and generated sources, without copying build artifacts.
        for directory in ("api", "cmd", "internal", "test"):
            for source in (ROOT / directory).rglob("*.go"):
                target = self.project / source.relative_to(ROOT)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, target)

    def write_import(self, relative_path, imported):
        target = self.project / relative_path
        target.parent.mkdir(parents=True, exist_ok=True)
        package = "main" if target.parent.name == "cmd" else target.parent.name
        target.write_text(f'package {package}\n\nimport _ "{imported}"\n')

    def check(self):
        result = subprocess.run(
            [self.checker, "check", "--project-path", str(self.project), "--json"],
            cwd=self.project, capture_output=True, text=True, timeout=120,
        )
        diagnostic = result.stdout + result.stderr
        try:
            payload = json.loads(result.stdout)["Payload"]
        except (json.JSONDecodeError, KeyError) as error:
            self.fail(f"Checker did not return a check report: {error}\n{diagnostic}")
        self.assertEqual(payload["ExecutionWarnings"], [], diagnostic)
        self.assertEqual(payload["OmittedCount"], 0, diagnostic)
        return result.returncode, payload, diagnostic

    def assert_accepted(self):
        code, payload, diagnostic = self.check()
        self.assertEqual(code, 0, diagnostic)
        self.assertFalse(payload["ArchHasWarnings"], diagnostic)

    def assert_import_rejected(self, relative_path, imported):
        self.write_import(relative_path, imported)
        code, payload, diagnostic = self.check()
        self.assertEqual(code, 1, diagnostic)
        self.assertTrue(payload["ArchHasWarnings"], diagnostic)
        self.assertTrue(any(
            warning["FileRelativePath"].lstrip("/") == relative_path
            and warning["ResolvedImportName"] == imported
            for warning in payload["ArchWarningsDeps"]
        ), diagnostic)

    def test_current_production_graph_is_accepted(self):
        self.assert_accepted()

    def test_allowed_boundaries_are_accepted(self):
        imports = {
            "cmd": ("api/v1alpha1", "internal/controller", "internal/postgres"),
            "internal/controller": ("api/v1alpha1", "internal/postgres"),
            "internal/postgres": ("api/v1alpha1",),
        }
        for directory, dependencies in imports.items():
            for index, dependency in enumerate(dependencies):
                self.write_import(f"{directory}/architecture_allowed_{index}.go",
                                  f"{self.module}/{dependency}")
        for directory, imported in (
            ("api/v1alpha1", "k8s.io/apimachinery/pkg/runtime"),
            ("internal/postgres", "github.com/jackc/pgx/v5"),
            ("internal/postgres", "k8s.io/api/core/v1"),
            ("internal/controller", "k8s.io/client-go/kubernetes"),
            ("internal/controller", "sigs.k8s.io/controller-runtime/pkg/client"),
        ):
            self.write_import(f"{directory}/architecture_vendor_{imported.split('/')[-1]}.go",
                              imported)
        self.assert_accepted()

    def test_controller_cannot_import_pgx(self):
        self.assert_import_rejected("internal/controller/architecture_violation.go",
                                    "github.com/jackc/pgx/v5")

    def test_postgres_cannot_import_controller_runtime(self):
        self.assert_import_rejected("internal/postgres/architecture_violation.go",
                                    "sigs.k8s.io/controller-runtime/pkg/client")

    def test_api_cannot_import_internal_implementation(self):
        self.assert_import_rejected("api/v1alpha1/architecture_violation.go",
                                    f"{self.module}/internal/postgres")

    def test_production_cannot_import_test_helpers(self):
        self.assert_import_rejected("internal/controller/architecture_violation.go",
                                    f"{self.module}/test/utils")

    def test_new_unmapped_package_is_rejected(self):
        relative_path = "internal/unmapped/architecture_unmapped.go"
        target = self.project / relative_path
        target.parent.mkdir(parents=True)
        target.write_text("package unmapped\n")
        code, payload, diagnostic = self.check()
        self.assertEqual(code, 1, diagnostic)
        self.assertTrue(payload["ArchHasWarnings"], diagnostic)
        self.assertIn(relative_path, json.dumps(payload["ArchWarningsNotMatched"]), diagnostic)

    def test_excluded_directory_prefixes_do_not_hide_new_packages(self):
        # The scanner resolves directory exclusions only when their targets exist.
        (self.project / "tools").mkdir(exist_ok=True)
        for directory in ("testhelper", "toolshed"):
            with self.subTest(directory=directory):
                relative_path = f"{directory}/unknown.go"
                target = self.project / relative_path
                target.parent.mkdir()
                target.write_text(f"package {directory}\n")
                code, payload, diagnostic = self.check()
                self.assertEqual(code, 1, diagnostic)
                self.assertTrue(payload["ArchHasWarnings"], diagnostic)
                self.assertIn(relative_path,
                              json.dumps(payload["ArchWarningsNotMatched"]), diagnostic)

    def test_tests_are_outside_the_production_policy(self):
        self.write_import("internal/controller/architecture_violation_test.go",
                          "github.com/jackc/pgx/v5")
        self.write_import("test/architecture/architecture_violation.go",
                          "github.com/architecture-policy/forbidden")
        self.assert_accepted()


if __name__ == "__main__":
    unittest.main(verbosity=2)
