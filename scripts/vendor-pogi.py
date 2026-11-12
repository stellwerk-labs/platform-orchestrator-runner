#!/usr/bin/env python3
"""Reproduce the reviewed Pogi source selection from an existing pinned checkout, offline."""
import hashlib
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
DESTINATION = ROOT / "internal/thirdparty/pogi"
MANIFEST_SHA256 = "1a7de1e55c216535392b227276c71fc94ca39268959c7619e9848bb05d669176"


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: vendor-pogi.py PINNED_UPSTREAM_SOURCE")
    upstream = Path(sys.argv[1]).resolve(strict=True)
    encoded = (DESTINATION / "SOURCE.json").read_bytes()
    if hashlib.sha256(encoded).hexdigest() != MANIFEST_SHA256:
        raise SystemExit("source manifest differs from reviewed pin")
    manifest = json.loads(encoded)
    selected = []
    for item in manifest["files"]:
        source = upstream / item["upstream"]
        if source.is_symlink() or not source.is_file():
            raise SystemExit("upstream source must be a regular file")
        original = source.read_bytes()
        if hashlib.sha256(original).hexdigest() != item["originalSHA256"]:
            raise SystemExit("upstream source differs from reviewed hash")
        adapted = original.replace(
            b"github.com/nrndev/pogi/internal/terraform",
            b"github.com/stellwerk-labs/platform-orchestrator-runner/internal/thirdparty/pogi/terraform",
        )
        if hashlib.sha256(adapted).hexdigest() != item["adaptedSHA256"]:
            raise SystemExit("adapted source differs from reviewed hash")
        selected.append((DESTINATION / item["path"], adapted))
    for destination, content in selected:
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(content)
    print("Reproduced 11 reviewed Pogi source/model/catalogue/licence files")


if __name__ == "__main__":
    main()
