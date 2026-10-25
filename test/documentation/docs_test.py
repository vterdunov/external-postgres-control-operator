"""Behavioral tests for Go-source API docs and repository Markdown links."""

from pathlib import Path
import shutil
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))

if not (ROOT / "tools/docs.py").is_file():
    raise ModuleNotFoundError("repository documentation generator is missing: tools/docs.py")
import docs


def copy_api_doc_inputs(destination):
    """Copy only inputs needed to render/check the API reference."""
    for relative in ("api", "tools/crd-ref-docs"):
        source = ROOT / relative
        if source.is_dir():
            shutil.copytree(source, destination / relative)
    for name in ("go.mod", "go.sum"):
        source = ROOT / name
        if source.is_file():
            shutil.copyfile(source, destination / name)


class APIDocumentationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if Path(docs.__file__).resolve() != ROOT / "tools/docs.py":
            raise AssertionError("Tests must load the repository documentation generator")

    def test_reference_uses_api_source_comments_and_supported_schema_markers(self):
        reference = docs.expected_reference(ROOT)

        for token in (
            "ExternalPostgres", "DatabaseRole", "Database",
            "Host is the PostgreSQL hostname or IP address.",
            "Port", "5432", "verify-full", "disable", "verify-ca",
            "duration must be positive", "cluster reference is immutable",
            "passwordGeneration", "connectionSecrets", "defaultPrivileges",
            "required", "default", "enum", "array", "map",
        ):
            with self.subTest(token=token):
                self.assertIn(token, reference)

    def test_reference_generation_is_deterministic(self):
        first = docs.expected_reference(ROOT)
        second = docs.expected_reference(ROOT)

        self.assertEqual(first, second)

    def test_reference_has_clean_markdown_line_endings(self):
        reference = docs.expected_reference(ROOT)

        trailing = [index for index, line in enumerate(reference.splitlines(), 1)
                    if line.rstrip(" \t") != line]
        self.assertEqual([], trailing, f"Generated API reference has trailing whitespace on lines {trailing}")
        self.assertTrue(reference.endswith("\n"), "Generated API reference must end with a newline")
        self.assertFalse(reference.endswith("\n\n"), "Generated API reference must have exactly one terminal newline")

    def test_reference_tracks_api_comment_changes_before_crd_regeneration(self):
        with tempfile.TemporaryDirectory(prefix="epco-api-source-") as temporary:
            root = Path(temporary)
            copy_api_doc_inputs(root)
            source = root / "api/v1alpha1/externalpostgres_types.go"
            before = source.read_text()
            original = "Host is the PostgreSQL hostname or IP address."
            changed = "Host is the docs-source-freshness sentinel."
            self.assertIn(original, before)

            initial = docs.expected_reference(root)
            source.write_text(before.replace(original, changed, 1))
            regenerated = docs.expected_reference(root)

            self.assertNotEqual(initial, regenerated)
            self.assertIn(changed, regenerated)
            self.assertFalse((root / "config/crd/bases").exists(),
                             "The isolated source check must not depend on generated CRDs")

    def test_reference_check_reports_missing_and_stale_without_rewriting(self):
        with tempfile.TemporaryDirectory(prefix="epco-api-reference-") as temporary:
            root = Path(temporary)
            copy_api_doc_inputs(root)

            missing = docs.check_api_reference(root)
            self.assertTrue(missing, "A missing generated reference must fail the documentation check")
            self.assertFalse((root / "docs/reference/api.md").exists())

            reference = root / "docs/reference/api.md"
            reference.parent.mkdir(parents=True)
            reference.write_text("stale content\n")
            before = reference.read_bytes()
            stale = docs.check_api_reference(root)
            self.assertTrue(stale, "A stale generated reference must fail the documentation check")
            self.assertEqual(before, reference.read_bytes(), "Check mode must never rewrite generated docs")

    def test_markdown_links_accept_local_targets_and_ignore_external_and_code(self):
        with tempfile.TemporaryDirectory(prefix="epco-doc-links-") as temporary:
            root = Path(temporary)
            (root / "docs").mkdir()
            (root / "examples").mkdir()
            (root / "docs/index.md").write_text(
                "[example](../examples/basic.yaml) [external](https://example.test)\n"
                "`[code](missing.md)`\n"
            )
            (root / "examples/basic.yaml").write_text("kind: Example\n")

            self.assertEqual([], docs.check_markdown_links(root))

    def test_markdown_link_checker_reports_missing_local_target(self):
        with tempfile.TemporaryDirectory(prefix="epco-doc-links-") as temporary:
            root = Path(temporary)
            (root / "README.md").write_text("[missing](docs/absent.md)\n")

            diagnostics = docs.check_markdown_links(root)

            self.assertEqual(1, len(diagnostics))
            self.assertIn("README.md", diagnostics[0])
            self.assertIn("docs/absent.md", diagnostics[0])


if __name__ == "__main__":
    unittest.main(verbosity=2)
