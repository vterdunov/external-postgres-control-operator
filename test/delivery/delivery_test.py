"""R7a local packaging and orchestration contracts, without cluster writes."""

import json
import re
import shutil
import sys
import tomllib
import unittest

from support import CHART, DIGEST, IMAGE, KINDS, DeliveryCase, documents, kind


def named(text, resource_kind, name=None):
    matches = [doc for doc in documents(text) if kind(doc) == resource_kind]
    if name:
        matches = [doc for doc in matches if re.search(r"(?m)^  name:\s*" + re.escape(name) + r"\s*$", doc)]
    if len(matches) != 1:
        raise AssertionError(f"expected one {resource_kind} {name or ''}, found {len(matches)}")
    return matches[0]


def rule_tokens(text):
    rules = text.split("\nrules:", 1)[1]
    return [re.sub(r"[\s\"']", "", line.split("#", 1)[0]) for line in rules.splitlines()
            if line.strip() and not line.lstrip().startswith("#")]


class RemovalTests(DeliveryCase):
    def test_undeploy_preserves_namespace_crds_all_cr_history_and_credentials(self):
        result = self.run_cli("bash", "tools/deploy.sh", "undeploy")
        self.assert_success(result)
        expected = [item for item in self.inventory if item["kind"] not in {"Deployment", "ClusterRole"}]
        self.assertEqual(self.remaining(), expected, "controller stop must retain API and all application state")
        self.assertTrue(any("delete" in call["args"] for call in self.calls()), "controller must actually stop")

    def test_installer_has_distinct_complete_and_controller_only_bundles(self):
        result = self.run_cli("bash", "tools/deploy.sh", "build-installer", environment={"KUBE_CONTEXT": ""})
        self.assert_success(result)
        install = (self.repo / "dist/install.yaml").read_text()
        removal_path = self.repo / "dist/uninstall.yaml"
        self.assertTrue(removal_path.exists(), "installer must include a safe removal bundle")
        removal = removal_path.read_text()
        install_kinds = {kind(doc) for doc in documents(install)}
        removal_kinds = {kind(doc) for doc in documents(removal)}
        self.assertTrue({"Namespace", "CustomResourceDefinition", "Deployment"}.issubset(install_kinds))
        self.assertIn(IMAGE, install, "installer must preserve visible version plus immutable digest")
        self.assertIn("Deployment", removal_kinds)
        self.assertFalse({"Namespace", "CustomResourceDefinition", "Secret", "ExternalPostgres", "DatabaseRole", "Database"} & removal_kinds)
        self.assert_no_mutations()

    def test_crd_retirement_requires_acknowledgement_and_explicit_context(self):
        for variable in ("ACK_CRD_RETIREMENT", "KUBE_CONTEXT"):
            with self.subTest(variable=variable):
                (self.directory / "calls.jsonl").write_text("")
                (self.directory / "inventory.json").write_text(json.dumps(self.inventory))
                result = self.run_cli("bash", "tools/deploy.sh", "uninstall", environment={variable: ""})
                self.assertNotEqual(result.returncode, 0, f"retirement requires {variable}")
                self.assert_no_mutations()

    def test_crd_retirement_checks_every_kind_all_namespaces_before_delete(self):
        result = self.run_cli("bash", "tools/deploy.sh", "uninstall")
        self.assert_success(result)
        reads = [call for call in self.calls() if "get" in call["args"]]
        for resource in KINDS:
            matching = [call for call in reads if resource + ".postgresql.epco.io" in call["args"]]
            self.assertEqual(len(matching), 1, f"must list {resource} exactly once")
            self.assertIn("--all-namespaces", matching[0]["args"])
            self.assertTrue("json" in matching[0]["args"] or "--output=json" in matching[0]["args"] or "-o=json" in matching[0]["args"])
        writes = [index for index, call in enumerate(self.calls()) if "delete" in call["args"]]
        self.assertTrue(writes)
        self.assertGreater(min(writes), max(index for index, call in enumerate(self.calls()) if "get" in call["args"]))
        self.assertEqual(self.remaining(), [item for item in self.inventory if item["kind"] != "CustomResourceDefinition"])

    def test_retirement_refuses_live_terminating_failed_and_malformed_lists_without_writes(self):
        for resource in KINDS:
            for state in ([{"metadata": {"uid": "live"}}],
                          [{"metadata": {"uid": "terminating", "deletionTimestamp": "2026-10-12T00:00:00Z", "finalizers": ["epco"]}}],
                          "failure", "malformed"):
                with self.subTest(resource=resource, state=state):
                    (self.directory / "calls.jsonl").write_text("")
                    (self.directory / "inventory.json").write_text(json.dumps(self.inventory))
                    self.lists = {name: [] for name in KINDS}
                    self.lists[resource] = state
                    self.save_lists()
                    result = self.run_cli("bash", "tools/deploy.sh", "uninstall")
                    self.assertNotEqual(result.returncode, 0, "unsafe retirement must refuse")
                    self.assert_no_mutations()
                    self.assertEqual(self.remaining(), self.inventory)


