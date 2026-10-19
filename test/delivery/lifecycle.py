#!/usr/bin/env python3
"""Real Helm/SQL lifecycle smoke inside the e2e runner's owned Kind fixture.

This helper never creates a cluster or image. The existing e2e runner owns
bootstrap, bounded process lifetime, diagnostics and final cluster cleanup.
"""

import base64
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


ROOT = Path(__file__).resolve().parents[2]
API = "postgresql.epco.io/v1alpha1"
CRDS = [name + ".postgresql.epco.io" for name in ("externalpostgres", "databaseroles", "databases")]
DURABLE_STATUS = ("hasManagedSQL", "managedACLTargets", "activeACLTarget", "connectionSecretHistory",
                  "generatedPasswordInitialized", "managedEndpoints")


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def differing_paths(before, after, path="snapshot"):
    """Describe private comparisons using structural paths and types only."""
    if before == after:
        return []
    if isinstance(before, dict) and isinstance(after, dict):
        differences = []
        for key in sorted(before.keys() | after.keys()):
            child = path + "/" + key
            if key not in before or key not in after:
                differences.append({"path": child, "beforeType": type(before[key]).__name__ if key in before else "absent",
                                    "afterType": type(after[key]).__name__ if key in after else "absent"})
            else:
                differences.extend(differing_paths(before[key], after[key], child))
        return differences
    if isinstance(before, list) and isinstance(after, list) and len(before) == len(after):
        return [difference for index, (old, new) in enumerate(zip(before, after))
                for difference in differing_paths(old, new, path + "/" + str(index))]
    return [{"path": path, "beforeType": type(before).__name__, "afterType": type(after).__name__}]


