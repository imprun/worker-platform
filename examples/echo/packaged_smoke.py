"""Prepare the packaged Echo fixture for direct deployment with workerctl."""

import argparse
import json
import os
from pathlib import Path
import re


def replace_field(document, field, value):
    line = f"{field}: {json.dumps(value)}"
    document, count = re.subn(rf"^{re.escape(field)}:.*$", lambda _: line, document, flags=re.MULTILINE)
    if count != 1:
        raise ValueError(f"expected one {field.strip()} field in the Echo fixture")
    return document


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path)
    parser.add_argument("runtime_image")
    parser.add_argument("app_package")
    args = parser.parse_args()

    registry, _, _ = args.app_package.partition("/")
    hostname, _, port = registry.partition(":")
    profile, worker = Path(__file__).with_name("packaged.yaml").read_text(encoding="utf-8").split("\n---\n")
    # workerctl accepts the file-based API version of the same fixture.
    version = "workerplatform.imprun.dev/v1alpha2"
    profile = replace_field(profile, "apiVersion", version)
    profile = replace_field(profile, "  runtimeImage", args.runtime_image)
    profile = replace_field(profile, "      hostname", hostname)
    profile = replace_field(profile, "      port", int(port) if port else 443)
    if ca_path := os.environ.get("PACKAGE_REGISTRY_CA"):
        ca = json.dumps(Path(ca_path).read_text(encoding="utf-8"))
        profile = profile.replace("\nspec:\n", f"\nspec:\n  appRegistryCA: {ca}\n", 1)
    worker = replace_field(worker, "apiVersion", version)
    worker = replace_field(worker, "  appPackage", args.app_package)

    (args.destination / "profile.yaml").write_text(profile + "\n", encoding="utf-8")
    (args.destination / "worker.yaml").write_text(worker, encoding="utf-8")


if __name__ == "__main__":
    main()