class ImageTests(DeliveryCase):
    def test_buildx_uses_the_catalog_pinned_mirror_for_its_builder(self):
        catalog = json.loads((self.repo / "test/images/catalog.json").read_text())["images"]
        entry = next(image for image in catalog if image["name"] == "buildkit")
        expected = entry["destination"] + "@" + entry["source"].rsplit("@", 1)[1]
        result = self.run_cli("bash", "tools/image.sh", "buildx")
        self.assert_success(result)
        creates = [call for call in self.calls() if call["args"][:2] == ["buildx", "create"]]
        self.assertEqual(len(creates), 1)
        args = creates[0]["args"]
        options = [argument.split("=", 1)[1] for argument in args if argument.startswith("--driver-opt=")]
        options.extend(args[index + 1] for index, argument in enumerate(args[:-1]) if argument == "--driver-opt")
        self.assertIn("image=" + expected, options, "Buildx must not pull its unpinned default builder")

    def test_buildx_uses_only_supported_platforms(self):
        result = self.run_cli("bash", "tools/image.sh", "buildx")
        self.assert_success(result)
        build = next(call for call in self.calls() if "build" in call["args"])
        args = build["args"]
        platforms = next(arg.split("=", 1)[1] for arg in args if arg.startswith("--platform="))
        self.assertEqual(set(platforms.split(",")), {"linux/amd64", "linux/arm64"})

    def test_buildx_uses_authoritative_dockerfile_without_rewriting(self):
        original = (self.repo / "Dockerfile").read_text()
        result = self.run_cli("bash", "tools/image.sh", "buildx")
        self.assert_success(result)
        build = next(call for call in self.calls() if "build" in call["args"])
        self.assertEqual(build["dockerfile"], original, "buildx must not rewrite the Dockerfile")
        self.assertEqual((self.repo / "Dockerfile").read_text(), original)

    def test_native_build_preserves_base_override_and_image(self):
        result = self.run_cli("bash", "tools/image.sh", "build", environment={"BASE_IMAGE": "local/base:fixture"})
        self.assert_success(result)
        args = self.calls()[0]["args"]
        self.assertIn("BASE_IMAGE=local/base:fixture", args)
        self.assertEqual(args[args.index("-t") + 1], IMAGE)


