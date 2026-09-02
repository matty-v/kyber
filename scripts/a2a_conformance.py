#!/usr/bin/env python3
"""Validate and generate Kyber's pinned A2A conformance contract."""

from __future__ import annotations

import argparse
import datetime as dt
import fnmatch
import hashlib
import json
import os
import re
import shutil
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
BASE = ROOT / "conformance" / "a2a" / "1.0"
SHA256 = re.compile(r"^[0-9a-f]{64}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")
REQUIREMENT_ID = re.compile(r"^[A-Z][A-Z0-9_-]*-[0-9]{3}[a-z]?$")
ALLOWED_LEVELS = {"MUST", "SHOULD", "MAY"}
ALLOWED_APPLICABILITY = {"applicable", "not-applicable", "deferred-optional"}
SECRET_PATTERNS = (
    re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[a-z0-9._~+/-]{12,}"),
    re.compile(rb"(?i)(api[_-]?key|token|secret)\s*[:=]\s*[\"']?[a-z0-9_./+=-]{20,}"),
    re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
)


class GateError(ValueError):
    pass


def load(name: str) -> Any:
    try:
        return json.loads((BASE / name).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise GateError(f"{name}: {exc}") from exc


def canonical(value: Any) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise GateError(message)


def parse_date(value: str, field: str) -> dt.date:
    try:
        return dt.date.fromisoformat(value)
    except (TypeError, ValueError) as exc:
        raise GateError(f"{field}: expected YYYY-MM-DD") from exc


def validate_pins(pins: dict[str, Any]) -> None:
    require(pins.get("schemaVersion") == 1, "pins: unsupported schemaVersion")
    require(pins.get("protocolVersion") == "1.0", "pins: protocolVersion must be 1.0")
    for section in ("specification", "serverSDK", "tck", "independentClient"):
        require(isinstance(pins.get(section), dict), f"pins: missing {section}")
    for field in (
        ("specification", "commit"),
        ("serverSDK", "commit"),
        ("tck", "commit"),
        ("independentClient", "commit"),
    ):
        require(bool(COMMIT.fullmatch(pins[field[0]].get(field[1], ""))), f"pins: invalid {'.'.join(field)}")
    for section, key in (
        ("specification", "protoSHA256"),
        ("specification", "documentSHA256"),
        ("tck", "uvLockSHA256"),
        ("independentClient", "uvLockSHA256"),
    ):
        require(bool(SHA256.fullmatch(pins[section].get(key, ""))), f"pins: invalid {section}.{key}")
    go_sum = (ROOT / "go.sum").read_text()
    sdk = pins["serverSDK"]
    require(f'{sdk["module"]} {sdk["version"]} {sdk["moduleSum"]}' in go_sum, "pins: server SDK module checksum does not match go.sum")
    require(f'{sdk["module"]} {sdk["version"]}/go.mod {sdk["goModSum"]}' in go_sum, "pins: server SDK go.mod checksum does not match go.sum")
    parse_date(pins.get("retrievedAt", ""), "pins.retrievedAt")


def validate_policy(name: str, rows_key: str, today: dt.date) -> None:
    document = load(name)
    require(document.get("schemaVersion") == 1, f"{name}: unsupported schemaVersion")
    rows = document.get(rows_key)
    require(isinstance(rows, list), f"{name}: {rows_key} must be an array")
    seen: set[str] = set()
    for row in rows:
        row_id = row.get("id", "") if isinstance(row, dict) else ""
        require(row_id and row_id not in seen, f"{name}: missing or duplicate id {row_id!r}")
        seen.add(row_id)
        expiry = parse_date(row.get("expires", ""), f"{name}:{row_id}.expires")
        require(expiry >= today, f"{name}: {row_id} expired on {expiry}")
        require(bool(row.get("owner")), f"{name}: {row_id} has no owner")
        require(bool(row.get("approvedBy")), f"{name}: {row_id} has no approval")
        if rows_key == "patches":
            path = ROOT / row.get("path", "")
            require(path.is_file(), f"{name}: {row_id} patch is missing")
            require(hashlib.sha256(path.read_bytes()).hexdigest() == row.get("sha256"), f"{name}: {row_id} patch digest mismatch")


def validate_test_reference(reference: str, requirement_id: str) -> None:
    if reference.startswith("tck:"):
        value = reference.removeprefix("tck:")
        require(value.startswith("tests/") and "::test_" in value, f"requirements: {requirement_id} has non-exact TCK test {reference}")
        return
    require(reference.startswith("kyber:"), f"requirements: {requirement_id} has unknown test reference {reference}")
    value = reference.removeprefix("kyber:")
    path_text, separator, symbol = value.partition("::")
    require(separator == "::" and symbol.startswith("Test"), f"requirements: {requirement_id} has non-exact Kyber test {reference}")
    path = ROOT / path_text
    require(path.is_file(), f"requirements: {requirement_id} references missing {path_text}")
    require(re.search(rf"\bfunc\s+{re.escape(symbol)}\b", path.read_text()) is not None, f"requirements: {requirement_id} references missing {symbol}")


def validate_requirements(pins: dict[str, Any], today: dt.date) -> list[dict[str, Any]]:
    document = load("requirements.yaml")
    require(document.get("schemaVersion") == 1, "requirements: unsupported schemaVersion")
    require(document.get("specificationCommit") == pins["specification"]["commit"], "requirements: specification pin mismatch")
    require(document.get("tckCommit") == pins["tck"]["commit"], "requirements: TCK pin mismatch")
    rows = document.get("requirements")
    require(isinstance(rows, list), "requirements: requirements must be an array")
    require(len(rows) == pins["tck"]["inventoryCount"], f"requirements: expected {pins['tck']['inventoryCount']} rows, got {len(rows)}")
    seen: set[str] = set()
    for row in rows:
        requirement_id = row.get("id", "") if isinstance(row, dict) else ""
        require(bool(REQUIREMENT_ID.fullmatch(requirement_id)), f"requirements: invalid id {requirement_id!r}")
        require(requirement_id not in seen, f"requirements: duplicate id {requirement_id}")
        seen.add(requirement_id)
        require(row.get("level") in ALLOWED_LEVELS, f"requirements: {requirement_id} has invalid level")
        require(row.get("applicability") in ALLOWED_APPLICABILITY, f"requirements: {requirement_id} has invalid applicability")
        source = row.get("source", {})
        require(source.get("artifact") == "a2a-spec-1.0.0", f"requirements: {requirement_id} has wrong source artifact")
        require(bool(source.get("section")), f"requirements: {requirement_id} has no source section")
        require(bool(SHA256.fullmatch(source.get("quoteDigest", "").removeprefix("sha256:"))), f"requirements: {requirement_id} has invalid quote digest")
        reviewed = parse_date(row.get("lastReviewed", ""), f"requirements:{requirement_id}.lastReviewed")
        require((today - reviewed).days <= 366, f"requirements: {requirement_id} review is stale")
        require(bool(row.get("owner")), f"requirements: {requirement_id} has no owner")
        tests = row.get("tests", [])
        if row["applicability"] in {"applicable", "deferred-optional"}:
            require(tests, f"requirements: {row['applicability']} {requirement_id} has no tests")
        else:
            require(bool(row.get("reason")), f"requirements: {requirement_id} has no applicability reason")
            require(bool(row.get("reviewedBy")), f"requirements: {requirement_id} has no applicability reviewer")
        for reference in tests:
            validate_test_reference(reference, requirement_id)
        if row["level"] == "MUST":
            require(not row.get("deviation"), f"requirements: MUST {requirement_id} cannot be waived")
    return rows


def validate_profile(profile: dict[str, Any]) -> None:
    require(profile.get("schemaVersion") == 1, "profile: unsupported schemaVersion")
    require(profile.get("claim") == "self-tested-conformance", "profile: claim must remain self-tested-conformance")
    require(profile.get("bindings") == ["HTTP+JSON"], "profile: only HTTP+JSON may be declared")
    require(profile.get("securitySchemes") == ["Bearer"], "profile: only Bearer may be declared")
    forbidden = {"certified", "fully compliant", "officially approved"}
    encoded = json.dumps(profile).lower()
    require(not any(word in encoded for word in forbidden), "profile: forbidden claim language")


def validate_paths() -> None:
    document = load("adapter-paths.json")
    require(document.get("schemaVersion") == 1, "adapter-paths: unsupported schemaVersion")
    paths = document.get("paths")
    require(isinstance(paths, list) and paths == sorted(set(paths)), "adapter-paths: paths must be sorted and unique")
    require(all(path and not path.startswith("/") and ".." not in Path(path).parts for path in paths), "adapter-paths: paths must be repository-relative")
    require(document.get("owners") == ["platform-a2a", "security"], "adapter-paths: owner review contract changed")


def support_matrix(pins: dict[str, Any], profile: dict[str, Any], requirements: list[dict[str, Any]]) -> dict[str, Any]:
    deviations = load("deviations.json")["deviations"]
    patches = load("tck-patches.json")["patches"]
    counts: dict[str, dict[str, int]] = {}
    for row in requirements:
        level = row["level"]
        counts.setdefault(level, {})[row["applicability"]] = counts.setdefault(level, {}).get(row["applicability"], 0) + 1
    return {
        **{key: value for key, value in profile.items() if key != "schemaVersion"},
        "serverSDK": pins["serverSDK"],
        "tck": {"commit": pins["tck"]["commit"], "patches": [row["id"] for row in patches]},
        "independentClient": {key: pins["independentClient"][key] for key in ("version", "commit")},
        "requirementCounts": counts,
        "deviations": [row["id"] for row in deviations],
        "evidenceDigest": "unpublished",
    }


def markdown_matrix(matrix: dict[str, Any]) -> str:
    yes = lambda value: "yes" if value else "no"
    return "\n".join((
        "# A2A 1.0 support matrix",
        "",
        "> Status: self-tested conformance. This is not certification.",
        "",
        f"- Protocol: {matrix['protocolVersion']} ({matrix['specification']})",
        f"- Binding: {', '.join(matrix['bindings'])}",
        f"- Authentication: {', '.join(matrix['securitySchemes'])}",
        f"- Streaming: {yes(matrix['capabilities']['streaming'])}",
        f"- Push notifications: {yes(matrix['capabilities']['pushNotifications'])}",
        f"- Extended Agent Card: {yes(matrix['capabilities']['extendedAgentCard'])}",
        f"- Unsupported: {', '.join(matrix['unsupported'])}",
        f"- Evidence digest: {matrix['evidenceDigest']}",
        "",
        "The matrix is generated from the checked-in profile, applicability ledger,",
        "and immutable pins. Run `python3 scripts/a2a_conformance.py generate`.",
        "",
    ))


def generate() -> None:
    pins = load("pins.json")
    rows = validate_requirements(pins, dt.date.today())
    matrix = support_matrix(pins, load("profile.json"), rows)
    (BASE / "support-matrix.json").write_bytes(canonical(matrix))
    (BASE / "SUPPORT.md").write_text(markdown_matrix(matrix))


def check_generated() -> None:
    pins = load("pins.json")
    rows = validate_requirements(pins, dt.date.today())
    expected = support_matrix(pins, load("profile.json"), rows)
    require((BASE / "support-matrix.json").read_bytes() == canonical(expected), "support-matrix.json is stale; run generate")
    require((BASE / "SUPPORT.md").read_text() == markdown_matrix(expected), "SUPPORT.md is stale; run generate")


def affected(paths: list[str]) -> bool:
    prefixes = load("adapter-paths.json")["paths"]
    for changed in paths:
        changed = changed.strip().removeprefix("./")
        if not changed:
            continue
        if any(changed == prefix.rstrip("/") or changed.startswith(prefix) for prefix in prefixes):
            return True
    return False


def scan_evidence(directory: Path) -> None:
    require(directory.is_dir(), f"evidence: missing directory {directory}")
    required = {
        "manifest.json", "requirements.json", "support-matrix.json",
        "tck/compatibility.json", "tck/compatibility.html", "tck/junit.xml",
        "kyber/junit.xml", "SHA256SUMS",
    }
    present = {str(path.relative_to(directory)) for path in directory.rglob("*") if path.is_file()}
    require(required <= present, f"evidence: missing {sorted(required - present)}")
    for relative in sorted(present):
        data = (directory / relative).read_bytes()
        require(not any(pattern.search(data) for pattern in SECRET_PATTERNS), f"evidence: possible secret in {relative}")
    sums: dict[str, str] = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        digest, separator, relative = line.partition("  ")
        require(separator == "  " and SHA256.fullmatch(digest) is not None, "evidence: malformed SHA256SUMS")
        sums[relative] = digest
    expected_files = present - {"SHA256SUMS"}
    require(set(sums) == expected_files, "evidence: SHA256SUMS file set mismatch")
    for relative, digest in sums.items():
        require(hashlib.sha256((directory / relative).read_bytes()).hexdigest() == digest, f"evidence: checksum mismatch for {relative}")
    manifest = json.loads((directory / "manifest.json").read_text())
    require(manifest.get("claim") == "self-tested-conformance", "evidence: invalid claim")
    require(manifest.get("cleanup") == "succeeded", "evidence: cleanup did not succeed")
    require(manifest.get("applicableMustFailures") == 0, "evidence: applicable MUST failure")
    require(manifest.get("securityFailures") == 0, "evidence: security failure")


def bundle_evidence(output: Path, manifest_path: Path, tck_reports: Path, kyber_junit: Path) -> None:
    require(not output.exists(), f"evidence: output already exists: {output}")
    required_sources = {
        manifest_path: "manifest.json",
        BASE / "requirements.yaml": "requirements.json",
        BASE / "support-matrix.json": "support-matrix.json",
        tck_reports / "compatibility.json": "tck/compatibility.json",
        tck_reports / "compatibility.html": "tck/compatibility.html",
        tck_reports / "junitreport.xml": "tck/junit.xml",
        kyber_junit: "kyber/junit.xml",
    }
    for source in required_sources:
        require(source.is_file(), f"evidence: missing source {source}")
    output.mkdir(parents=True)
    for source, relative in required_sources.items():
        destination = output / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)
    present = sorted(path for path in output.rglob("*") if path.is_file())
    for path in present:
        data = path.read_bytes()
        require(not any(pattern.search(data) for pattern in SECRET_PATTERNS), f"evidence: possible secret in {path.relative_to(output)}")
    sums = [f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.relative_to(output)}" for path in present]
    (output / "SHA256SUMS").write_text("\n".join(sums) + "\n")
    scan_evidence(output)


