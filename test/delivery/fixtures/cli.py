#!/usr/bin/env python3
"""Local CLI boundary fake; it proves orchestration, never API lifecycle."""

import json
import os
from pathlib import Path
import re
import sys


directory = Path(os.environ["DELIVERY_STATE"])
tool = Path(sys.argv[0]).name
args = sys.argv[1:]
record = {"tool": tool, "args": args}
server_dry_run = ("--dry-run=server" in args or
                  ("--dry-run" in args and args[args.index("--dry-run") + 1] == "server"))
record["serverDryRun"] = server_dry_run
record["persistentMutation"] = bool({"apply", "create", "delete", "patch", "replace"}.intersection(args)) and not server_dry_run
if tool == "container-cli":
    if "build" in args:
        dockerfile = Path(args[args.index("-f") + 1]) if "-f" in args else Path("Dockerfile")
        record["dockerfile"] = dockerfile.read_text()
    with (directory / "calls.jsonl").open("a") as stream:
        stream.write(json.dumps(record) + "\n")
    sys.exit(0)

with (directory / "calls.jsonl").open("a") as stream:
    stream.write(json.dumps(record) + "\n")
if "--context" not in args or args[args.index("--context") + 1] != "kind-epco-delivery-owned":
    print("missing owned explicit context", file=sys.stderr)
    sys.exit(90)
for index, argument in enumerate(args):
    if argument.startswith("--context="):
        sys.exit(90)

operation = next((value for value in args if value in {"get", "delete", "apply", "patch", "replace"}), "")
if server_dry_run:
    if "-f" not in args:
        print("API dry-run requires an exact rendered manifest", file=sys.stderr)
        sys.exit(91)
    path = args[args.index("-f") + 1]
    rendered = sys.stdin.read() if path == "-" else Path(path).read_text()
    expected = os.environ.get("DELIVERY_EXPECTED_API_POLICY")
    if expected and ("NetworkPolicy" not in rendered or
                     any(value not in rendered for value in json.loads(expected))):
        print("API dry-run did not receive exact selected rendered policy", file=sys.stderr)
        sys.exit(91)
    failure = os.environ.get("DELIVERY_SERVER_DRY_RUN_FAILURE")
    if failure:
        print("Error from server (Invalid): NetworkPolicy invalid: " + failure, file=sys.stderr)
        sys.exit(1)
    sys.exit(0)
if os.environ.get("DELIVERY_INSTALL_VALUES_ONLY") == "true" and (operation == "apply" or "wait" in args):
    sys.exit(0)
if operation == "get":
    changed = directory / "values-changed"
    if os.environ.get("DELIVERY_VALUES_CHANGE_PATH") and not changed.exists():
        Path(os.environ["DELIVERY_VALUES_CHANGE_PATH"]).write_text(json.dumps({"replicaCount": 0}))
        changed.touch()
    schemas = directory / "schema-objects.json"
    if schemas.exists() and any(value in {"crd", "crds", "customresourcedefinitions"} for value in args):
        scenario = os.environ.get("DELIVERY_SCHEMA_SCENARIO", "known")
        if scenario == "read-failure":
            print("fixture CRD read forbidden", file=sys.stderr)
            sys.exit(23)
        objects = json.loads(schemas.read_text())
        requested = [obj for obj in objects if obj["metadata"]["name"] in args]
        print(json.dumps(requested[0] if requested else {"apiVersion": "v1", "kind": "List", "items": objects}))
        sys.exit(0)
    if any(value in {"clusterrole", "clusterroles"} for value in args):
        if os.environ.get("DELIVERY_SCHEMA_SCENARIO") == "legacy-ownership":
            print(json.dumps({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
                              "metadata": {"name": "epco-manager-role", "labels": {"app.kubernetes.io/managed-by": "kustomize"}}}))
            sys.exit(0)
        if "--ignore-not-found" in args or "--ignore-not-found=true" in args:
            sys.exit(0)
        print("NotFound", file=sys.stderr)
        sys.exit(1)
    state = json.loads((directory / "cr-lists.json").read_text())
    requested = [value for value in args if value.split(".")[0] in state]
    if not requested:
        print("unsupported read", file=sys.stderr)
        sys.exit(91)
    items = []
    for resource in requested:
        value = state[resource.split(".")[0]]
        if value == "failure":
            print("fixture list denied", file=sys.stderr)
            sys.exit(23)
        if value == "malformed":
            print("{broken")
            sys.exit(0)
        items.extend(value)
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
elif operation == "delete":
    inventory = json.loads((directory / "inventory.json").read_text())
    if "-f" in args:
        path = args[args.index("-f") + 1]
        manifest = sys.stdin.read() if path == "-" else Path(path).read_text()
        kinds = re.findall(r"^kind:\s*(\S+)", manifest, re.MULTILINE)
        (directory / "deleted-kinds.json").write_text(json.dumps(kinds))
        inventory = [item for item in inventory if item["kind"] not in kinds]
    elif any(value in {"crd", "crds", "customresourcedefinitions"} for value in args):
        inventory = [item for item in inventory if item["kind"] != "CustomResourceDefinition"]
    else:
        print("unsupported delete", file=sys.stderr)
        sys.exit(92)
    (directory / "inventory.json").write_text(json.dumps(inventory))
else:
    print("unexpected Kubernetes mutation", file=sys.stderr)
    sys.exit(93)
