"""Build an OCI layout for a Worker Platform app executable."""

import argparse
import gzip
import hashlib
import io
import json
import tarfile
from pathlib import Path


def json_bytes(value):
    return json.dumps(value, separators=(",", ":")).encode()


def build(executable, destination):
    if not 0 < executable.stat().st_size < 32 << 20:
        raise ValueError("app executable must be nonempty and smaller than 32 MiB")
    content = io.BytesIO()
    with tarfile.open(fileobj=content, mode="w") as archive:
        for name, payload, mode in (
            ("app.json", json_bytes({"contractVersion": "v1", "entrypoint": "bin/app"}), 0o644),
            ("bin/app", executable.read_bytes(), 0o755),
        ):
            entry = tarfile.TarInfo(name)
            entry.mode = mode
            entry.size = len(payload)
            archive.addfile(entry, io.BytesIO(payload))

    compressed = gzip.compress(content.getvalue(), mtime=0)
    if len(compressed) > 32 << 20:
        raise ValueError("compressed app layer exceeds 32 MiB")
    blobs = destination / "blobs" / "sha256"
    blobs.mkdir(parents=True, exist_ok=True)

    def put(media_type, payload):
        digest = hashlib.sha256(payload).hexdigest()
        (blobs / digest).write_bytes(payload)
        return {"mediaType": media_type, "size": len(payload), "digest": f"sha256:{digest}"}

    media_type = "application/vnd.oci.image.manifest.v1+json"
    config = put("application/vnd.oci.empty.v1+json", b"{}")
    layer = put("application/vnd.imprun.worker-app.layer.v1.tar+gzip", compressed)
    manifest = put(media_type, json_bytes({
        "schemaVersion": 2,
        "mediaType": media_type,
        "artifactType": "application/vnd.imprun.worker-app.v1",
        "config": config,
        "layers": [layer],
    }))
    (destination / "oci-layout").write_bytes(json_bytes({"imageLayoutVersion": "1.0.0"}))
    (destination / "index.json").write_bytes(json_bytes({"schemaVersion": 2, "manifests": [manifest]}))
    return manifest["digest"]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("executable", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    print(build(args.executable, args.output))