def validate() -> None:
    today = dt.date.fromisoformat(os.environ.get("A2A_GATE_DATE", dt.date.today().isoformat()))
    pins = load("pins.json")
    validate_pins(pins)
    validate_profile(load("profile.json"))
    validate_paths()
    validate_policy("deviations.json", "deviations", today)
    validate_policy("tck-patches.json", "patches", today)
    validate_requirements(pins, today)
    check_generated()


def main() -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate")
    sub.add_parser("generate")
    affected_parser = sub.add_parser("affected")
    affected_parser.add_argument("paths", nargs="*")
    evidence_parser = sub.add_parser("evidence")
    evidence_parser.add_argument("directory", type=Path)
    bundle_parser = sub.add_parser("bundle")
    bundle_parser.add_argument("output", type=Path)
    bundle_parser.add_argument("--manifest", type=Path, required=True)
    bundle_parser.add_argument("--tck-reports", type=Path, required=True)
    bundle_parser.add_argument("--kyber-junit", type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == "validate":
            validate()
        elif args.command == "generate":
            generate()
        elif args.command == "affected":
            paths = args.paths or sys.stdin.read().splitlines()
            print("true" if affected(paths) else "false")
        elif args.command == "evidence":
            scan_evidence(args.directory)
        elif args.command == "bundle":
            bundle_evidence(args.output, args.manifest, args.tck_reports, args.kyber_junit)
    except GateError as exc:
        print(f"A2A conformance gate: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
