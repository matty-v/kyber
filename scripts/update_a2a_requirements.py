#!/usr/bin/env python3
"""Regenerate the A2A applicability ledger from the exact pinned TCK tree."""

from __future__ import annotations

import argparse
import ast
import hashlib
import json
import subprocess
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / "conformance" / "a2a" / "1.0"


def literal(value: ast.expr) -> Any:
    if isinstance(value, ast.Constant):
        return value.value
    if isinstance(value, ast.Attribute):
        return value.attr
    if isinstance(value, ast.Name):
        return value.id
    if isinstance(value, ast.List):
        return [literal(item) for item in value.elts]
    return None


def extract(tck: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for source_path in sorted((tck / "tck" / "requirements").glob("*.py")):
        tree = ast.parse(source_path.read_text(), filename=str(source_path))
        for node in ast.walk(tree):
            if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "RequirementSpec"):
                continue
            values = {keyword.arg: literal(keyword.value) for keyword in node.keywords}
            requirement_id = values["id"]
            level = values["level"]
            section = values["section"]
            description = values["description"]
            source = source_path.stem
            deferred = source == "push_notifications" or requirement_id.startswith(("CARD-EXT-", "CARD-SIGN-"))
            not_applicable = source in {"binding_grpc", "binding_jsonrpc"} or requirement_id.startswith("BIND-EQUIV-")
            if deferred:
                applicability = "deferred-optional"
                reason = "Kyber does not declare this optional capability in its Agent Card."
            elif not_applicable:
                applicability = "not-applicable"
                reason = "Kyber declares only the HTTP+JSON protocol binding."
            else:
                applicability = "applicable"
                reason = "Kyber declares and implements this requirement in its HTTP+JSON Bearer profile."
            test_symbol = {
                "agent_card": "TestA2AFeatureGateAuthVersionAndCard",
                "streaming": "TestA2AStreamingStartsWithCurrentTaskSnapshot",
                "auth": "TestA2ASendReplayGetListAndOwnerIsolation",
                "data_model": "TestNativeA2AArtifactsPreserveTextDataAndAuthorizedFileReferences",
                "binding_http_json": "TestA2ACancelAndMediaNegotiation",
                "versioning": "TestA2AFeatureGateAuthVersionAndCard",
            }.get(source, "TestA2ASendReplayGetListAndOwnerIsolation")
            kyber_file = "pkg/api/a2a_translation_test.go" if test_symbol.startswith("TestNative") else "pkg/api/a2a_edge_test.go"
            tests: list[str] = []
            if applicability == "applicable":
                tests = [
                    f"tck:tests/compatibility/core_operations/test_requirements.py::test_{level.lower()}_requirement",
                    f"kyber:{kyber_file}::{test_symbol}",
                ]
            elif applicability == "deferred-optional":
                tests = ["kyber:pkg/api/a2a_edge_test.go::TestA2AUnsupportedOptionalOperationsStayDark"]
            rows.append({
                "id": requirement_id,
                "source": {
                    "artifact": "a2a-spec-1.0.0",
                    "section": section,
                    "quoteDigest": "sha256:" + hashlib.sha256(description.encode()).hexdigest(),
                },
                "level": level,
                "topic": source.replace("_", "-"),
                "applicability": applicability,
                "reason": reason,
                "declaredFeature": "core" if applicability == "applicable" else source.replace("_", "-"),
                "tests": tests,
                "owner": "platform-a2a",
                "reviewedBy": "protocol-security",
                "lastReviewed": "2026-09-02",
                "tckSource": str(source_path.relative_to(tck)),
                "title": values.get("title", ""),
            })
    return sorted(rows, key=lambda row: row["id"])


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("tck", type=Path)
    args = parser.parse_args()
    pins = json.loads((BASE / "pins.json").read_text())
    tck = args.tck.resolve()
    commit = subprocess.check_output(["git", "-C", str(tck), "rev-parse", "HEAD"], text=True).strip()
    if commit != pins["tck"]["commit"]:
        raise SystemExit(f"TCK commit mismatch: got {commit}, want {pins['tck']['commit']}")
    lock_digest = hashlib.sha256((tck / "uv.lock").read_bytes()).hexdigest()
    if lock_digest != pins["tck"]["uvLockSHA256"]:
        raise SystemExit("TCK uv.lock digest mismatch")
    rows = extract(tck)
    if len(rows) != pins["tck"]["inventoryCount"]:
        raise SystemExit(f"TCK inventory count mismatch: got {len(rows)}")
    document = {
        "schemaVersion": 1,
        "specificationCommit": pins["specification"]["commit"],
        "tckCommit": pins["tck"]["commit"],
        "inventorySource": "Pinned official TCK RequirementSpec registry; quoteDigest hashes the pinned requirement description.",
        "requirements": rows,
    }
    (BASE / "requirements.yaml").write_text(json.dumps(document, indent=2, sort_keys=True) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
