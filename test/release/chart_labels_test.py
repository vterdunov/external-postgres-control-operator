"""Rendered chart label contracts for plain and Flux OCI chart versions."""

from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


CHART = Path(__file__).resolve().parents[2] / "charts/external-postgres-control-operator"


def documents(text):
    return [value for value in re.split(r"(?m)^---\s*$", text) if re.search(r"(?m)^kind:", value)]


def kind(document):
    return re.search(r"(?m)^kind:\s*(\S+)", document).group(1)


class ChartLabelTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-chart-labels-")
        self.addCleanup(temporary.cleanup)
        self.chart = Path(temporary.name) / "chart"
        shutil.copytree(CHART, self.chart)

    def render(self, release):
        result = subprocess.run(
            ["helm", "template", release, str(self.chart), "--namespace", "epco-system"],
            capture_output=True, text=True, timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_chart_labels_normalize_oci_build_metadata_and_preserve_standard_labels(self):
        metadata = self.chart / "Chart.yaml"
        source = metadata.read_text()
        chart_name = re.search(r"(?m)^name:\s*(\S+)\s*$", source).group(1)
        for version in ("0.1.3", "0.1.3+356da4c1e7d7", "0.1.3-rc.1+356da4c1e7d7"):
            with self.subTest(version=version):
                metadata.write_text(re.sub(r"(?m)^version:.*$", "version: " + version, source))
                rendered = self.render(release="label-fixture")
                expected = {
                    "app.kubernetes.io/name": "epco",
                    "app.kubernetes.io/instance": "label-fixture",
                    "app.kubernetes.io/managed-by": "Helm",
                    "helm.sh/chart": chart_name + "-" + version.replace("+", "_"),
                }
                for document in documents(rendered):
                    for label, value in expected.items():
                        matches = re.findall(r"(?m)^\s+" + re.escape(label) + r":\s*([^\n]+)$", document)
                        self.assertTrue(matches, f"{kind(document)} must retain {label}")
                        for match in matches:
                            actual = match.strip().strip("\"'")
                            self.assertEqual(actual, value, f"{kind(document)} {label}")
                            if label == "helm.sh/chart":
                                self.assertLessEqual(len(actual), 63, "chart label must fit Kubernetes limits")
                                self.assertRegex(actual, r"^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$",
                                                 "chart label must use Kubernetes label syntax")


if __name__ == "__main__":
    unittest.main(verbosity=2)
