"""Helm security defaults plus real PSA admission checks for owned fixtures."""

import copy
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest

from support import DeliveryCase, documents


def rendered_objects(rendered, environment, directory):
    """Use the pinned Helm YAML parser on actual rendered objects."""
    parser_chart = Path(tempfile.mkdtemp(prefix="security-parser-", dir=directory))
    (parser_chart / "templates").mkdir(parents=True)
    (parser_chart / "objects").mkdir()
    (parser_chart / "Chart.yaml").write_text("apiVersion: v2\nname: epco-security-parser\nversion: 0.0.0\n")
    paths = []
    for index, document in enumerate(documents(rendered)):
        name = f"objects/{index}.yaml"
        (parser_chart / name).write_text(document)
        paths.append(json.dumps(name))
    template = '{"apiVersion":"v1","kind":"List","items":['
    template += "{{ range $index, $path := list " + " ".join(paths) + " }}"
    template += "{{ if $index }},{{ end }}{{ $.Files.Get $path | fromYaml | toJson }}{{ end }}]}\n"
    (parser_chart / "templates/objects.yaml").write_text(template)
    result = subprocess.run(["helm", "template", "security-parser", str(parser_chart)],
                            env=environment, text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise AssertionError("rendered security object parsing failed: " + result.stderr)
    return json.loads(result.stdout[result.stdout.index("{"):])["items"]


def restricted_violations(pod_spec):
    """EPCO's declared restricted defaults, independently inspected by tests."""
    violations = []
    context = pod_spec.get("securityContext", {})
    if context.get("runAsNonRoot") is not True:
        violations.append("pod.runAsNonRoot")
    if context.get("runAsUser") == 0:
        violations.append("pod.runAsUser")
    if context.get("runAsUser") != 65532 or context.get("runAsGroup") != 65532:
        violations.append("pod.declaredUIDGID")
    if context.get("seccompProfile", {}).get("type") != "RuntimeDefault":
        violations.append("pod.seccompProfile")
    for key in ("hostNetwork", "hostPID", "hostIPC"):
        if pod_spec.get(key, False):
            violations.append("pod." + key)
    for container in pod_spec.get("containers", []):
        context = container.get("securityContext", {})
        if context.get("privileged") is not False:
            violations.append("container.privileged")
        if context.get("allowPrivilegeEscalation") is not False:
            violations.append("container.allowPrivilegeEscalation")
        if context.get("readOnlyRootFilesystem") is not True:
            violations.append("container.readOnlyRootFilesystem")
        capabilities = context.get("capabilities", {})
        if capabilities.get("drop") != ["ALL"] or capabilities.get("add", []):
            violations.append("container.capabilities")
        if context.get("runAsUser") == 0 or context.get("runAsNonRoot") is False:
            violations.append("container.nonroot")
        if context.get("seccompProfile", {}).get("type") == "Unconfined":
            violations.append("container.seccompProfile")
        if any("hostPath" in volume for volume in pod_spec.get("volumes", [])):
            violations.append("pod.hostPath")
    return violations


def prepare_restricted_namespace(smoke):
    """Namespace policy is owned by the disposable test, never the Helm chart."""
    version = json.loads(smoke.kube("version", "-o", "json").stdout)["serverVersion"]
    minor = "".join(character for character in version["minor"] if character.isdigit())
    policy_version = "v" + version["major"] + "." + minor
    smoke.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {
        "name": smoke.namespace,
        "labels": {"pod-security.kubernetes.io/enforce": "restricted",
                   "pod-security.kubernetes.io/enforce-version": policy_version},
    }})


