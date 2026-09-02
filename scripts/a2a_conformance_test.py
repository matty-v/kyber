#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

import a2a_conformance as gate


class A2AConformanceTest(unittest.TestCase):
    def test_adapter_affecting_paths_fail_safe(self) -> None:
        self.assertTrue(gate.affected(["pkg/api/a2a_handler.go"]))
        self.assertTrue(gate.affected(["pkg/taskstore/postgres.go"]))
        self.assertTrue(gate.affected(["conformance/a2a/1.0/profile.json"]))
        self.assertFalse(gate.affected(["docs/product/overview.md"]))

    def test_checked_in_contract_validates(self) -> None:
        gate.validate()

    def test_evidence_requires_clean_results_and_exact_checksums(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            files = {
                "manifest.json": json.dumps({"claim": "self-tested-conformance", "cleanup": "succeeded", "applicableMustFailures": 0, "securityFailures": 0}),
                "requirements.json": "{}",
                "support-matrix.json": "{}",
                "tck/compatibility.json": "{}",
                "tck/compatibility.html": "<html></html>",
                "tck/junit.xml": "<testsuite/>",
                "kyber/junit.xml": "<testsuite/>",
            }
            for relative, content in files.items():
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content)
            sums = []
            for relative in sorted(files):
                sums.append(f"{hashlib.sha256((root / relative).read_bytes()).hexdigest()}  {relative}")
            (root / "SHA256SUMS").write_text("\n".join(sums) + "\n")
            gate.scan_evidence(root)
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps({"claim": "self-tested-conformance", "cleanup": "succeeded", "applicableMustFailures": 1, "securityFailures": 0}))
            updated_sums = []
            for relative in sorted(files):
                updated_sums.append(f"{hashlib.sha256((root / relative).read_bytes()).hexdigest()}  {relative}")
            (root / "SHA256SUMS").write_text("\n".join(updated_sums) + "\n")
            with self.assertRaisesRegex(gate.GateError, "MUST failure"):
                gate.scan_evidence(root)

    def test_bundle_is_immutable_and_validated(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            reports = root / "reports"
            reports.mkdir()
            (reports / "compatibility.json").write_text("{}")
            (reports / "compatibility.html").write_text("<html></html>")
            (reports / "junitreport.xml").write_text("<testsuite/>")
            junit = root / "kyber.xml"
            junit.write_text("<testsuite/>")
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps({"claim": "self-tested-conformance", "cleanup": "succeeded", "applicableMustFailures": 0, "securityFailures": 0}))
            output = root / "bundle"
            gate.bundle_evidence(output, manifest, reports, junit)
            self.assertTrue((output / "SHA256SUMS").is_file())
            with self.assertRaisesRegex(gate.GateError, "already exists"):
                gate.bundle_evidence(output, manifest, reports, junit)


if __name__ == "__main__":
    unittest.main()
