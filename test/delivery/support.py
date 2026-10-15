"""Disposable subprocess fixtures shared by the delivery suite."""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
CHART = Path("charts/external-postgres-control-operator")
KINDS = ("postgresclusters", "databaseroles", "databases")
DIGEST = "sha256:" + "a" * 64
IMAGE = "ghcr.io/vterdunov/external-postgres-control-operator:0.2.3@" + DIGEST


def documents(text):
    """Split rendered YAML documents; assertions inspect actual rendered fields."""
    return [value for value in re.split(r"(?m)^---\s*$", text) if re.search(r"(?m)^kind:", value)]


def kind(document):
    return re.search(r"(?m)^kind:\s*(\S+)", document).group(1)


class DeliveryCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="epco-delivery-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.repo = self.directory / "repo"
        self.repo.mkdir()
        for name in ("tools", "config", "charts"):
            if (ROOT / name).exists():
                shutil.copytree(ROOT / name, self.repo / name)
        shutil.copytree(ROOT / "test/images", self.repo / "test/images")
        for name in ("Dockerfile", "mise.toml", "mise.lock"):
            if (ROOT / name).exists():
                shutil.copy2(ROOT / name, self.repo / name)
        self.bin = self.directory / "bin"
        self.bin.mkdir()
        for name in ("kubectl", "container-cli"):
            shutil.copy2(Path(__file__).with_name("fixtures") / "cli.py", self.bin / name)
            fake = self.bin / name
            fake.write_text(fake.read_text().replace("#!/usr/bin/env python3", "#!" + sys.executable, 1))
            (self.bin / name).chmod(0o700)
        self.inventory = [
            {"kind": "CustomResourceDefinition", "uid": "schema-original"},
            {"kind": "Namespace", "uid": "namespace-original"},
            {"kind": "PostgresCluster", "uid": "cluster-original", "finalizers": ["epco"]},
            {"kind": "DatabaseRole", "uid": "role-original", "status": {"hasManagedSQL": True}},
            {"kind": "Database", "uid": "database-original", "status": {"managedACLTargets": ["original"]}},
            {"kind": "Secret", "uid": "supplied-original", "data": {"password": "private-fixture"}},
            {"kind": "Secret", "uid": "generated-original", "data": {"password": "private-fixture"}},
            {"kind": "Deployment", "uid": "manager-original"},
            {"kind": "ClusterRole", "uid": "rbac-original"},
        ]
        (self.directory / "inventory.json").write_text(json.dumps(self.inventory))
        self.lists = {resource: [] for resource in KINDS}
        self.save_lists()
        self.environment = {
            **os.environ,
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "DELIVERY_STATE": str(self.directory),
            "CONTAINER_TOOL": str(self.bin / "container-cli"),
            "KUBE_CONTEXT": "kind-epco-delivery-owned",
            "ACK_CRD_RETIREMENT": "delete-all-epco-crds",
            "IMG": IMAGE,
            "MISE_TRUSTED_CONFIG_PATHS": str(self.repo),
        }

    def save_lists(self):
        (self.directory / "cr-lists.json").write_text(json.dumps(self.lists))

    def run_cli(self, *args, environment=None):
        return subprocess.run(args, cwd=self.repo, env={**self.environment, **(environment or {})},
                              capture_output=True, text=True, timeout=30)

    def calls(self):
        path = self.directory / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def remaining(self):
        return json.loads((self.directory / "inventory.json").read_text())

    def assert_success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_no_mutations(self):
        writes = {"delete", "apply", "patch", "replace", "create"}
        self.assertFalse([call for call in self.calls() if writes.intersection(call["args"]) and not call.get("serverDryRun")],
                         "refusal must perform zero persistent Kubernetes writes")

    def helm(self, *args):
        self.assertTrue((self.repo / CHART / "Chart.yaml").exists(), "R7a chart is required")
        result = self.run_cli("helm", *args)
        return result

    def render(self, *overrides, release="epco", namespace="epco-system"):
        result = self.helm("template", release, str(CHART), "--namespace", namespace, *overrides)
        self.assert_success(result)
        return result.stdout