def assert_real_restricted_admission(smoke):
    """Check actual admitted manager and a rejected privileged Pod via the API."""
    namespace = smoke.get("namespace", smoke.namespace)
    if namespace["metadata"].get("labels", {}).get("pod-security.kubernetes.io/enforce") != "restricted":
        raise AssertionError("owned manager namespace must enforce Restricted Pod Security")
    deployment = smoke.get("deployment", "epco-controller-manager", smoke.namespace)
    specification = deployment["spec"]["template"]["spec"]
    violations = restricted_violations(specification)
    if violations:
        raise AssertionError("installed manager security violations: " + ", ".join(violations))
    pods = [pod for pod in smoke.get("pods", namespace=smoke.namespace)["items"]
            if pod["metadata"].get("labels", {}).get("control-plane") == "controller-manager"
            and not pod["metadata"].get("deletionTimestamp")]
    if len(pods) != 1 or restricted_violations(pods[0]["spec"]):
        raise AssertionError("actual admitted manager Pod must retain restricted security settings")
    control = {"apiVersion": "v1", "kind": "Pod", "metadata": {
        "name": "epco-security-admission-control", "namespace": smoke.namespace}, "spec": copy.deepcopy(specification)}
    good = smoke.kube("create", "--dry-run=server", "-f", "-", "-o", "json",
                      input_text=json.dumps(control), allow_failure=True)
    if good.returncode != 0:
        raise AssertionError("actual manager Pod specification must pass restricted server admission")
    control["spec"]["containers"][0]["securityContext"]["privileged"] = True
    control["spec"]["containers"][0]["securityContext"]["allowPrivilegeEscalation"] = True
    bad = smoke.kube("create", "--dry-run=server", "-f", "-", "-o", "json",
                     input_text=json.dumps(control), allow_failure=True)
    diagnostic = bad.stdout + bad.stderr
    if bad.returncode == 0 or "violates PodSecurity" not in diagnostic or "privileged" not in diagnostic:
        raise AssertionError("privileged negative control must fail actual Pod Security admission")
    smoke.event("restricted namespace admits healthy manager and rejects privileged Pod", admission="actual API server")


