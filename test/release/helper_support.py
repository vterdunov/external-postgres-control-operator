"""Use real local packaging tools and fake only remote/container boundaries."""

import copy
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile

from support import (
    AMD64, ARM64, CHART, CHART_DIGEST, IMAGE, IMAGE_SHA, INDEX, REPOSITORY, ReleaseFixture,
    checksum, command_main,
)


ROOT = Path(__file__).resolve().parents[2]


PSA_GATE = "restricted namespace admits healthy manager and rejects privileged Pod"
CNI_GATE = "NetworkPolicy actual CNI allow-deny-allow and node probe continuity"
CNI_IMAGE = "docker.io/kindest/kindnetd:v20260512-fixture"


def security_lifecycle_gates():
    return [{"gate": PSA_GATE, "result": "passed", "admission": "actual API server"},
            {"gate": CNI_GATE, "result": "passed", "cniDaemonImages": [CNI_IMAGE],
             "metricsSameNamespace": "allowed", "metricsOtherNamespace": "denied", "podHealthPort": "denied"}]


def measured_runtime(manifest, directory):
    image = manifest["image"]
    subjects = [IMAGE + "@" + image["index_digest"], *[IMAGE + "@" + entry["digest"] for entry in image["platforms"]]]
    if manifest["stream"] == "chart":
        subjects.append(CHART + "@" + manifest["chart"]["digest"])
    reports = []
    for architecture, digest in (("amd64", AMD64), ("arm64", ARM64)):
        for postgres in (15, 16, 17, 18):
            reports.append({"source_sha": manifest["source_sha"], "image_source_sha": image["source_sha"], "subjects": subjects,
                            "pg_major": str(postgres), "context": f"kind-epco-e2e-{architecture}-pg{postgres}",
                            "observations": [{"imageID": "ghcr.io/example@" + digest, "architecture": architecture,
                                              "platform_digest": digest, "podUID": f"pod-{architecture}-pg{postgres}"}],
                            "leaseUID": f"lease-{architecture}-pg{postgres}",
                            "fixture_observed": "server observed version: " + {15: "15.19", 16: "16.15", 17: "17.11", 18: "18.6"}[postgres] + "\nclient observed version: 16.15\n"})
    gates = ("CR readiness, real application SQL and credential controls", "second release refused in same and other namespace",
             "same-UID removed named-table grant revoked in real SQL; unrelated access preserved",
             "unknown schema refused before schema or manager writes; known schema reinstalls",
             "same-package Helm upgrade and controller revision rollback mechanics",
             "live, terminating and real list-denied retirement refusal", "delivery lifecycle complete")
    archive = manifest.get("chart", {}).get("archive", "external-postgres-control-operator-0.0.0-image-validation.tgz")
    return {"repository": REPOSITORY, "stream": manifest["stream"], "tag": manifest["tag"], "source_sha": manifest["source_sha"],
            "image_source_sha": image["source_sha"], "image_index_digest": image["index_digest"],
            "chart_digest": manifest.get("chart", {}).get("digest"), "subjects": subjects,
            "gates": {"platforms": "success", "runtime": "success", "lifecycle": "success", "pg15-18": "success"},
            "observed_runtime": reports,
            "observed_lifecycle": {"scope": "selected artifact lifecycle", "imageSourceSHA": image["source_sha"],
                                   "chartSourceSHA": manifest["source_sha"], "imageReference": IMAGE + ":" + image["version"] + "@" + image["index_digest"],
                                   "chartArchiveSHA256": checksum((directory / archive).read_bytes()),
                                   "gates": [{"gate": gate, "result": "passed", **({"podImageID": "ghcr.io/example@" + AMD64} if index == 0 else {})}
                                             for index, gate in enumerate(gates)] + security_lifecycle_gates()}}


