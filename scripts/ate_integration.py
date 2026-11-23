"""Select and run the existing Echo smoke test for ATE integration changes."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
GO_BOUNDARIES = (
    "cmd/actor-runtime/",
    "cmd/worker-controller/",
    "cmd/workerctl/",
    "internal/apppackage/",
    "internal/ateclient/",
    "internal/controller/",
    "internal/platform/",
)
BUILD_BOUNDARIES = {
    "Dockerfile.actor-runtime",
    "Dockerfile.controller",
    "scripts/build-app-package.py",
}
ATE_DEPENDENCY = re.compile(
    r"^[+-]\s*(?:go\s|toolchain\s|(?:go\.ate\.dev/\S+|k8s\.io/\S+|"
    r"sigs\.k8s\.io/gateway-api|google\.golang\.org/(?:grpc|protobuf)|"
    r"golang\.org/x/(?:net|crypto|sys))\s)"
)


def output(*args):
    return subprocess.check_output(args, cwd=ROOT, encoding="utf-8", errors="surrogateescape", timeout=60)


def resolve(ref):
    return output("git", "rev-parse", "--verify", "--end-of-options", ref + "^{commit}").strip()


def changes(base, head):
    paths = output("git", "diff", "--no-renames", "--name-only", "-z", base, head, "--").split("\0")
    selected = set()
    for path in paths:
        if path in BUILD_BOUNDARIES:
            selected.add(path)
        elif path.endswith(".go") and not path.endswith("_test.go") and path.startswith(GO_BOUNDARIES):
            selected.add(path)
        elif path.endswith((".yaml", ".yml")) and path.startswith(("deploy/", "examples/echo/")):
            selected.add(path)
        elif path in {"go.mod", "go.sum"}:
            diff = output("git", "diff", "--no-ext-diff", "--no-color", "-U0", base, head, "--", path)
            if any(ATE_DEPENDENCY.match(line) for line in diff.splitlines()):
                selected.add(path)
    return sorted(selected)


def require_fixture():
    # Check the fixture identities before allowing the smoke test to deploy.
    context = os.environ["KUBE_CONTEXT"]
    kube = ("kubectl", "--context", context, "--request-timeout=20s")
    ate = ("kubectl", "ate", "--context", context, "--token-file", os.environ["ATE_TOKEN_FILE"])
    namespace = "wp-echo-example"
    for command, expected, label in (
        ((*kube, "get", "namespace", "kube-system"), "ATE_TEST_CLUSTER_UID", "cluster"),
        ((*kube, "get", "namespace", namespace), "ATE_TEST_NAMESPACE_UID", "test namespace"),
        ((*kube, "-n", "ate-system", "get", "workerpool", "echo-arm64-dev"), "ATE_TEST_POOL_UID", "test pool"),
        ((*ate, "get", "atespace", namespace), "ATE_TEST_SPACE_UID", "test Atespace"),
    ):
        actual = json.loads(output(*command, "-o", "json"))["metadata"]["uid"]
        if not actual or actual != os.environ[expected]:
            raise ValueError(f"{label} UID does not match {expected}")
    for resource, key in (("actors", "actors"), ("actor-templates", "actorTemplates")):
        listed = json.loads(output(*ate, "get", resource, "-a", namespace, "-o", "json"))
        if listed not in ({}, {key: []}):
            raise ValueError(f"test Atespace did not return an empty {resource} list")
    routes = json.loads(output(*kube, "-n", namespace, "get", "httproute", "-o", "json"))
    if routes["items"]:
        raise ValueError("test namespace already contains HTTPRoutes")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("base", help="commit before the change")
    parser.add_argument("head", nargs="?", default="HEAD", help="candidate commit")
    parser.add_argument("--run", action="store_true", help="run against the dedicated test fixture if the gate matches")
    args = parser.parse_args()
    base, head = resolve(args.base), resolve(args.head)
    selected = changes(base, head)
    if not args.run and (github_output := os.environ.get("GITHUB_OUTPUT")):
        with open(github_output, "a", encoding="utf-8") as stream:
            stream.write(f"required={'true' if selected else 'false'}\n")
    if not selected:
        print("ATE integration changes: none", flush=True)
        return
    print("ATE integration changes:", *selected, sep="\n", flush=True)
    if args.run:
        if head != resolve("HEAD") or output("git", "status", "--porcelain"):
            raise ValueError("check out the candidate commit with a clean worktree before the live smoke test")
        require_fixture()
        subprocess.run(("sh", "examples/echo/run.sh"), cwd=ROOT, check=True)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, ValueError, OSError, subprocess.SubprocessError) as exc:
        sys.exit(f"ATE integration: {exc}")
