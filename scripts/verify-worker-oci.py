"""배포 후보 OCI의 소스 커밋, 아키텍처와 digest를 검증한다."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import tarfile


def verified_blob(archive, descriptor, *, read_json=False):
    digest = descriptor["digest"]
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
        raise ValueError("OCI 블롭 digest 형식이 잘못됐습니다")
    member = archive.getmember("blobs/" + digest.replace(":", "/", 1))
    if not member.isfile() or member.size != descriptor["size"]:
        raise ValueError(f"OCI 블롭 크기 불일치 {digest}")

    with archive.extractfile(member) as blob:
        if "sha256:" + hashlib.file_digest(blob, "sha256").hexdigest() != digest:
            raise ValueError(f"OCI 블롭 digest 불일치 {digest}")
        if read_json:
            blob.seek(0)
            return json.load(blob)


def image_digest(path, revision):
    with tarfile.open(path) as archive:
        with archive.extractfile("index.json") as index:
            manifest_descriptor, = json.load(index)["manifests"]
        manifest = verified_blob(archive, manifest_descriptor, read_json=True)
        config = verified_blob(archive, manifest["config"], read_json=True)
        if (config["os"], config["architecture"]) != ("linux", "arm64"):
            raise ValueError("Linux ARM64 OCI가 필요합니다")
        if config["config"]["Labels"].get("org.opencontainers.image.revision") != revision:
            raise ValueError("OCI 소스 커밋이 검증 후보와 일치하지 않습니다")
        for layer in manifest["layers"]:
            verified_blob(archive, layer)
        return manifest_descriptor["digest"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("revision")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-f0-9]{40}", args.revision):
        parser.error("Worker Platform 전체 SHA가 필요합니다")
    for name in ("run-api", "worker-controller", "actor-runtime"):
        try:
            digest = image_digest(args.directory / f"{name}.oci.tar", args.revision)
        except (OSError, tarfile.TarError, KeyError, TypeError, ValueError) as error:
            parser.exit(1, f"{name} OCI 검증 실패 ({error})\n")
        print(f"{name}={digest}")


if __name__ == "__main__":
    main()
