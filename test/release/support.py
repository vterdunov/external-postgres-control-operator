"""Owned, offline command fixtures for release behavior tests."""

import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
from urllib.parse import parse_qs, urlparse
from urllib.parse import unquote
import zipfile
import uuid


REPOSITORY = "vterdunov/external-postgres-control-operator"
IMAGE = f"ghcr.io/{REPOSITORY}"
CHART = "ghcr.io/vterdunov/charts/external-postgres-control-operator"
IMAGE_SHA = "1" * 40
CHART_SHA = "2" * 40
INDEX = "sha256:" + "a" * 64
AMD64 = "sha256:" + "b" * 64
ARM64 = "sha256:" + "c" * 64
CHART_DIGEST = "sha256:" + "d" * 64
ISSUER = "https://token.actions.githubusercontent.com"
GATES = {
    "test.yml": ["test-unit", "test-envtest", "check-generated", "PostgreSQL 15", "PostgreSQL 16"],
    "lint.yml": ["Run on Ubuntu"],
    "test-e2e.yml": ["E2E"],
    "delivery.yml": ["Release validation", "Platforms"],
}


def checksum(data):
    return hashlib.sha256(data).hexdigest()


def identity(stream, tag, source):
    workflow = f"release-{stream}.yml"
    return {
        "identity": f"https://github.com/{REPOSITORY}/.github/workflows/{workflow}@refs/tags/{tag}",
        "issuer": ISSUER, "repository": REPOSITORY, "ref": f"refs/tags/{tag}",
        "trigger": "push", "source_sha": source,
    }


def bundle(subject, claims):
    # A standardized-format fixture, not a valid cryptographic signature.
    return {
        "mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
        "verificationMaterial": {"certificate": {"rawBytes": "ZmFrZQ=="}, "tlogEntries": []},
        "messageSignature": {
            "messageDigest": {"algorithm": "SHA2_256", "digest": base64.b64encode(subject.encode()).decode()},
            "signature": "ZmFrZQ==",
        },
        "fixture_claims": claims,
    }


