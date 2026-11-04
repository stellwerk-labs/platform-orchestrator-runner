#!/usr/bin/env python3
"""Exercise real production collectors and actual notice bytes, without mock tools."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class Notices(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="runner-notices-")
        cls.work = Path(cls.workspace.name)
        cls.env = {**os.environ, "GOTOOLCHAIN": "go1.26.10", "GOWORK": "off", "GOENV": "off", "CGO_ENABLED": "0", "PYTHONDONTWRITEBYTECODE": "1"}
        for key in ("GOOS", "GOARCH", "GOAMD64", "GOARM64", "GOFLAGS", "GOEXPERIMENT"):
            cls.env.pop(key, None)
        cls.checker = cls.work / "noticecheck"
        subprocess.run(["go", "build", "-o", str(cls.checker), "./scripts/noticecheck"], cwd=ROOT, env=cls.env, check=True, timeout=120)
        for arch in ("amd64", "arm64"):
            env = {**cls.env, "GOARCH": arch, "OPENTOFU_NOTICES_DIR": str(ROOT / "scripts/release-notices/testdata/opentofu")}
            subprocess.run(["sh", "scripts/collect-licenses.sh", str(cls.work / arch), "./cmd/runner"], cwd=ROOT, env=env, check=True, timeout=180)

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def check(self, bundle, arch="arm64"):
        return subprocess.run([str(self.checker), str(bundle), str(ROOT), arch], cwd=ROOT, env=self.env, capture_output=True)

    def final(self, bundle):
        return subprocess.run(["sh", "scripts/validate-notices.sh", str(bundle)], cwd=ROOT, capture_output=True)

    def rehash(self, bundle):
        rows = []
        for path in sorted(bundle.rglob("*")):
            if path.is_file() and path != bundle / "manifest.sha256":
                rows.append(hashlib.sha256(path.read_bytes()).hexdigest() + "  ./" + path.relative_to(bundle).as_posix())
        (bundle / "manifest.sha256").write_text("\n".join(rows) + "\n")

    def copy(self):
        owned = tempfile.TemporaryDirectory(dir=self.work)
        self.addCleanup(owned.cleanup)
        destination = Path(owned.name) / "bundle"
        shutil.copytree(self.work / "arm64", destination)
        return destination

    def test_real_both_target_closures(self):
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                self.assertEqual(self.check(self.work / arch, arch).returncode, 0)
                self.assertEqual(self.final(self.work / arch).returncode, 0)

    def test_actual_final_copy_rejects_corruption_missing_extra_and_links(self):
        for kind in ("corrupt", "missing", "extra", "link"):
            with self.subTest(kind=kind):
                bundle = self.copy()
                if kind == "corrupt":
                    with (bundle / "application/LICENSE").open("ab") as stream:
                        stream.write(b"changed")
                elif kind == "missing":
                    (bundle / "opentofu/LICENSE").unlink()
                elif kind == "extra":
                    (bundle / "unlisted").write_text("extra")
                else:
                    (bundle / "linked").symlink_to(bundle / "application/LICENSE")
                self.assertNotEqual(self.final(bundle).returncode, 0)

    def test_actual_provenance_rejects_changed_identity_and_duplicate_fields(self):
        for kind in ("commit", "sum", "blob", "duplicate", "unknown"):
            with self.subTest(kind=kind):
                bundle = self.copy()
                path = bundle / "github.com/stellwerk-labs/golib/hmessaging/LICENSE.provenance"
                data = json.loads(path.read_text())
                if kind == "commit":
                    data["sourceCommit"] = "0" * 40
                elif kind == "sum":
                    data["moduleSum"] = "h1:" + "A" * 43 + "="
                elif kind == "blob":
                    data["licenseBlob"] = "0" * 40
                elif kind == "unknown":
                    data["unknown"] = "field"
                encoded = json.dumps(data)
                if kind == "duplicate":
                    encoded = encoded.replace("{", '{"module":"duplicate",', 1)
                path.write_text(encoded)
                self.rehash(bundle)
                self.assertNotEqual(self.check(bundle).returncode, 0)

    def test_real_closure_rejects_missing_dependency_wrong_graph_and_target(self):
        for kind in ("dependency", "graph", "target", "application", "vendor-source"):
            with self.subTest(kind=kind):
                bundle = self.copy()
                if kind == "dependency":
                    shutil.rmtree(bundle / "github.com/stellwerk-labs/golib/hnats")
                elif kind == "graph":
                    path = bundle / "modules.txt"
                    path.write_text(path.read_text().split("\n", 1)[1])
                elif kind == "target":
                    (bundle / "target.txt").write_text("linux/amd64\n")
                elif kind == "application":
                    (bundle / "application/LICENSE").write_text("changed")
                else:
                    (bundle / "opentofu/SOURCE.json").write_text("{}")
                self.rehash(bundle)
                self.assertNotEqual(self.check(bundle).returncode, 0)

if __name__ == "__main__":
    unittest.main()
