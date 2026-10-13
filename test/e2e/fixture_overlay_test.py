"""Offline behavior checks for the owned migration fixture overlay."""

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SUITE = Path(__file__).resolve().parent
HELPER = SUITE / "fixture_overlay.py"
ORIGINAL = SUITE / "database_connections_test.go"
SCRIPT = SUITE / "fixtures" / "migration.sh"
ANCHOR = b"func installedConnectionJob(namespace, output string) *batchv1.Job {\n\tconst script = `"
APPLICATION_SHA = "a" * 40
FIXTURE_SHA = "b" * 40


def digest(data):
    return hashlib.sha256(data).hexdigest()


class FixtureOverlayTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-overlay-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "database_connections_test.go"
        self.source.write_bytes(ORIGINAL.read_bytes())
        self.script = self.root / "migration.sh"
        self.output = self.root / "owned"
        self.record = self.root / "artifacts" / "fixture-overlay.json"

    def invoke(self):
        self.assertTrue(HELPER.is_file(), "The owned overlay helper is required")
        return subprocess.run(
            [sys.executable, str(HELPER), "--source", str(self.source), "--script", str(self.script),
             "--output", str(self.output), "--record", str(self.record),
             "--application-source", APPLICATION_SHA, "--fixture-source", FIXTURE_SHA],
            text=True, capture_output=True, timeout=5,
        )

    def test_only_migration_literal_is_replaced_and_hashes_bind_sources(self):
        self.assertTrue(SCRIPT.is_file(), "The migration fixture script is required")
        self.script.write_bytes(SCRIPT.read_bytes())
        original = self.source.read_bytes()
        script = self.script.read_bytes()
        self.assertEqual(original.count(ANCHOR), 1)
        prefix, tail = original.split(ANCHOR)
        old_script, suffix = tail.split(b"`", 1)
        self.assertTrue(old_script)

        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.source.read_bytes(), original, "The original Go test must stay byte-identical")
        compiled = self.output / "database_connections_test.go"
        overlay = self.output / "go-overlay.json"
        self.assertEqual(compiled.read_bytes(), prefix + ANCHOR + script + b"`" + suffix)
        self.assertEqual(json.loads(overlay.read_text()),
                         {"Replace": {str(self.source.resolve()): str(compiled.resolve())}})
        record = json.loads(self.record.read_text())
        self.assertEqual(record["application_source_sha"], APPLICATION_SHA)
        self.assertEqual(record["fixture_source_sha"], FIXTURE_SHA)
        self.assertEqual(record["original_test_sha256"], digest(original))
        self.assertEqual(record["fixture_script_sha256"], digest(script))
        self.assertEqual(record["compiled_test_sha256"], digest(compiled.read_bytes()))
        self.assertEqual(record["overlay_map_sha256"], digest(overlay.read_bytes()))
        self.assertEqual(record["preserved_prefix_sha256"], digest(prefix + ANCHOR))
        self.assertEqual(record["preserved_suffix_sha256"], digest(b"`" + suffix))

    def test_ambiguous_or_unsafe_profiles_fail_without_derived_files(self):
        self.assertTrue(SCRIPT.is_file(), "The migration fixture script is required")
        valid_source = self.source.read_bytes()
        valid_script = SCRIPT.read_bytes()
        source_cases = {
            "missing-anchor": valid_source.replace(ANCHOR, b"func omittedJob() {}", 1),
            "duplicate-anchor": valid_source + b"\n" + ANCHOR + b"duplicate`\n",
            "unclosed-script": valid_source.replace(b"`\n\treturn &batchv1.Job", b"\n\treturn &batchv1.Job", 1),
        }
        script_cases = {
            "backtick": valid_script + b"`",
            "nul": valid_script + b"\x00",
            "cr": valid_script + b"\r",
            "invalid-utf8": valid_script + b"\xff",
        }
        for name, source in source_cases.items():
            with self.subTest(name=name):
                self.source.write_bytes(source)
                self.script.write_bytes(valid_script)
                result = self.invoke()
                self.assertNotEqual(result.returncode, 0, name)
                self.assertEqual(self.source.read_bytes(), source)
                self.assertFalse((self.output / "go-overlay.json").exists())
                self.assertFalse(self.record.exists())
                self.output.mkdir(exist_ok=True)
                for path in self.output.iterdir():
                    path.unlink()
                self.record.unlink(missing_ok=True)
        self.source.write_bytes(valid_source)
        for name, script in script_cases.items():
            with self.subTest(name=name):
                self.script.write_bytes(script)
                result = self.invoke()
                self.assertNotEqual(result.returncode, 0, name)
                self.assertEqual(self.source.read_bytes(), valid_source)
                self.assertFalse((self.output / "go-overlay.json").exists())
                self.assertFalse(self.record.exists())
                for path in self.output.iterdir():
                    path.unlink()


if __name__ == "__main__":
    unittest.main()