class ReleaseFixture:
    def __init__(self, directory, stream="chart", chart_version="0.2.1"):
        self.root = Path(directory)
        self.root.mkdir(parents=True, exist_ok=True)
        self.stream = stream
        self.chart_version = chart_version
        self.log = self.root / "commands.jsonl"
        self.state_path = self.root / "scenario.json"
        self.manifest_path = self.root / "candidate" / "manifest.json"
        self.manifest_path.parent.mkdir()
        self.manifest = self.make_manifest(self.manifest_path.parent, stream)
        image_dir = self.root / "accepted-image"
        image_dir.mkdir()
        image_manifest = self.make_manifest(image_dir, "image")
        self.state = self.make_state(image_manifest, image_dir)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for tool in ("gh", "crane", "cosign", "helm", "docker"):
            script = self.bin / tool
            script.write_text(f"#!{sys.executable}\nfrom support import command_main\ncommand_main()\n")
            script.chmod(0o755)
        auth = self.root / "authenticated"
        auth.mkdir()
        (auth / "config.json").write_text(json.dumps({"auths": {"ghcr.io": {"auth": "fixture-only"}}}))
        self.env = {
            **os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "PYTHONPATH": str(Path(__file__).parent),
            "EPCO_RELEASE_FIXTURE_STATE": str(self.state_path),
            "EPCO_RELEASE_FIXTURE_LOG": str(self.log), "DOCKER_CONFIG": str(auth),
            "GH_TOKEN": "fixture-token-not-a-credential", "GITHUB_REPOSITORY": REPOSITORY,
            "GITHUB_SHA": self.manifest["source_sha"],
            "GITHUB_REF": "refs/tags/" + self.manifest["tag"], "GITHUB_EVENT_NAME": "push",
            "GITHUB_RUN_ID": "900", "GITHUB_RUN_ATTEMPT": "1",
        }
        self.env.pop("EPCO_RELEASE_IMMUTABILITY_PRECHECK", None)
        self.save()

    def make_manifest(self, directory, stream):
        version, tag, source = ("0.1.0", "v0.1.0", IMAGE_SHA) if stream == "image" else (self.chart_version, "chart-v" + self.chart_version, CHART_SHA)
        image_claims = identity("image", "v0.1.0", IMAGE_SHA)
        manifest = {
            "schema_version": 1, "stream": stream, "repository": REPOSITORY,
            "tag": tag, "source_sha": source, "required_workflows": GATES,
            "image": {
                "repository": IMAGE, "version": "0.1.0", "release_tag": "v0.1.0",
                "source_sha": IMAGE_SHA, "index_digest": INDEX, "bundle": "image-index.sigstore.json",
                "platforms": [
                    {"os": "linux", "architecture": "amd64", "digest": AMD64, "bundle": "image-amd64.sigstore.json"},
                    {"os": "linux", "architecture": "arm64", "digest": ARM64, "bundle": "image-arm64.sigstore.json"},
                ],
                "generated_inputs": [],
            },
            "assets": [], "checksum_manifest": "SHA256SUMS", "checksum_bundle": "SHA256SUMS.sigstore.json",
            "runtime_evidence": {"run_id": 900, "run_attempt": 1, "artifact_id": 901},
        }
        def asset(name, data, kind):
            (directory / name).write_bytes(data)
            entry = {"kind": kind, "path": name, "sha256": checksum(data)}
            manifest["assets"].append(entry)
            return entry
        for name, kind, data in (
            ("application-crds.yaml", "crds", b"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n"),
            ("application-rbac.yaml", "rbac", b"apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\n"),
        ):
            entry = asset(name, data, kind)
            manifest["image"]["generated_inputs"].append({**entry, "source_sha": IMAGE_SHA})
        for name, digest in (("image-index.sigstore.json", INDEX), ("image-amd64.sigstore.json", AMD64), ("image-arm64.sigstore.json", ARM64)):
            asset(name, json.dumps(bundle(IMAGE + "@" + digest, image_claims)).encode(), "signature_bundle")
        if stream == "chart":
            archive = f"external-postgres-control-operator-{version}.tgz"
            data = io.BytesIO()
            with tarfile.open(fileobj=data, mode="w:gz") as output:
                for name, contents in {
                    "Chart.yaml": f'apiVersion: v2\nname: external-postgres-control-operator\nversion: {version}\nappVersion: "0.1.0"\n',
                    "values.yaml": f"image:\n  repository: {IMAGE}\n  tag: 0.1.0\n  digest: {INDEX}\n",
                    "crds/application-crds.yaml": (directory / "application-crds.yaml").read_text(),
                }.items():
                    payload = contents.encode()
                    header = tarfile.TarInfo("external-postgres-control-operator/" + name)
                    header.size = len(payload)
                    output.addfile(header, io.BytesIO(payload))
            asset(archive, data.getvalue(), "chart")
            manifest["chart"] = {
                "repository": CHART, "version": version, "app_version": "0.1.0",
                "digest": CHART_DIGEST, "archive": archive, "bundle": "chart.sigstore.json",
            }
            asset("chart.sigstore.json", json.dumps(bundle(CHART + "@" + CHART_DIGEST, identity(stream, tag, source))).encode(), "signature_bundle")
        else:
            asset("installer.yaml", b"kind: Deployment\n", "installer")
            asset("controller-removal.yaml", b"kind: Deployment\n", "controller_removal")
        evidence = json.loads(json.dumps(manifest))
        evidence.update({"image_source_sha": IMAGE_SHA, "image_index_digest": INDEX,
                         "chart_digest": CHART_DIGEST if stream == "chart" else None})
        asset("release-evidence.json", json.dumps(evidence).encode(), "evidence")
        sums = "".join(f'{entry["sha256"]}  {entry["path"]}\n' for entry in manifest["assets"])
        (directory / "SHA256SUMS").write_text(sums)
        (directory / "SHA256SUMS.sigstore.json").write_text(json.dumps(bundle("SHA256SUMS", identity(stream, tag, source))))
        (directory / "manifest.json").write_text(json.dumps(manifest))
        return manifest

    def make_state(self, image_manifest, image_dir):
        source = self.manifest["source_sha"]
        runs, jobs = [], {}
        for index, (workflow, names) in enumerate(GATES.items(), 100):
            runs.append({"id": index, "workflow_id": index, "path": f".github/workflows/{workflow}",
                         "head_sha": source, "head_branch": "main", "event": "push", "status": "completed",
                         "conclusion": "success", "run_attempt": 1, "created_at": "2026-10-12T00:00:00Z",
                         "repository": {"full_name": REPOSITORY}, "head_repository": {"full_name": REPOSITORY}})
            jobs[str(index)] = [{"name": name, "status": "completed", "conclusion": "success"} for name in names]
        claims = identity(self.stream, self.manifest["tag"], source)
        runtime = {"id": 900, "path": f'.github/workflows/release-{self.stream}.yml', "head_sha": source,
                   "head_branch": self.manifest["tag"], "event": "push", "status": "in_progress",
                   "conclusion": None, "run_attempt": 1, "repository": {"full_name": REPOSITORY},
                   "head_repository": {"full_name": REPOSITORY}}
        jobs["900"] = [{"name": "Runtime acceptance", "status": "completed", "conclusion": "success"}]
        runtime_payload = {
            "repository": REPOSITORY, "stream": self.stream, "tag": self.manifest["tag"], "source_sha": source,
            "image_source_sha": IMAGE_SHA, "image_index_digest": INDEX,
            "chart_digest": CHART_DIGEST if self.stream == "chart" else None,
            "subjects": [IMAGE + "@" + INDEX, IMAGE + "@" + AMD64, IMAGE + "@" + ARM64]
            + ([CHART + "@" + CHART_DIGEST] if self.stream == "chart" else []),
            "gates": {"platforms": "success", "runtime": "success", "lifecycle": "success", "pg15-18": "success"},
        }
        release = self.release(image_manifest, image_dir)
        return {
            "runs": runs, "jobs": jobs, "runtime_run": runtime,
            "runtime_artifact": {"id": 901, "name": "release-runtime-1", "expired": False,
                                 "workflow_run": {"id": 900, "head_sha": source}},
            "runtime_payload": runtime_payload,
            "releases": {"v0.1.0": release} if self.stream == "chart" else {},
            "tag_sha": source, "on_main": True, "failures": [], "private": [],
            "tag_reads": 0,
            "versions": {IMAGE + ":0.1.0": INDEX, CHART + ":" + self.chart_version: CHART_DIGEST},
            "subjects": {IMAGE + "@" + digest: identity("image", "v0.1.0", IMAGE_SHA)
                         for digest in (INDEX, AMD64, ARM64)}
            | {CHART + "@" + CHART_DIGEST: claims},
            "image_manifest": {
                "schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
                "manifests": [
                    {"digest": AMD64, "mediaType": "application/vnd.oci.image.manifest.v1+json", "platform": {"os": "linux", "architecture": "amd64"}},
                    {"digest": ARM64, "mediaType": "application/vnd.oci.image.manifest.v1+json", "platform": {"os": "linux", "architecture": "arm64"}},
                    {"digest": "sha256:" + "e" * 64, "platform": {"os": "unknown", "architecture": "unknown"},
                     "annotations": {"vnd.docker.reference.type": "attestation-manifest"}},
                ],
            },
            "chart_metadata": {"version": self.chart_version, "appVersion": "0.1.0"}, "uploads": [],
        }

    @staticmethod
    def release(manifest, directory, immutable=True, draft=False):
        return {
            "id": 700, "tag_name": manifest["tag"], "target_commitish": manifest["source_sha"],
            "immutable": immutable, "draft": draft, "prerelease": "-" in manifest["tag"].removeprefix("chart-v").removeprefix("v"),
            "assets": [{"id": 800 + index, "name": path.name, "size": path.stat().st_size,
                        "digest": "sha256:" + checksum(path.read_bytes())}
                       for index, path in enumerate(directory.iterdir()) if path.name != "manifest.json"],
            "_directory": str(directory), "_manifest": manifest,
        }

    def save(self):
        self.manifest_path.write_text(json.dumps(self.manifest))
        self.state_path.write_text(json.dumps(self.state))

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def fail(self, tool, contains, message="fixture verification failure", code=1):
        self.state["failures"].append({"tool": tool, "contains": contains, "message": message, "code": code})