class PreparationFixture(ReleaseFixture):
    def __init__(self, directory, stream="chart"):
        helm = shutil.which("helm")
        if not helm:
            raise AssertionError("Pinned Helm is required; run preparation tests with mise")
        super().__init__(directory, stream)
        self.source = self.root / "source"
        (self.source / "tools").mkdir(parents=True)
        for name in ("release_ci.py", "release.py", "chart.py", "deploy.sh"):
            shutil.copyfile(ROOT / "tools" / name, self.source / "tools" / name)
        shutil.copytree(ROOT / "config", self.source / "config")
        shutil.copytree(ROOT / "charts", self.source / "charts")
        chart = self.source / "charts/external-postgres-control-operator/Chart.yaml"
        chart.write_text(chart.read_text().replace('version: 0.1.0', 'version: 0.2.1').replace('appVersion: "dev"', 'appVersion: "0.1.0"'))
        self.add_released_config()
        # The newer chart checkout's config deliberately differs from the
        # selected released application snapshot. It must not be packaged.
        for path in (self.source / "config/crd/bases").glob("*.yaml"):
            path.write_bytes(path.read_bytes() + b"\n# fixture chart HEAD differs from selected application\n")
        for tool in ("helm", "docker", "git", "kubectl"):
            executable = self.bin / tool
            executable.write_text(f"#!{sys.executable}\nfrom helper_support import helper_command_main\nhelper_command_main()\n")
            executable.chmod(0o755)
        self.state["real_helm"] = helm
        self.output = self.root / "prepared"
        self.helper = self.source / "tools/release_ci.py"
        self.save()

    def add_released_config(self):
        original = self.root / "accepted-image"
        old = copy.deepcopy(self.state["releases"].get("v0.1.0", {}).get("_manifest"))
        if old is None:
            old = json.loads((original / "manifest.json").read_text())
        removed = {entry["path"] for entry in old["assets"] if entry["kind"] in ("crds", "rbac")}
        old["assets"] = [entry for entry in old["assets"] if entry["kind"] not in ("crds", "rbac", "evidence")]
        for name in removed:
            (original / name).unlink()
        old["image"]["generated_inputs"] = []
        for source in sorted((ROOT / "config/crd/bases").glob("*.yaml")) + [ROOT / "config/rbac/role.yaml"]:
            name = "application-" + source.name
            shutil.copyfile(source, original / name)
            role = source.name == "role.yaml"
            entry = {"kind": "rbac" if role else "crds", "path": name, "sha256": checksum(source.read_bytes()),
                     "input_path": "rbac/role.yaml" if role else "crd/bases/" + source.name}
            old["assets"].append(entry)
            old["image"]["generated_inputs"].append({**entry, "source_sha": IMAGE_SHA})
        with tarfile.open(original / "application-config.tar.gz", "w:gz") as archive:
            archive.add(ROOT / "config", arcname="config")
        old["assets"].append({"kind": "application_config", "path": "application-config.tar.gz",
                              "sha256": checksum((original / "application-config.tar.gz").read_bytes())})
        evidence = copy.deepcopy(old)
        evidence["image_source_sha"] = IMAGE_SHA
        evidence["image_index_digest"] = INDEX
        evidence["chart_digest"] = None
        (original / "release-evidence.json").write_text(json.dumps(evidence))
        old["assets"].append({"kind": "evidence", "path": "release-evidence.json", "sha256": checksum((original / "release-evidence.json").read_bytes())})
        (original / "SHA256SUMS").write_text("".join(entry["sha256"] + "  " + entry["path"] + "\n" for entry in old["assets"]))
        (original / "manifest.json").write_text(json.dumps(old))
        if self.stream == "chart":
            self.state["releases"]["v0.1.0"] = self.release(old, original)
            self.manifest["image"] = copy.deepcopy(old["image"])

    def seed_path(self):
        seed = {key: self.manifest[key] for key in ("schema_version", "stream", "repository", "tag", "source_sha")}
        seed["completed"] = False
        if self.stream == "chart":
            seed["image"] = copy.deepcopy(self.manifest["image"])
        path = self.root / "preflight.json"
        path.write_text(json.dumps(seed))
        self.state["versions"].pop((CHART + ":0.2.1") if self.stream == "chart" else (IMAGE + ":0.1.0"), None)
        return path


def helper_command_main():
    tool, args = Path(sys.argv[0]).name, sys.argv[1:]
    state_path = Path(os.environ["EPCO_RELEASE_FIXTURE_STATE"])
    state = json.loads(state_path.read_text())
    def record(mutation=False):
        with Path(os.environ["EPCO_RELEASE_FIXTURE_LOG"]).open("a") as output:
            output.write(json.dumps({"tool": tool, "args": args, "mutation": mutation, "anonymous": False}) + "\n")
    if tool == "helm":
        record(bool(args and args[0] == "push"))
        if args[0] == "push":
            archive = Path(args[1]).resolve()
            with tarfile.open(archive, "r:gz") as package:
                metadata = package.extractfile("external-postgres-control-operator/Chart.yaml").read().decode()
            version = next(line.split(":", 1)[1].strip().strip('"') for line in metadata.splitlines() if line.startswith("version:"))
            state["chart_archive"] = str(archive)
            state["versions"][CHART + ":" + version] = CHART_DIGEST
            state_path.write_text(json.dumps(state))
            print("Fixture chart pushed")
            return
        result = subprocess.run([state["real_helm"], *args], check=False)
        raise SystemExit(result.returncode)
    if tool == "git":
        record()
        print(state.get("application_checkout_sha", IMAGE_SHA))
        return
    if tool == "kubectl":
        record()
        if "--context" not in args or "--kubeconfig" not in args or "get" not in args:
            raise SystemExit("Fixture requires explicit read-only kubeconfig/context")
        resource = args[args.index("get") + 1]
        key = {"pods": "kube_pods", "nodes": "kube_nodes", "lease": "kube_lease"}.get(resource)
        if key not in state:
            raise SystemExit("Kubernetes response fixture missing")
        print(json.dumps(state[key]))
        return
    if tool == "docker":
        record(args[:2] == ["buildx", "build"])
        if args[:2] == ["buildx", "build"]:
            metadata = Path(args[args.index("--metadata-file") + 1])
            metadata.write_text(json.dumps({"containerimage.digest": INDEX}))
            state["versions"][args[args.index("--tag") + 1]] = INDEX
            state_path.write_text(json.dumps(state))
            return
        if args[:2] == ["image", "inspect"]:
            native = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
            inspected = state.get("docker_inspect", {"Architecture": native, "Os": "linux", "Config": {
                "User": "65532:65532", "Labels": {"org.opencontainers.image.source": "https://github.com/" + REPOSITORY,
                    "org.opencontainers.image.revision": IMAGE_SHA, "org.opencontainers.image.version": "0.1.0"}}})
            print(json.dumps([inspected]))
            return
        if args and args[0] == "run":
            if state.get("docker_run_failure"):
                print("Fixture manager execution failed", file=sys.stderr)
                raise SystemExit(17)
            print(state.get("docker_help", "--leader-elect --health-probe-bind-address"))
            return
        raise SystemExit("Unexpected Docker fixture command")
    command_main()
