"""Chart orchestration must use the selected application's legacy config."""

import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

from support import CHART_SHA, IMAGE, IMAGE_SHA, INDEX


ROOT = Path(__file__).resolve().parents[2]


class LegacySourcePairingTests(unittest.TestCase):
    def test_chart_lifecycle_legacy_routes_use_selected_application_workspace(self):
        specification = importlib.util.spec_from_file_location("epco_delivery_lifecycle_test", ROOT / "test/delivery/lifecycle.py")
        lifecycle = importlib.util.module_from_spec(specification)
        specification.loader.exec_module(lifecycle)
        with tempfile.TemporaryDirectory(prefix="epco-source-pairing-") as temporary:
            directory = Path(temporary)
            application = directory / "old-application"
            (application / "config").mkdir(parents=True)
            (application / "config/selected-source.txt").write_text(IMAGE_SHA)
            (application / "tools").mkdir()
            (application / "tools/deploy.sh").write_text("#!/usr/bin/env bash\nexit 0\n")
            (application / "dist").mkdir()
            for name in ("install.yaml", "uninstall.yaml"):
                (application / "dist" / name).write_text("selected application " + IMAGE_SHA)
            kubeconfig = directory / "kubeconfig"
            kubeconfig.write_text("private fixture kubeconfig")
            package = directory / "chart-0.2.1.tgz"
            package.write_bytes(b"owned smoke fixture")
            environment = {"EPCO_E2E_CONTEXT": "kind-epco-e2e-owned", "KUBECONFIG": str(kubeconfig),
                           "EPCO_E2E_ARTIFACTS": str(directory / "artifacts"), "EPCO_DELIVERY_CHART_PACKAGE": str(package),
                           "EPCO_DELIVERY_IMAGE_REFERENCE": IMAGE + ":0.1.0@" + INDEX,
                           "EPCO_DELIVERY_IMAGE_SOURCE_SHA": IMAGE_SHA, "EPCO_DELIVERY_CHART_SOURCE_SHA": CHART_SHA,
                           "EPCO_E2E_PROJECT_DIR": str(application), "EPCO_DELIVERY_PROJECT_DIR": str(application)}
            captured = []
            def execute(arguments, **kwargs):
                captured.append((arguments, kwargs))
                return subprocess.CompletedProcess(arguments, 0, "", "")
            with mock.patch.dict(os.environ, environment):
                smoke = lifecycle.Smoke()
            with mock.patch.object(lifecycle.subprocess, "run", side_effect=execute), \
                    mock.patch.object(smoke, "ready_application"), mock.patch.object(smoke, "ready_manager"), \
                    mock.patch.object(smoke, "no_managers"), mock.patch.object(smoke, "assert_retained"), \
                    mock.patch.object(smoke, "snapshot", return_value={}), mock.patch.object(smoke, "helm"), \
                    mock.patch.object(smoke, "get", return_value={"metadata": {"uid": "original-manager"}}), \
                    mock.patch.object(smoke, "install", return_value=subprocess.CompletedProcess([], 1, "", "ownership refusal")):
                smoke.legacy_routes()
            deploy = [(args, kwargs) for args, kwargs in captured if args[:2] == ("bash", "tools/deploy.sh")]
            self.assertEqual(len(deploy), 3, "Both legacy routes require build/deploy/undeploy checks")
            for args, kwargs in deploy:
                self.assertEqual(Path(kwargs["cwd"]).resolve(), application.resolve(),
                                 "Chart HEAD cannot supply legacy CRDs/RBAC for the selected old image")
                self.assertEqual(kwargs["env"]["IMG"], environment["EPCO_DELIVERY_IMAGE_REFERENCE"])
            bundle_commands = [args for args, _ in captured if "-f" in args]
            self.assertEqual({str(application / "dist/install.yaml"), str(application / "dist/uninstall.yaml")},
                             {str(args[args.index("-f") + 1]) for args in bundle_commands})


if __name__ == "__main__":
    unittest.main()