def command_main():
    tool, args = Path(sys.argv[0]).name, sys.argv[1:]
    state_path = Path(os.environ["EPCO_RELEASE_FIXTURE_STATE"])
    state = json.loads(state_path.read_text())
    auth_file = Path(os.environ.get("DOCKER_CONFIG", "")) / "config.json"
    anonymous = not auth_file.is_file() or not json.loads(auth_file.read_text()).get("auths")
    mutation = (tool == "gh" and args[:2] in (["release", "create"], ["release", "upload"], ["release", "edit"], ["release", "delete"])) or (tool == "gh" and any(method in args for method in ("POST", "PATCH", "PUT", "DELETE"))) or (tool == "cosign" and args and args[0] in ("sign", "sign-blob", "attest", "clean")) or (tool == "helm" and args and args[0] == "push") or (tool in ("crane", "docker") and args and args[0] in ("push", "tag", "copy", "build", "buildx"))
    with Path(os.environ["EPCO_RELEASE_FIXTURE_LOG"]).open("a") as log:
        log.write(json.dumps({"tool": tool, "args": args, "mutation": bool(mutation), "anonymous": anonymous}) + "\n")
    def emit(value):
        print(json.dumps(value))
    def fail(message, code=1):
        print(message, file=sys.stderr)
        raise SystemExit(code)
    def value(flag, default=None):
        for index, argument in enumerate(args):
            if argument == flag and index + 1 < len(args):
                return args[index + 1]
            if argument.startswith(flag + "="):
                return argument.split("=", 1)[1]
        return default
    for failure in state["failures"]:
        if failure["tool"] == tool and all(part in " ".join(args) for part in failure["contains"]):
            fail(failure["message"], failure["code"])
    if tool == "gh":
        if args and args[0] == "api":
            endpoint = next((arg for arg in args[1:] if arg.startswith("repos/")), "")
            if "/actions/artifacts/" in endpoint and endpoint.endswith("/zip"):
                output = io.BytesIO()
                with zipfile.ZipFile(output, "w") as archive:
                    archive.writestr("release-runtime.json", json.dumps(state["runtime_payload"]))
                sys.stdout.buffer.write(output.getvalue())
            elif "/actions/runs/900/artifacts" in endpoint:
                emit({"total_count": 1, "artifacts": [state["runtime_artifact"]]})
            elif "/actions/runs/" in endpoint and "/jobs" in endpoint:
                run_id = endpoint.split("/actions/runs/", 1)[1].split("/", 1)[0]
                attempt = endpoint.split("/attempts/", 1)[1].split("/", 1)[0] if "/attempts/" in endpoint else None
                job_list = state.get("jobs_by_attempt", {}).get(run_id + ":" + str(attempt), state["jobs"].get(run_id, []))
                emit({"total_count": len(job_list), "jobs": job_list})
            elif endpoint.endswith("/actions/runs/900"):
                emit(state["runtime_run"])
            elif "/actions/runs/900/attempts/" in endpoint:
                attempt = endpoint.split("/attempts/", 1)[1].split("?", 1)[0]
                emit(state.get("runtime_attempts", {}).get(attempt, state["runtime_run"]))
            elif "/actions/" in endpoint and "runs" in endpoint:
                runs = state["runs"]
                if "/workflows/" in endpoint:
                    workflow = endpoint.split("/workflows/", 1)[1].split("/", 1)[0]
                    runs = [run for run in runs if run["path"].removeprefix(".github/workflows/").split("@", 1)[0] == workflow]
                query = parse_qs(urlparse(endpoint).query)
                for index, argument in enumerate(args):
                    if argument in ("-f", "-F", "--field", "--raw-field") and index + 1 < len(args) and "=" in args[index + 1]:
                        key, val = args[index + 1].split("=", 1)
                        query[key] = [val]
                for key, field in (("status", "conclusion"), ("head_sha", "head_sha"), ("branch", "head_branch"), ("event", "event")):
                    if key in query:
                        runs = [run for run in runs if run.get(field) == query[key][0] or (key == "status" and run.get("status") == query[key][0])]
                runs = sorted(runs, key=lambda run: (run.get("created_at", ""), run["id"]), reverse=True)
                emit({"total_count": len(runs), "workflow_runs": runs})
            elif "/releases/tags/" in endpoint:
                tag = endpoint.split("/releases/tags/", 1)[1]
                release = state["releases"].get(tag)
                if release is None or release["draft"]:
                    fail("gh: Not Found (HTTP 404)")
                emit({key: val for key, val in release.items() if not key.startswith("_")})
            elif urlparse(endpoint).path == "repos/" + REPOSITORY + "/releases":
                if state.get("release_list_failure"):
                    fail(state["release_list_failure"])
                query = parse_qs(urlparse(endpoint).query)
                for index, argument in enumerate(args):
                    if argument in ("-f", "-F", "--field", "--raw-field") and index + 1 < len(args) and "=" in args[index + 1]:
                        key, val = args[index + 1].split("=", 1)
                        query[key] = [val]
                page = int(query.get("page", ["1"])[0])
                per_page = int(query.get("per_page", ["30"])[0])
                if page < 1 or not 1 <= per_page <= 100:
                    fail("gh: Unprocessable Entity (HTTP 422)")
                releases = list(state["releases"].values())
                visible = [{key: val for key, val in release.items() if not key.startswith("_")}
                           for release in releases]
                if "--paginate" in args:
                    pages = [visible[start:start + per_page]
                             for start in range((page - 1) * per_page, len(visible), per_page)]
                    if "--slurp" in args:
                        emit(pages)
                    else:
                        print("".join(json.dumps(batch) for batch in pages))
                else:
                    start = (page - 1) * per_page
                    emit(visible[start:start + per_page])
            elif "/git/ref" in endpoint:
                tag = unquote(endpoint.split("/tags/", 1)[-1])
                own_tag = os.environ["GITHUB_REF"].removeprefix("refs/tags/")
                if tag == own_tag:
                    state["tag_reads"] = state.get("tag_reads", 0) + 1
                    actual_sha = state["tag_sha"]
                    if state.get("move_own_tag_after_reads") is not None and state["tag_reads"] > state["move_own_tag_after_reads"]:
                        actual_sha = "f" * 40
                else:
                    actual_sha = state["releases"].get(tag, {}).get("target_commitish", IMAGE_SHA)
                obj = state.get("tag_objects", {}).get(tag, {"type": "commit", "sha": actual_sha})
                emit({"object": obj})
            elif "/git/tags/" in endpoint:
                emit({"object": state["annotated_tag_objects"][endpoint.rsplit("/", 1)[-1]]})
            elif "/compare/" in endpoint:
                emit({"status": "ahead" if state["on_main"] else "diverged", "merge_base_commit": {"sha": state["tag_sha"]}})
            else:
                fail("Unsupported fixture gh API: " + endpoint)
        elif args[:2] == ["release", "download"]:
            tag = args[2]
            release = state["releases"].get(tag)
            if release is None:
                fail("gh: Not Found (HTTP 404)")
            destination = Path(value("--dir", value("-D", ".")))
            destination.mkdir(parents=True, exist_ok=True)
            for path in Path(release["_directory"]).iterdir():
                if path.is_file() and path.name != "manifest.json":
                    shutil.copyfile(path, destination / path.name)
        elif args[:2] == ["release", "create"]:
            tag = args[2]
            if tag in state["releases"]:
                fail("gh: Validation Failed: release already exists (HTTP 422)")
            directory = state_path.parent / "draft-assets"
            directory.mkdir(exist_ok=True)
            state["releases"][tag] = {"id": 701, "tag_name": tag, "target_commitish": state["tag_sha"], "draft": True,
                                      "immutable": False, "assets": [], "_directory": str(directory)}
        elif args[:2] == ["release", "upload"]:
            tag = args[2]
            release = state["releases"][tag]
            for argument in args[3:]:
                path = Path(argument.split("#", 1)[0])
                if path.is_file():
                    shutil.copyfile(path, Path(release["_directory"]) / path.name)
                    state["uploads"].append(path.name)
                    release["assets"].append({"name": path.name, "digest": "sha256:" + checksum(path.read_bytes())})
        elif args[:2] == ["release", "edit"]:
            if "--latest" in args:
                fail("accepts 1 arg(s), received 2")
            latest = [argument for argument in args[3:] if argument.startswith("--latest=")]
            if len(latest) != 1 or latest[0] not in ("--latest=true", "--latest=false"):
                fail("invalid --latest boolean flag")
            release = state["releases"][args[2]]
            release["draft"], release["immutable"] = False, True
            if state.get("move_tag_after_publish"):
                state["tag_sha"] = "f" * 40
        elif args[:2] in (["release", "verify"], ["release", "verify-asset"]):
            print("Fixture release attestation verified")
        else:
            fail("Unsupported fixture gh command: " + " ".join(args))
    elif tool == "crane":
        reference = next((arg for arg in args if arg.startswith("ghcr.io/")), "")
        if anonymous and any(item in reference for item in state["private"]):
            fail("HTTP 401: Unauthorized")
        if "manifest" in args:
            if reference.startswith(IMAGE):
                emit(state["image_manifest"])
            else:
                if state.get("chart_archive"):
                    archive = Path(state["chart_archive"])
                else:
                    candidate = state_path.parent / "candidate" / "manifest.json"
                    manifest = json.loads(candidate.read_text())
                    archive = candidate.parent / manifest["chart"]["archive"]
                emit({"schemaVersion": 2, "config": {"mediaType": "application/vnd.cncf.helm.config.v1+json"},
                      "layers": [{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
                                  "digest": "sha256:" + checksum(archive.read_bytes())}]})
        elif "digest" in args:
            digest = reference.split("@", 1)[1] if "@" in reference else state["versions"].get(reference)
            if digest is None:
                fail("MANIFEST_UNKNOWN: manifest unknown", 1)
            print(digest)
        elif "config" in args:
            emit({"config": {"User": "65532:65532", "Labels": {"org.opencontainers.image.source": "https://github.com/" + REPOSITORY,
                  "org.opencontainers.image.revision": IMAGE_SHA, "org.opencontainers.image.version": "0.1.0"}}})
        else:
            fail("Unsupported fixture crane command")
    elif tool == "helm":
        if args[:2] == ["show", "chart"]:
            print(f'name: external-postgres-control-operator\nversion: {state["chart_metadata"]["version"]}\nappVersion: "{state["chart_metadata"]["appVersion"]}"')
        elif args[:2] == ["show", "values"]:
            print(f"image:\n  repository: {IMAGE}\n  tag: 0.1.0\n  digest: {INDEX}")
        elif args and args[0] in ("pull", "push"):
            print("Fixture chart transfer")
        else:
            fail("Unsupported fixture helm command")
    elif tool == "cosign":
        command = args[0] if args else ""
        reference = next((arg for arg in args if arg.startswith("ghcr.io/")), "")
        if command.startswith("verify"):
            if reference:
                if anonymous and any(item in reference for item in state["private"]):
                    fail("HTTP 401: Unauthorized")
                claims = state["subjects"].get(reference)
                if claims is None:
                    fail("No signatures found")
            else:
                path = Path(value("--bundle", "missing"))
                if not path.is_file():
                    fail("Signature bundle missing")
                claims = json.loads(path.read_text()).get("fixture_claims", {})
            flags = {
                "--certificate-identity": "identity", "--certificate-oidc-issuer": "issuer",
                "--certificate-github-workflow-repository": "repository", "--certificate-github-workflow-ref": "ref",
                "--certificate-github-workflow-trigger": "trigger", "--certificate-github-workflow-sha": "source_sha",
            }
            for flag, field in flags.items():
                if value(flag) != claims.get(field):
                    fail("Certificate policy mismatch: " + field)
            if any("insecure-ignore" in arg or arg.startswith("--check-claims=false") or "identity-regexp" in arg for arg in args):
                fail("Verification bypass forbidden")
            emit([{"critical": {"image": {"docker-manifest-digest": reference.split("@")[-1]}}, "optional": {}}])
        elif command in ("sign", "sign-blob"):
            path = value("--bundle")
            if path:
                tag = os.environ["GITHUB_REF"].removeprefix("refs/tags/")
                stream = "chart" if tag.startswith("chart-") else "image"
                claims = identity(stream, tag, os.environ["GITHUB_SHA"])
                signed = bundle(reference or "SHA256SUMS", claims)
                if state.get("fresh_signature_nonces"):
                    signed["fixture_nonce"] = uuid.uuid4().hex
                Path(path).write_text(json.dumps(signed))
                if reference:
                    state["subjects"][reference] = claims
        else:
            fail("Unsupported fixture cosign command")
    else:
        fail("Unexpected image build/write command: " + tool)
    state_path.write_text(json.dumps(state))


if __name__ == "__main__":
    command_main()