def assert_real_network_policy(smoke):
    """Prove actual CNI allow/deny/allow behavior, rather than API existence."""
    policies = smoke.get("networkpolicies", namespace=smoke.namespace)["items"]
    if len(policies) != 1:
        raise AssertionError("default manager namespace must contain exactly one chart NetworkPolicy")
    policy = policies[0]
    original = copy.deepcopy(policy["spec"])
    if original.get("policyTypes") != ["Ingress"]:
        raise AssertionError("default policy must preserve arbitrary PostgreSQL/API/DNS egress")
    manager = smoke.get("deployment", "epco-controller-manager", smoke.namespace)
    if original["podSelector"] != {"matchLabels": manager["spec"]["selector"]["matchLabels"]}:
        raise AssertionError("NetworkPolicy must select only the exact manager group")
    pods = [pod for pod in smoke.get("pods", namespace=smoke.namespace)["items"]
            if pod["metadata"].get("labels", {}).get("control-plane") == "controller-manager"
            and not pod["metadata"].get("deletionTimestamp")]
    if len(pods) != 1:
        raise AssertionError("network control requires one actual manager Pod")
    address = pods[0]["status"]["podIP"]
    source_name = "epco-policy-probe"
    image = smoke.environment.get("EPCO_E2E_CLIENT_IMAGE")
    if not image:
        raise AssertionError("network control requires the existing pinned PostgreSQL client image")
    for namespace in (smoke.namespace, smoke.pg_namespace):
        smoke.apply({"apiVersion": "v1", "kind": "Pod", "metadata": {
            "name": source_name, "namespace": namespace, "labels": {"delivery.epco.test/network-probe": "owned"}},
            "spec": {"restartPolicy": "Never", "securityContext": {
                "runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [{"name": "probe", "image": image, "command": ["bash", "-c", "exec sleep 600"],
                    "securityContext": {"privileged": False, "allowPrivilegeEscalation": False,
                                        "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                    "resources": {"requests": {"cpu": "5m", "memory": "16Mi"}, "limits": {"memory": "64Mi"}}}]}})
        smoke.kube("wait", "--for=condition=Ready", "pod/" + source_name, "-n", namespace, "--timeout=120s", timeout=140)

    script = ('timeout 4 bash -c \'exec 3<>"/dev/tcp/$1/$2"\' control "$1" "$2" >/dev/null 2>&1; '
              'status=$?; case "$status" in 0) printf connected;; 124) printf timed-out;; *) printf refused;; esac')

    def probe(namespace, port):
        result = smoke.kube("exec", source_name, "-n", namespace, "--", "bash", "-c", script,
                            "control", address, str(port), allow_failure=True, timeout=15)
        if result.returncode:
            raise AssertionError("network probe command failed before reporting connectivity")
        return result.stdout.strip()

    def expect(namespace, port, expected, explanation):
        smoke.poll(lambda: probe(namespace, port) == expected, explanation, timeout=60)

    try:
        expect(smoke.namespace, 8443, "connected", "same-namespace metrics peer must connect before denial control")
        expect(smoke.pg_namespace, 8443, "timed-out", "other-namespace peer must be blocked from manager metrics")
        expect(smoke.namespace, 8081, "timed-out", "NetworkPolicy must not expose node health port to peer Pods")
        denied = copy.deepcopy(original)
        denied["ingress"] = []
        name = policy["metadata"]["name"]
        smoke.kube("patch", "networkpolicy", name, "-n", smoke.namespace, "--type=json", "-p",
                   json.dumps([{"op": "replace", "path": "/spec", "value": denied}]))
        expect(smoke.namespace, 8443, "timed-out", "actual CNI must enforce the denied same-peer control")
    finally:
        # Restore the exact owned fixture policy before checking connectivity.
        smoke.kube("patch", "networkpolicy", policy["metadata"]["name"], "-n", smoke.namespace,
                   "--type=json", "-p", json.dumps([{"op": "replace", "path": "/spec", "value": original}]))
    try:
        expect(smoke.namespace, 8443, "connected", "actual CNI must restore allowed same-peer connectivity")
        live = smoke.get("pod", pods[0]["metadata"]["name"], smoke.namespace)
        if not all(container["ready"] for container in live["status"]["containerStatuses"]):
            raise AssertionError("node-origin health probes must remain Ready throughout manager ingress isolation")
        daemonsets = smoke.get("daemonsets", namespace="kube-system")["items"]
        images = [container["image"] for daemonset in daemonsets if "kindnet" in daemonset["metadata"]["name"]
                  for container in daemonset["spec"]["template"]["spec"]["containers"]]
        if not images:
            raise AssertionError("actual owned Kind CNI daemon image must be recorded")
        smoke.event("NetworkPolicy actual CNI allow-deny-allow and node probe continuity", cniDaemonImages=images,
                    metricsSameNamespace="allowed", metricsOtherNamespace="denied", podHealthPort="denied")
    finally:
        for namespace in (smoke.namespace, smoke.pg_namespace):
            smoke.kube("delete", "pod", source_name, "-n", namespace, "--wait=true", "--timeout=60s", timeout=80)


class SecurityDefaultsTests(DeliveryCase):
    def parsed_render(self, *overrides):
        rendered = self.render(*overrides)
        return rendered_objects(rendered, self.environment, self.directory)

    def test_default_pod_security_is_explicit_and_restricted(self):
        objects = self.parsed_render()
        manager = next(obj for obj in objects if obj["kind"] == "Deployment")
        specification = manager["spec"]["template"]["spec"]
        self.assertEqual(restricted_violations(specification), [])
        self.assertFalse({"Namespace", "Pod", "Job"}.intersection(obj["kind"] for obj in objects),
                         "security policy must not add chart-owned namespaces or test/admission hooks")

    def test_security_defaults_preserve_probes_resources_leader_and_authenticated_metrics(self):
        manager = next(obj for obj in self.parsed_render() if obj["kind"] == "Deployment")
        container = manager["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["livenessProbe"]["httpGet"], {"path": "/healthz", "port": 8081})
        self.assertEqual(container["readinessProbe"]["httpGet"], {"path": "/readyz", "port": 8081})
        for bound in ("requests", "limits"):
            for resource in ("cpu", "memory"):
                self.assertTrue(container["resources"][bound][resource])
                self.assertNotEqual(str(container["resources"][bound][resource]), "0")
        self.assertIn("--leader-elect", container["args"])
        self.assertIn("--metrics-bind-address=:8443", container["args"])
        self.assertNotIn("--metrics-secure=false", container["args"])
        self.assertTrue(manager["spec"]["template"]["spec"]["serviceAccountName"],
                        "manager requires its authenticated Kubernetes API identity")

    def test_values_cannot_bypass_the_declared_security_context(self):
        for unsafe in ("podSecurityContext.runAsNonRoot=false", "securityContext.privileged=true",
                       "extraArgs[0]=--metrics-secure=false"):
            with self.subTest(unsafe=unsafe):
                result = self.helm("template", "epco", "charts/external-postgres-control-operator", "--set", unsafe)
                self.assertNotEqual(result.returncode, 0, "unknown unsafe values must fail schema validation")

    def test_privileged_admission_negative_control_is_internally_consistent(self):
        manager = next(obj for obj in self.parsed_render() if obj["kind"] == "Deployment")
        controls, events = [], []

        def get(resource, name=None, namespace=None):
            if resource == "namespace":
                return {"metadata": {"labels": {"pod-security.kubernetes.io/enforce": "restricted"}}}
            if resource == "deployment":
                return manager
            return {"items": [{"metadata": {"labels": {"control-plane": "controller-manager"}},
                               "spec": manager["spec"]["template"]["spec"]}]}

        def kube(*args, input_text, allow_failure):
            self.assertIn("--dry-run=server", args)
            control = json.loads(input_text)
            controls.append(control)
            context = control["spec"]["containers"][0]["securityContext"]
            if context["privileged"]:
                self.assertTrue(context["allowPrivilegeEscalation"],
                                "privileged control must pass intrinsic Kubernetes validation before PSA")
                return SimpleNamespace(returncode=1, stdout="", stderr='violates PodSecurity "restricted": privileged')
            return SimpleNamespace(returncode=0, stdout=json.dumps(control), stderr="")

        # This checks control construction only; actual API admission remains the owned Kind gate.
        smoke = SimpleNamespace(namespace="epco-system", get=get, kube=kube,
                                event=lambda name, **details: events.append({"gate": name, **details}))
        assert_real_restricted_admission(smoke)
        self.assertEqual(len(controls), 2)
        self.assertFalse(controls[0]["spec"]["containers"][0]["securityContext"]["privileged"])
        self.assertEqual(events, [{"gate": "restricted namespace admits healthy manager and rejects privileged Pod",
                                   "admission": "actual API server"}])

    def test_default_network_policy_selects_only_manager_and_allows_authenticated_metrics(self):
        objects = self.parsed_render()
        manager = next(obj for obj in objects if obj["kind"] == "Deployment")
        policies = [obj for obj in objects if obj["kind"] == "NetworkPolicy"]
        self.assertEqual(len(policies), 1, "default chart must isolate manager ingress")
        policy = policies[0]
        self.assertEqual(policy["metadata"]["namespace"], "epco-system")
        self.assertEqual(policy["spec"]["podSelector"], {"matchLabels": manager["spec"]["selector"]["matchLabels"]})
        self.assertEqual(policy["spec"]["policyTypes"], ["Ingress"], "arbitrary SQL/DNS/API egress remains available by default")
        self.assertNotIn("egress", policy["spec"])
        self.assertEqual(policy["spec"].get("ingress"), [{"from": [{"podSelector": {}}],
                                                       "ports": [{"protocol": "TCP", "port": 8443}]}])

    def test_network_policy_disablement_and_metrics_disablement_are_explicit(self):
        disabled = self.parsed_render("--set", "networkPolicy.enabled=false")
        self.assertFalse([obj for obj in disabled if obj["kind"] == "NetworkPolicy"])
        no_metrics = self.parsed_render("--set", "metrics.enabled=false")
        policies = [obj for obj in no_metrics if obj["kind"] == "NetworkPolicy"]
        self.assertEqual(len(policies), 1)
        self.assertEqual(policies[0]["spec"]["policyTypes"], ["Ingress"])
        self.assertEqual(policies[0]["spec"].get("ingress", []), [])

    def test_explicit_monitoring_peers_and_ipv4_ipv6_egress_rules_render_without_broadening(self):
        peers = [{"namespaceSelector": {"matchLabels": {"purpose": "monitoring"}},
                  "podSelector": {"matchLabels": {"app": "scraper"}}}]
        rules = [
            {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                     "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
             "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]},
            {"to": [{"ipBlock": {"cidr": "192.0.2.0/24", "except": ["192.0.2.1/32"]}}],
             "ports": [{"protocol": "TCP", "port": 5432}]},
            {"to": [{"ipBlock": {"cidr": "2001:db8::/32", "except": ["2001:db8:1::/48"]}}],
             "ports": [{"protocol": "TCP", "port": 443}]},
        ]
        objects = self.parsed_render("--set-json", "networkPolicy.metricsIngressPeers=" + json.dumps(peers),
                                     "--set", "networkPolicy.egress.enabled=true",
                                     "--set-json", "networkPolicy.egress.rules=" + json.dumps(rules))
        policy = next(obj for obj in objects if obj["kind"] == "NetworkPolicy")
        self.assertEqual(set(policy["spec"]["policyTypes"]), {"Ingress", "Egress"})
        self.assertEqual(policy["spec"]["ingress"], [{"from": peers, "ports": [{"protocol": "TCP", "port": 8443}]}])
        self.assertEqual(policy["spec"]["egress"], rules)

    def test_egress_is_fail_closed_for_empty_or_malformed_explicit_rules(self):
        invalid = [[], [{}], [{"to": [{"ipBlock": {"cidr": "not-a-cidr"}}]}],
                   [{"ports": [{"port": 0}]}], [{"ports": [{"protocol": "INVALID", "port": 443}]}],
                   [{"to": [{"ipBlock": {"cidr": "192.0.2.0/24"}, "podSelector": {}}]}],
                   [{"unknownRuleKey": "value"}]]
        for rules in invalid:
            with self.subTest(rules=rules):
                result = self.helm("template", "epco", "charts/external-postgres-control-operator",
                                   "--set", "networkPolicy.egress.enabled=true",
                                   "--set-json", "networkPolicy.egress.rules=" + json.dumps(rules))
                self.assertNotEqual(result.returncode, 0, "explicit egress policy requires nonempty valid rules")

    def test_network_policy_unknown_configuration_keys_fail_schema_validation(self):
        for value in ("networkPolicy.allowAll=true", "networkPolicy.egress.allowAll=true"):
            with self.subTest(value=value):
                result = self.helm("template", "epco", "charts/external-postgres-control-operator", "--set", value)
                self.assertNotEqual(result.returncode, 0)


FAKE_INSTALL_HELM = '''import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
args = sys.argv[1:]
record = {"args": args}
if args[0] in {"upgrade", "install"}:
    paths = [args[index + 1] for index, arg in enumerate(args) if arg in {"--values", "-f"}]
    record["values"] = [Path(path).read_text() for path in paths]
    package = next(Path(arg) for arg in args if arg.endswith(".tgz"))
    record["packageSHA256"] = hashlib.sha256(package.read_bytes()).hexdigest()
    with (Path(os.environ["DELIVERY_STATE"]) / "helm-calls.jsonl").open("a") as stream:
        stream.write(json.dumps(record) + "\\n")
    sys.exit(0)
sys.exit(subprocess.run([os.environ["DELIVERY_REAL_HELM"], *args]).returncode)
'''


class SecurityInstallValuesTests(DeliveryCase):
    def package_fixture(self):
        self.assert_success(self.run_cli(sys.executable, "tools/chart.py", "sync", "--chart-dir",
                                        "charts/external-postgres-control-operator", "--inputs-dir", "config"))
        objects = json.loads((self.repo / "charts/external-postgres-control-operator/generated/crds.json").read_text())
        (self.directory / "schema-objects.json").write_text(json.dumps(objects))
        self.assert_success(self.helm("package", "charts/external-postgres-control-operator", "--destination", str(self.directory)))
        package = next(self.directory.glob("*.tgz"))
        real_helm = shutil.which("helm")
        self.assertTrue(real_helm)
        fake = self.bin / "helm"
        fake.write_text("#!" + sys.executable + "\n" + FAKE_INSTALL_HELM)
        fake.chmod(0o700)
        self.environment.update(DELIVERY_REAL_HELM=real_helm, DELIVERY_INSTALL_VALUES_ONLY="true")
        return package

    def install(self, package, *values):
        return self.run_cli(sys.executable, "tools/chart.py", "install", "--package", str(package),
                            "--release", "epco", "--namespace", "epco-system", "--context", "kind-epco-delivery-owned", *values)

    def test_verified_package_installs_with_repeatable_values_in_order_without_source_edits(self):
        package = self.package_fixture()
        initial = package.read_bytes()
        first = self.directory / "first-values.json"
        second = self.directory / "second-values.json"
        first.write_text(json.dumps({"replicaCount": 2, "resources": {"requests": {"cpu": "20m", "memory": "64Mi"}}}))
        second.write_text(json.dumps({"replicaCount": 3, "podAnnotations": {"delivery-values": "second"}}))
        first_bytes, second_bytes = first.read_bytes(), second.read_bytes()
        result = self.install(package, "--values", str(first), "--values", str(second))
        self.assert_success(result)
        calls = [json.loads(line) for line in (self.directory / "helm-calls.jsonl").read_text().splitlines()]
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["values"], [first.read_text(), second.read_text()], "selected values order must reach the same verified Helm package")
        self.assertEqual(calls[0]["packageSHA256"], hashlib.sha256(initial).hexdigest())
        self.assertEqual(package.read_bytes(), initial)
        self.assertEqual(first.read_bytes(), first_bytes)
        self.assertEqual(second.read_bytes(), second_bytes)

    def test_missing_or_invalid_selected_values_refuse_before_any_cluster_write(self):
        package = self.package_fixture()
        invalid = self.directory / "invalid-values.json"
        invalid.write_text(json.dumps({"replicaCount": 0}))
        for path in (self.directory / "missing-values.yaml", invalid):
            with self.subTest(path=path.name):
                (self.directory / "calls.jsonl").write_text("")
                result = self.install(package, "--values", str(path))
                self.assertNotEqual(result.returncode, 0)
                self.assert_no_mutations()
                self.assertFalse((self.directory / "helm-calls.jsonl").exists())

    def test_values_snapshot_preserves_validated_bytes_if_caller_file_changes_after_preflight(self):
        package = self.package_fixture()
        selected = self.directory / "changing-values.json"
        initial = json.dumps({"replicaCount": 2})
        selected.write_text(initial)
        self.environment["DELIVERY_VALUES_CHANGE_PATH"] = str(selected)
        result = self.install(package, "--values", str(selected))
        self.assert_success(result)
        self.assertNotEqual(selected.read_text(), initial, "fixture must actually change caller file after local validation")
        calls = [json.loads(line) for line in (self.directory / "helm-calls.jsonl").read_text().splitlines()]
        self.assertEqual(calls[0]["values"], [initial], "same validated values bytes must reach Helm despite caller-file changes")

    def test_invalid_cidr_exceptions_refuse_before_persistent_cluster_writes(self):
        package = self.package_fixture()
        cases = [("192.0.2.0/24", "198.51.100.0/24"),
                 ("192.0.2.0/24", "192.0.2.0/24"),
                 ("192.0.2.0/24", "2001:db8::/64"),
                 ("2001:db8::/32", "2001:db9::/48")]
        for parent, exception in cases:
            with self.subTest(parent=parent, exception=exception):
                (self.directory / "calls.jsonl").write_text("")
                values = self.directory / "policy-values.json"
                values.write_text(json.dumps({"networkPolicy": {"egress": {"enabled": True, "rules": [{
                    "to": [{"ipBlock": {"cidr": parent, "except": [exception]}}],
                    "ports": [{"protocol": "TCP", "port": 5432}]}]}}}))
                self.environment.update(DELIVERY_SERVER_DRY_RUN_FAILURE="must be a strict subset of cidr",
                                        DELIVERY_EXPECTED_API_POLICY=json.dumps([parent, exception]))
                result = self.install(package, "--values", str(values))
                self.assertNotEqual(result.returncode, 0, "API-invalid CIDR exception must refuse installation")
                self.assertIn("strict subset", result.stdout + result.stderr)
                self.assert_no_mutations()
                dry_runs = [call for call in self.calls() if call.get("serverDryRun")]
                self.assertEqual(len(dry_runs), 1, "exact selected rendered policy must be validated by the API before writes")

    def test_valid_strict_subset_exceptions_pass_actual_server_preflight_before_writes(self):
        package = self.package_fixture()
        values = self.directory / "policy-values.json"
        for parent, exception in (("192.0.2.0/24", "192.0.2.1/32"), ("2001:db8::/32", "2001:db8:1::/48")):
            with self.subTest(parent=parent, exception=exception):
                (self.directory / "calls.jsonl").write_text("")
                rules = [{"to": [{"ipBlock": {"cidr": parent, "except": [exception]}}],
                          "ports": [{"protocol": "TCP", "port": 5432}]}]
                values.write_text(json.dumps({"networkPolicy": {"egress": {"enabled": True, "rules": rules}}}))
                self.environment["DELIVERY_EXPECTED_API_POLICY"] = json.dumps([parent, exception])
                self.assert_success(self.install(package, "--values", str(values)))
                calls = self.calls()
                dry_runs = [index for index, call in enumerate(calls) if call.get("serverDryRun")]
                writes = [index for index, call in enumerate(calls) if call.get("persistentMutation")]
                self.assertEqual(len(dry_runs), 1, "API semantic preflight is required even for valid values")
                self.assertTrue(writes, "valid policy must proceed to install after API validation")
                self.assertLess(dry_runs[0], min(writes))


if __name__ == "__main__":
    unittest.main(verbosity=2)