class Smoke:
    def __init__(self):
        self.context = os.environ.get("EPCO_E2E_CONTEXT", "")
        require(self.context.startswith("kind-epco-e2e-"), "delivery smoke requires the runner-owned Kind context")
        kubeconfig = Path(os.environ.get("KUBECONFIG", ""))
        require(kubeconfig.is_file() and kubeconfig.stat().st_size > 0, "delivery smoke requires a private kubeconfig")
        self.package = Path(os.environ.get("EPCO_DELIVERY_CHART_PACKAGE", "")).resolve()
        require(self.package.is_file() and self.package.suffix == ".tgz", "supply an exact local chart package")
        self.image = os.environ.get("EPCO_DELIVERY_IMAGE_REFERENCE", "")
        self.local = os.environ.get("EPCO_DELIVERY_LOCAL_IMAGE", "false") == "true"
        require(self.image, "supply the selected application image")
        if not self.local:
            require(re.fullmatch(r"[^\s@:]+(?:/[^\s@:]+)*:[^\s@]+@sha256:[0-9a-f]{64}", self.image),
                    "delivered image must include a visible version and immutable digest")
        self.artifacts = Path(os.environ["EPCO_E2E_ARTIFACTS"])
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.pg_namespace = os.environ.get("EPCO_E2E_POSTGRES_NAMESPACE", "epco-postgres")
        self.application_root = Path(os.environ.get("EPCO_E2E_PROJECT_DIR", ROOT)).absolute()
        self.namespace = "epco-system"
        self.release = "epco-delivery"
        suffix = secrets.token_hex(5)
        self.app_namespace = "epco-delivery-" + suffix
        self.sql_prefix = "delivery_" + suffix
        self.database = self.sql_prefix + "_db"
        self.roles = {name: self.sql_prefix + "_" + name for name in ("owner", "supplied", "generated")}
        self.events = []
        self.environment = {**os.environ, "KUBE_CONTEXT": self.context, "IMG": self.image,
                            "ACK_CRD_RETIREMENT": "delete-all-epco-crds", "KUBECTL_KUBERC": "false"}
        self.kubectl = ["kubectl", "--kubeconfig", str(kubeconfig), "--context", self.context,
                        "--request-timeout=30s"]

    def command(self, *args, input_text=None, allow_failure=False, timeout=120, environment=None, cwd=None):
        result = subprocess.run(args, cwd=cwd or ROOT, env={**self.environment, **(environment or {})},
                                input=input_text, text=True, capture_output=True, timeout=timeout)
        if not allow_failure:
            require(result.returncode == 0, f"{Path(args[0]).name} operation failed (exit {result.returncode}); private output suppressed")
        return result

    def kube(self, *args, **kwargs):
        return self.command(*self.kubectl, *args, **kwargs)

    def get(self, resource, name=None, namespace=None):
        args = ["get", resource]
        if name:
            args.append(name)
        if namespace:
            args.extend(["-n", namespace])
        args.extend(["-o", "json"])
        return json.loads(self.kube(*args).stdout)

    def apply(self, resource):
        self.kube("apply", "-f", "-", input_text=json.dumps(resource))

    def event(self, name, **details):
        self.events.append({"gate": name, "result": "passed", **details})
        print(f"delivery smoke: {name} passed", flush=True)
        self.save_evidence()

    def save_evidence(self):
        evidence = {
            "scope": "local mechanics only" if self.local else "selected artifact lifecycle",
            "context": self.context,
            "imageReference": self.image,
            "chartArchiveSHA256": hashlib.sha256(self.package.read_bytes()).hexdigest(),
            "imageSourceSHA": os.environ.get("EPCO_DELIVERY_IMAGE_SOURCE_SHA", "unrecorded"),
            "chartSourceSHA": os.environ.get("EPCO_DELIVERY_CHART_SOURCE_SHA", "unrecorded"),
            "gates": self.events,
            "rollbackScope": "same-package revision mechanics; no cross-version compatibility claim",
        }
        (self.artifacts / "delivery-lifecycle.json").write_text(json.dumps(evidence, indent=2) + "\n")

    def poll(self, predicate, message, timeout=150):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                if predicate():
                    return
            except (AssertionError, KeyError, json.JSONDecodeError):
                pass
            time.sleep(1)
        raise AssertionError(message)

    def helm(self, *args, **kwargs):
        return self.command("helm", *args, "--kube-context", self.context, **kwargs)

    def install(self, release=None, namespace=None, allow_failure=False):
        return self.command(sys.executable, "tools/chart.py", "install", "--package", str(self.package),
                            "--release", release or self.release, "--namespace", namespace or self.namespace,
                            "--context", self.context, "--image-reference", self.image,
                            allow_failure=allow_failure, timeout=240)

    def ready_manager(self):
        self.kube("rollout", "status", "deployment/epco-controller-manager", "-n", self.namespace,
                  "--timeout=180s", timeout=200)
        pods = self.get("pods", namespace=self.namespace)["items"]
        managers = [pod for pod in pods if pod["metadata"].get("labels", {}).get("control-plane") == "controller-manager"
                    and not pod["metadata"].get("deletionTimestamp")]
        require(len(managers) == 1, "exactly one active manager Pod is required")
        pod = managers[0]
        require(all(container["ready"] for container in pod["status"]["containerStatuses"]), "manager containers must be Ready")
        require(pod["spec"]["containers"][0]["image"] == self.image, "manager must use selected image bytes")
        measured = pod["status"]["containerStatuses"][0]["imageID"]
        require(measured, "runtime imageID must be recorded")
        self.poll(lambda: (self.get("lease", "ccd893cc.epco.io", self.namespace)["spec"].get("holderIdentity", "")
                           .startswith(pod["metadata"]["name"] + "_")), "ready manager must hold its shared leader Lease", timeout=60)
        lease = self.get("lease", "ccd893cc.epco.io", self.namespace)
        self.event("manager readiness and selected image", podImageID=measured, leaseUID=lease["metadata"]["uid"])

    def no_managers(self):
        self.poll(lambda: not [pod for pod in self.get("pods", namespace=self.namespace)["items"]
                               if pod["metadata"].get("labels", {}).get("control-plane") == "controller-manager"],
                  "controller stop must remove every manager Pod", timeout=90)

    def sql(self, query, database="postgres", source=None, expected_state=None, allow_failure=False):
        # Credentials travel over stdin to an owned Pod, never argv or artifacts.
        script = "set -eu\nexport PGCONNECT_TIMEOUT=5\n"
        user = "postgres"
        if source:
            data = source["data"]
            user = base64.b64decode(data["user"]).decode()
            database = base64.b64decode(data["dbname"]).decode()
            password = base64.b64decode(data["password"]).decode()
            script += "export PGPASSWORD=" + shlex.quote(password) + "\n"
        script += "exec psql -X -h 127.0.0.1 -U " + shlex.quote(user) + " -d " + shlex.quote(database)
        script += " -v ON_ERROR_STOP=1 -v VERBOSITY=verbose -Atc " + shlex.quote(query) + "\n"
        result = self.kube("exec", "-i", "deployment/postgres", "-n", self.pg_namespace,
                           "--", "sh", "-s", input_text=script, allow_failure=True, timeout=30)
        if expected_state:
            require(result.returncode != 0 and expected_state in result.stderr,
                    "application query must fail with expected SQLSTATE " + expected_state)
        elif not allow_failure:
            require(result.returncode == 0, "fresh authenticated application/admin SQL failed; private diagnostics suppressed")
        return result

    def authentication_control(self, source):
        # psql does not report SQLSTATE for startup authentication failures.
        # Use the existing R6 approach: fresh pgx connections over an owned
        # loopback port-forward, with credentials carried only over stdin.
        with tempfile.TemporaryDirectory(prefix="epco-delivery-auth-") as temporary:
            directory = Path(temporary)
            probe = directory / "credential-probe"
            self.command("go", "build", "-mod=readonly", "-tags=deliveryprobe", "-o", str(probe),
                         "./test/delivery/fixtures/credential_probe.go")
            with (directory / "forward.log").open("w+") as log:
                forward = subprocess.Popen([*self.kubectl, "-n", self.pg_namespace, "port-forward",
                                            "--address=127.0.0.1", "service/postgres", "0:5432"],
                                           cwd=ROOT, env=self.environment, stdout=log, stderr=log)
                try:
                    port = None
                    deadline = time.monotonic() + 20
                    while time.monotonic() < deadline and forward.poll() is None:
                        log.seek(0)
                        match = re.search(r"Forwarding from 127\.0\.0\.1:(\d+) -> 5432", log.read())
                        if match:
                            port = int(match[1])
                            break
                        time.sleep(0.1)
                    require(port is not None, "owned authentication port-forward must become ready")
                    data = source["data"]
                    inputs = {"host": "127.0.0.1", "port": port,
                              "user": base64.b64decode(data["user"]).decode(),
                              "password": base64.b64decode(data["password"]).decode(),
                              "database": base64.b64decode(data["dbname"]).decode()}
                    accepted = json.loads(self.command(str(probe), input_text=json.dumps(inputs), timeout=15).stdout)
                    require(accepted["connected"] and accepted["sqlState"] == "",
                            "fresh typed application authentication control must accept exported credentials")
                    inputs["password"] = "known-invalid-delivery-control"
                    rejected = json.loads(self.command(str(probe), input_text=json.dumps(inputs), timeout=15).stdout)
                    require(not rejected["connected"] and rejected["sqlState"] == "28P01",
                            "fresh typed wrong-password control must fail with exact SQLSTATE 28P01")
                finally:
                    forward.terminate()
                    try:
                        forward.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        forward.kill()
                        forward.wait(timeout=5)

    def resource(self, kind, name, spec):
        return {"apiVersion": API, "kind": kind, "metadata": {"name": name, "namespace": self.app_namespace}, "spec": spec}

    def setup_application(self):
        self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": self.app_namespace}})
        fixture = self.get("secret", "postgres", self.pg_namespace)
        self.apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "admin", "namespace": self.app_namespace},
                    "type": "kubernetes.io/basic-auth", "data": {"username": base64.b64encode(b"postgres").decode(),
                                                                  "password": fixture["data"]["password"]}})
        self.apply(self.resource("ExternalPostgres", "postgres", {"connection": {"host": f"postgres.{self.pg_namespace}.svc.cluster.local"},
                    "credentialsSecret": {"name": "admin"}, "tls": {"mode": "disable"}, "reconcileInterval": "2s",
                    "connectTimeout": "2s", "statementTimeout": "2s"}))
        self.apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "supplied-source", "namespace": self.app_namespace},
                    "type": "kubernetes.io/basic-auth", "stringData": {"username": self.roles["supplied"], "password": secrets.token_urlsafe(24)}})
        for name, sql_name in self.roles.items():
            spec = {"cluster": {"name": "postgres"}, "name": sql_name, "login": name != "owner", "databaseRoleReclaimPolicy": "retain"}
            if name == "supplied":
                spec["passwordSecret"] = {"name": "supplied-source"}
            elif name == "generated":
                spec["passwordGeneration"] = {"secretName": "generated-source"}
            self.apply(self.resource("DatabaseRole", name, spec))
        self.database_spec = {
            "cluster": {"name": "postgres"}, "name": self.database, "owner": self.roles["owner"],
            "databaseReclaimPolicy": "retain",
            "permissions": {"connect": [{"name": self.roles[name]} for name in ("supplied", "generated")] + [{"name": "public", "ensure": "absent"}]},
            "schemas": [{"name": "app", "owner": self.roles["owner"], "permissions": {"usage": [{"name": self.roles[name]} for name in ("supplied", "generated")]}}],
            "connectionSecrets": [{"name": name + "-connection", "roleRef": {"name": name}} for name in ("supplied", "generated")],
        }
        self.apply(self.resource("Database", "application", self.database_spec))
        self.ready_application()
        self.sql(f'CREATE TABLE app.orders (value integer NOT NULL); ALTER TABLE app.orders OWNER TO "{self.roles["owner"]}"; INSERT INTO app.orders VALUES (42)', self.database)
        self.database_spec["schemas"][0]["tablePermissions"] = [{"name": "orders", "permissions": {"select": [{"name": self.roles[name]} for name in ("supplied", "generated")]}}]
        self.apply(self.resource("Database", "application", self.database_spec))
        self.ready_application()
        for name in ("supplied", "generated"):
            output = self.get("secret", name + "-connection", self.app_namespace)
            require(self.sql("SELECT value FROM app.orders", source=output).stdout.strip() == "42", "application must read persisted data using exported credentials")
        self.authentication_control(self.get("secret", "generated-connection", self.app_namespace))
        database = self.get("database", "application", self.app_namespace)
        require(database["status"].get("hasManagedSQL") and database["status"].get("managedACLTargets")
                and database["status"].get("connectionSecretHistory"), "fixture must possess durable SQL/ACL/output history")
        self.event("CR readiness, real application SQL and credential controls")

    def ready_application(self):
        for resource, names in (("externalpostgres", ["postgres"]), ("databaserole", list(self.roles)), ("database", ["application"])):
            for name in names:
                def current_ready(resource=resource, name=name):
                    value = self.get(resource, name, self.app_namespace)
                    status = value.get("status", {})
                    return status.get("observedGeneration") == value["metadata"]["generation"] and any(
                        item["type"] == "Ready" and item["status"] == "True" for item in status.get("conditions", []))
                self.poll(current_ready, resource + " must converge current generation")

    def snapshot(self):
        values = {}
        for resource, names in (("externalpostgres", ["postgres"]), ("databaserole", list(self.roles)), ("database", ["application"]),
                                ("secret", ["admin", "supplied-source", "generated-source", "supplied-connection", "generated-connection"])):
            for name in names:
                obj = self.get(resource, name, self.app_namespace)
                metadata = obj["metadata"]
                saved = {"uid": metadata["uid"], "finalizers": metadata.get("finalizers", []),
                         "ownerReferences": metadata.get("ownerReferences", []), "annotations": metadata.get("annotations", {})}
                if resource == "secret":
                    saved.update(data=obj.get("data", {}), type=obj.get("type"))
                else:
                    saved.update(spec=obj["spec"], status={field: obj.get("status", {}).get(field) for field in DURABLE_STATUS})
                values[resource + "/" + name] = saved
        values["application-namespace"] = self.get("namespace", self.app_namespace)["metadata"]["uid"]
        values["operator-namespace"] = self.get("namespace", self.namespace)["metadata"]["uid"]
        values["crds"] = {name: self.get("crd", name)["metadata"]["uid"] for name in CRDS}
        sql = f"""SELECT json_build_object(
            'database', (SELECT row_to_json(d) FROM (SELECT oid,datname,datdba,datacl FROM pg_database WHERE datname='{self.database}') d),
            'roles', (SELECT json_agg(r ORDER BY rolname) FROM (SELECT oid,rolname,rolcanlogin FROM pg_authid WHERE rolname LIKE '{self.sql_prefix}_%') r),
            'schema', (SELECT row_to_json(n) FROM (SELECT oid,nspname,nspowner,nspacl FROM pg_namespace WHERE nspname='app') n),
            'table', (SELECT row_to_json(t) FROM (SELECT oid,relowner,relacl FROM pg_class WHERE oid='app.orders'::regclass) t),
            'data', (SELECT json_agg(value ORDER BY value) FROM app.orders))"""
        values["sql"] = json.loads(self.sql(sql, self.database).stdout)
        return values

    def assert_retained(self, baseline, gate):
        current = self.snapshot()
        differences = differing_paths(baseline, current)
        if differences:
            diagnostics = {"gate": gate, "differences": differences}
            (self.artifacts / "delivery-retention-diff.json").write_text(json.dumps(diagnostics, indent=2) + "\n")
        require(not differences, gate + ": retained comparison failed; differing paths/types=" + json.dumps(differences))
        # Reconciliation encodes fresh random SCRAM salts even for the same
        # password. Compare exact retained Secret bytes above and prove their
        # continued authentication semantics with fresh positive/negative
        # connections, rather than comparing salted verifier encodings.
        for name in ("supplied", "generated"):
            output = baseline["secret/" + name + "-connection"]
            self.authentication_control(output)
            require(self.sql("SELECT current_user", source=output).stdout.strip() == self.roles[name],
                    gate + ": retained credentials must establish the same SQL identity")
        require(self.sql("SELECT value FROM app.orders", source=baseline["secret/supplied-connection"]).stdout.strip() == "42",
                gate + ": retained supplied credentials must read the same application data")
        self.event(gate, privateComparison="passed")

    def ownership_refusal(self):
        deployment = self.get("deployment", "epco-controller-manager", self.namespace)["metadata"]["uid"]
        for namespace in (self.namespace, "epco-delivery-other"):
            self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": namespace}})
            result = self.helm("install", "another-delivery", str(self.package), "--namespace", namespace,
                               "--skip-crds", allow_failure=True)
            require(result.returncode != 0 and "ownership" in (result.stdout + result.stderr).lower(), "second Helm release must fail actual ownership validation")
            require(self.get("deployment", "epco-controller-manager", self.namespace)["metadata"]["uid"] == deployment,
                    "second release must preserve the existing manager")
            if namespace != self.namespace:
                require(not self.get("deployments", namespace=namespace)["items"], "second namespace must contain no manager")
        self.event("second release refused in same and other namespace")

    def retention_and_revoke(self):
        self.ready_application()
        baseline = self.snapshot()
        self.helm("uninstall", self.release, "--namespace", self.namespace, "--wait", timeout=180)
        self.no_managers()
        self.assert_retained(baseline, "Helm stop retains all application state")
        self.install()
        self.ready_manager()
        self.ready_application()
        self.assert_retained(baseline, "reinstall retains live CR histories and credential provenance")
        old_uid = baseline["database/application"]["uid"]
        self.database_spec["schemas"][0]["tablePermissions"][0]["permissions"]["select"] = [{"name": self.roles["supplied"]}]
        self.apply(self.resource("Database", "application", self.database_spec))
        self.ready_application()
        require(self.get("database", "application", self.app_namespace)["metadata"]["uid"] == old_uid, "declarative revoke must occur within retained CR UID")
        self.sql("SELECT value FROM app.orders", source=self.get("secret", "generated-connection", self.app_namespace), expected_state="42501")
        require(self.sql("SELECT value FROM app.orders", source=self.get("secret", "supplied-connection", self.app_namespace)).stdout.strip() == "42",
                "unrelated supplied role access/data must survive managed revoke")
        self.event("same-UID removed named-table grant revoked in real SQL; unrelated access preserved")

    def legacy_routes(self):
        self.ready_application()
        baseline = self.snapshot()
        self.helm("uninstall", self.release, "--namespace", self.namespace, "--wait", timeout=180)
        self.no_managers()
        self.command("bash", "tools/deploy.sh", "build-installer", cwd=self.application_root)
        for route in ("kustomize", "installer"):
            if route == "kustomize":
                self.command("bash", "tools/deploy.sh", "deploy", cwd=self.application_root)
            else:
                self.kube("apply", "-f", str(self.application_root / "dist/install.yaml"))
            self.ready_manager()
            self.ready_application()
            deployment_uid = self.get("deployment", "epco-controller-manager", self.namespace)["metadata"]["uid"]
            refused = self.install(allow_failure=True)
            require(refused.returncode != 0 and "ownership" in (refused.stdout + refused.stderr).lower(), route + " to Helm must refuse automatic adoption")
            require(self.get("deployment", "epco-controller-manager", self.namespace)["metadata"]["uid"] == deployment_uid,
                    route + " ownership refusal must preserve existing manager")
            if route == "kustomize":
                self.command("bash", "tools/deploy.sh", "undeploy", cwd=self.application_root)
            else:
                self.kube("delete", "--ignore-not-found=true", "-f", str(self.application_root / "dist/uninstall.yaml"))
            self.no_managers()
            self.assert_retained(baseline, route + " stop retains API, CRs, durable histories, credentials and SQL")
        self.install()
        self.ready_manager()
        self.ready_application()

    def schema_refusal(self):
        self.helm("uninstall", self.release, "--namespace", self.namespace, "--wait", timeout=180)
        self.no_managers()
        name = "databases.postgresql.epco.io"
        original = self.get("crd", name)
        altered = copy.deepcopy(original["spec"])
        altered["versions"][0]["schema"]["openAPIV3Schema"]["properties"]["spec"]["properties"]["deliveryUnknown"] = {"type": "string"}
        self.kube("patch", "crd", name, "--type=merge", "-p", json.dumps({"spec": altered}))
        try:
            changed = self.get("crd", name)
            result = self.install(allow_failure=True)
            require(result.returncode != 0, "unknown retained schema must refuse manager startup")
            require(self.get("crd", name)["spec"] == changed["spec"], "refusal must not replace unknown/newer schema")
            require(not self.get("deployments", namespace=self.namespace)["items"], "schema refusal must create no manager")
        finally:
            # Restore the exact owned fixture schema, never any saved cluster.
            self.kube("patch", "crd", name, "--type=json", "-p", json.dumps([{"op": "replace", "path": "/spec", "value": original["spec"]}]))
        require(self.get("crd", name)["metadata"]["uid"] == original["metadata"]["uid"], "schema refusal/restoration must retain CRD UID")
        self.install()
        self.ready_manager()
        self.ready_application()
        self.event("unknown schema refused before schema or manager writes; known schema reinstalls")

    def revision_rollback(self):
        self.helm("upgrade", self.release, str(self.package), "--namespace", self.namespace, "--skip-crds",
                  "--reuse-values", "--set-string", "podAnnotations.deliveryRevision=second", "--wait", timeout=240)
        self.ready_manager()
        self.ready_application()
        self.helm("rollback", self.release, "1", "--namespace", self.namespace, "--wait", timeout=240)
        self.ready_manager()
        self.ready_application()
        self.event("same-package Helm upgrade and controller revision rollback mechanics")

    def retirement_refusal(self):
        self.helm("uninstall", self.release, "--namespace", self.namespace, "--wait", timeout=180)
        self.no_managers()
        baseline = self.snapshot()
        result = self.command("bash", "tools/deploy.sh", "uninstall", allow_failure=True)
        require(result.returncode != 0, "CRD retirement must refuse live application CRs")
        self.assert_retained(baseline, "live-CR retirement refuses with no resource loss")
        # A real permission denial proves fail-closed reads, rather than a fake CLI error.
        with tempfile.TemporaryDirectory(prefix="epco-delivery-denied-") as temporary:
            real_kubectl = shutil.which("kubectl")
            require(real_kubectl, "kubectl is required")
            wrapper = Path(temporary) / "kubectl"
            wrapper.write_text("#!/bin/sh\nexec " + shlex.quote(real_kubectl) + ' "$@" --as=epco-delivery-denied\n')
            wrapper.chmod(0o700)
            denied = self.command("bash", "tools/deploy.sh", "uninstall", allow_failure=True,
                                  environment={"PATH": temporary + os.pathsep + os.environ["PATH"]})
            require(denied.returncode != 0 and "forbidden" in (denied.stdout + denied.stderr).lower(), "retirement list permission failure must refuse")
        self.assert_retained(baseline, "real RBAC list denial retains every CRD and application object")
        terminating = self.resource("DatabaseRole", "terminating", {"cluster": {"name": "postgres"}, "name": self.sql_prefix + "_terminating"})
        terminating["metadata"]["finalizers"] = ["delivery.epco.test/owned-retirement-control"]
        self.apply(terminating)
        self.kube("delete", "databaserole", "terminating", "-n", self.app_namespace, "--wait=false")
        require(self.get("databaserole", "terminating", self.app_namespace)["metadata"].get("deletionTimestamp"), "terminating control must actually await its finalizer")
        require(self.command("bash", "tools/deploy.sh", "uninstall", allow_failure=True).returncode != 0,
                "retirement must refuse terminating CRs")
        require({name: self.get("crd", name)["metadata"]["uid"] for name in CRDS} == baseline["crds"],
                "terminating CR retirement must retain CRDs")
        self.event("live, terminating and real list-denied retirement refusal")
        # Leave finalizers intact; the runner removes its entire disposable Kind cluster.

    def run(self):
        from security_test import prepare_restricted_namespace, assert_real_restricted_admission, assert_real_network_policy

        for tool, args in (("helm", ("version", "--short")), ("kind", ("version",))):
            result = self.command(tool, *args)
            self.event(tool + " version", version=result.stdout.strip())
        self.event("kubectl version", version=self.kube("version", "-o", "json").stdout.strip())
        current = self.command(*self.kubectl, "config", "current-context").stdout.strip()
        require(current == self.context, "private kubeconfig must select the owned context")
        require(not self.get("deployments", namespace=self.namespace)["items"] if self.kube("get", "namespace", self.namespace, allow_failure=True).returncode == 0 else True,
                "runner delivery dispatch must skip default manager install")
        prepare_restricted_namespace(self)
        self.install()
        self.ready_manager()
        assert_real_restricted_admission(self)
        assert_real_network_policy(self)
        self.setup_application()
        self.ownership_refusal()
        self.retention_and_revoke()
        self.legacy_routes()
        self.schema_refusal()
        self.revision_rollback()
        self.retirement_refusal()
        self.event("delivery lifecycle complete")


if __name__ == "__main__":
    try:
        Smoke().run()
    except (AssertionError, subprocess.TimeoutExpired, OSError, KeyError, json.JSONDecodeError) as error:
        print("delivery lifecycle failed: " + str(error), file=sys.stderr)
        sys.exit(1)