class ChartTests(DeliveryCase):
    def test_helm_is_pinned_and_chart_is_application_without_dependencies(self):
        tools = tomllib.loads((self.repo / "mise.toml").read_text())["tools"]
        self.assertIn("helm", tools, "render/release gates require project-pinned Helm")
        self.assertRegex(tools["helm"], r"^\d+\.\d+\.\d+$")
        self.assertTrue((self.repo / CHART / "values.schema.json").exists())
        metadata = (self.repo / CHART / "Chart.yaml").read_text()
        self.assertRegex(metadata, r"(?m)^apiVersion: v2$")
        self.assertRegex(metadata, r"(?m)^type: application$")
        self.assertNotRegex(metadata, r"(?m)^dependencies:")
        self.assert_success(self.helm("lint", str(CHART), "--strict"))

    def test_render_keeps_tag_and_digest_and_chart_patch_preserves_app_identity(self):
        metadata = self.repo / CHART / "Chart.yaml"
        self.assertTrue(metadata.exists(), "R7a chart is required")
        source = metadata.read_text()
        self.assertRegex(source, r'(?m)^appVersion:\s*"[^\"]+"\s*$')
        overrides = ("--set", "image.repository=ghcr.io/vterdunov/external-postgres-control-operator",
                     "--set-string", "image.tag=0.2.3", "--set-string", "image.digest=" + DIGEST)
        before = self.render(*overrides)
        metadata.write_text(re.sub(r"(?m)^version:.*$", "version: 0.1.1", source))
        after = self.render(*overrides)
        self.assertIn(IMAGE, before)
        self.assertIn(IMAGE, after)
        self.assertEqual(re.findall(r"(?m)^\s+image:\s*(.+)$", before), re.findall(r"(?m)^\s+image:\s*(.+)$", after))
        self.assertEqual(re.search(r"(?m)^appVersion:.*$", source).group(), re.search(r"(?m)^appVersion:.*$", metadata.read_text()).group())
        tag_only = self.render("--set-string", "image.tag=0.2.3", "--set-string", "image.digest=")
        self.assertRegex(tag_only, r"(?m)^\s+image:.*:0\.2\.3[\"']?$")

    def test_required_manager_settings_and_no_application_or_namespace_resources(self):
        rendered = self.render()
        forbidden = {"Namespace", "Secret", "ExternalPostgres", "DatabaseRole", "Database", "Certificate", "ServiceMonitor", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"}
        self.assertFalse(forbidden.intersection(kind(doc) for doc in documents(rendered)))
        manager = named(rendered, "Deployment", "epco-controller-manager")
        for pattern in (r"runAsNonRoot:\s*true", r"type:\s*RuntimeDefault", r"readOnlyRootFilesystem:\s*true",
                        r"allowPrivilegeEscalation:\s*false", r"drop:\s*\n\s*-\s*[\"']?ALL", r"--leader-elect",
                        r"--health-probe-bind-address=:8081", r"--metrics-bind-address=:8443",
                        r"livenessProbe:[\s\S]*path:\s*/healthz", r"readinessProbe:[\s\S]*path:\s*/readyz"):
            self.assertRegex(manager, pattern)
        self.assertNotRegex(manager, r"--(?:watch-namespace|metrics-secure=false|leader-elect=false)")
        named(rendered, "ClusterRoleBinding", "epco-metrics-auth-rolebinding")
        named(rendered, "RoleBinding", "epco-leader-election-rolebinding")
        self.assertIn("subjectaccessreviews", rendered)
        self.assertIn("tokenreviews", rendered)
        disabled = self.render("--set", "metrics.enabled=false")
        self.assertNotIn("--metrics-bind-address=:8443", disabled)
        self.assertIn("--metrics-bind-address=0", disabled)

    def test_cluster_ownership_sentinel_is_fixed_across_release_and_namespace(self):
        first = self.render()
        second = self.render(release="another-release", namespace="another-namespace")
        for rendered in (first, second):
            role = named(rendered, "ClusterRole", "epco-manager-role")
            self.assertIn("app.kubernetes.io/managed-by:", role)
            self.assertIn("Helm", role)
            self.assertEqual(rule_tokens(role), rule_tokens((self.repo / "config/rbac/role.yaml").read_text()))
        values = (self.repo / CHART / "values.yaml").read_text()
        self.assertNotRegex(values, r"(?m)^(?:nameOverride|fullnameOverride|clusterRoleName):")

    def test_schema_rejects_invalid_digest_and_arbitrary_manager_flags(self):
        for arguments in (("--set-string", "image.digest=sha256:not-a-digest"),
                          ("--set", "extraArgs[0]=--leader-elect=false")):
            with self.subTest(arguments=arguments):
                result = self.helm("template", "epco", str(CHART), *arguments)
                self.assertNotEqual(result.returncode, 0, "unsafe/invalid values must fail schema validation")

    def test_generated_inputs_sync_and_read_only_check_use_selected_source(self):
        chart = self.repo / CHART
        self.assertTrue(chart.exists(), "R7a chart is required")
        selected = self.directory / "selected-inputs"
        shutil.copytree(self.repo / "config", selected)
        role_path = selected / "rbac/role.yaml"
        role_path.write_text(role_path.read_text() + "- apiGroups:\n  - provenance.epco.test\n  resources:\n  - selected-input\n  verbs:\n  - get\n")
        crd_path = selected / "crd/bases/postgresql.epco.io_databases.yaml"
        crd_path.write_text(crd_path.read_text() + "\n# selected application source fixture\n")
        command = (sys.executable, "tools/chart.py")
        flags = ("--chart-dir", str(chart), "--inputs-dir", str(selected))
        self.assert_success(self.run_cli(*command, "sync", *flags))
        for source in (selected / "crd/bases").glob("*.yaml"):
            self.assertEqual((chart / "crds" / source.name).read_bytes(), source.read_bytes())
        role = named(self.render(), "ClusterRole", "epco-manager-role")
        self.assertEqual(rule_tokens(role), rule_tokens(role_path.read_text()))
        self.assertIn("selected-input", role)
        self.assert_success(self.run_cli(*command, "check", *flags))
        copied = chart / "crds" / crd_path.name
        copied.write_text(copied.read_text() + "\n# stale selected input\n")
        before = {str(path.relative_to(chart)): path.read_bytes() for path in chart.rglob("*") if path.is_file()}
        self.assertNotEqual(self.run_cli(*command, "check", *flags).returncode, 0)
        self.assertEqual(before, {str(path.relative_to(chart)): path.read_bytes() for path in chart.rglob("*") if path.is_file()},
                         "provenance check must detect drift without silently repairing it")


if __name__ == "__main__":
    unittest.main(verbosity=2)
