#!/usr/bin/env python3
"""Run Kyber's opt-in synthetic Ori Eval slate without publishing raw turns."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / "docs/examples/ori-eval/kyber-synthetic.eval.ts"
FIXTURE_SHA256 = "1230d9e185774398bcb920c0a0c28936967592ab4824fa2fef0d61f37879705d"
MODELS = ("cohere/north-mini-code:free", "nvidia/nemotron-3.5-lightning:free")
ORI_VERSION = "0.15.6+4855f79"
ORI_SHA256 = "d3525283d0431197943445c499edf8d790d13816752efd0e373afe2da75e035c"
BUN_VERSION = "1.4.2"
ANCILLARY_MODEL = "typesafe/jev-1.13"
API = "https://openrouter.ai/api/v1"


class EvalError(Exception):
    pass


def request_json(url: str, key: str, retries: int = 0) -> dict:
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {key}"})
    for attempt in range(retries + 1):
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                body = json.load(response)
            break
        except urllib.error.HTTPError as exc:
            if exc.code != 404 or attempt == retries:
                raise
            time.sleep(min(2 ** attempt, 4))
    if not isinstance(body, dict) or not isinstance(body.get("data"), dict):
        raise EvalError("OpenRouter returned an unexpected metadata response")
    return body["data"]


def check_key(key: str, allow_uncapped: bool) -> bool:
    data = request_json(f"{API}/key", key)
    limit = data.get("limit")
    if limit is None:
        if not allow_uncapped:
            raise EvalError("OpenRouter key has no provider limit; use a capped key or explicitly pass --allow-uncapped")
        return False
    if not isinstance(limit, (int, float)) or limit <= 0:
        raise EvalError("OpenRouter key limit is invalid or exhausted")
    usage = data.get("usage")
    if not isinstance(usage, (int, float)) or usage >= limit:
        raise EvalError("OpenRouter key limit is exhausted or usage is unknown")
    return True


def verify_binary(ori: Path, bun: Path, env: dict[str, str]) -> None:
    if not ori.is_file() or not os.access(ori, os.X_OK):
        raise EvalError("Ori executable is missing")
    digest = hashlib.sha256()
    with ori.open("rb") as binary:
        for chunk in iter(lambda: binary.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != ORI_SHA256:
        raise EvalError("Ori binary does not match the pinned Linux SHA-256")
    if not bun.is_file() or not os.access(bun, os.X_OK):
        raise EvalError("Bun executable is missing")
    ori_version = subprocess.run([str(ori), "--version", "--human"], env=env, text=True, capture_output=True, timeout=20)
    bun_version = subprocess.run([str(bun), "--version"], env=env, text=True, capture_output=True, timeout=10)
    if ori_version.returncode or ORI_VERSION not in ori_version.stdout:
        raise EvalError("Ori version does not match the pinned release")
    if bun_version.returncode or bun_version.stdout.strip() != BUN_VERSION:
        raise EvalError("Bun version does not match the pinned release")


def run_ori(ori: Path, args: list[str], workspace: Path, env: dict[str, str], timeout: int) -> tuple[int, dict | None]:
    try:
        proc = subprocess.run([str(ori), "eval", *args], cwd=workspace, env=env, text=True,
                              capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise EvalError(f"Ori eval exceeded its {timeout}s process limit") from exc
    try:
        payload = json.loads(proc.stdout)
    except json.JSONDecodeError:
        payload = None
    return proc.returncode, payload


def verify_scope(data: dict, expected_runs: int) -> None:
    scope = data.get("scope", {})
    if sorted(scope.get("models", [])) != sorted(MODELS) or scope.get("runs") != expected_runs:
        raise EvalError("Ori ran an unexpected model or number of cases")
    if len(data.get("results", [])) != expected_runs or len(data.get("tests", [])) != expected_runs:
        raise EvalError("Ori returned an incomplete result set")


def classify_generation(record: dict, requested_model: str) -> str:
    model = record.get("model", "")
    api_type = record.get("api_type", "")
    if isinstance(model, str) and model.startswith(requested_model.split(":")[0]) and api_type != "decisions":
        return "candidate"
    if isinstance(model, str) and model.startswith(ANCILLARY_MODEL) and api_type == "decisions":
        return "ancillary"
    return "unknown"


def summarize(data: dict, key: str | None, phase: str) -> dict:
    expected = 2 if phase == "pilot" else 4
    verify_scope(data, expected)
    models = {model: {"runs": 0, "gradedPasses": 0, "durationMs": [], "reportedCostUsd": 0.0,
                      "verifiedCandidateCostUsd": 0.0, "verifiedAncillaryCostUsd": 0.0,
                      "candidateProviders": set(), "ancillaryProviders": set(),
                      "generationMetadataComplete": True} for model in MODELS}
    problems: list[str] = []
    for run in data["results"]:
        model = run.get("model")
        if model not in models:
            raise EvalError("Ori returned an unapproved candidate model")
        row = models[model]
        row["runs"] += 1
        row["gradedPasses"] += run.get("outcome") == "passed"
        row["durationMs"].append(round(run.get("durationMs", 0)))
        usage = run.get("terminal", {}).get("payload", {}).get("usage", {})
        reported = usage.get("costUsd")
        if not isinstance(reported, (int, float)):
            problems.append(f"{model}: cost not reported")
            row["generationMetadataComplete"] = False
            continue
        row["reportedCostUsd"] += reported
        ids = usage.get("generationIds", [])
        if not key or not ids or len(ids) > 4:
            problems.append(f"{model}: generation evidence unavailable or unexpectedly large")
            row["generationMetadataComplete"] = False
            continue
        verified = 0.0
        for generation_id in ids:
            if not isinstance(generation_id, str) or not generation_id.startswith("gen-"):
                problems.append(f"{model}: invalid generation ID")
                row["generationMetadataComplete"] = False
                continue
            try:
                record = request_json(f"{API}/generation?id={urllib.parse.quote(generation_id)}", key, retries=4)
            except urllib.error.URLError:
                problems.append(f"{model}: generation metadata unavailable")
                row["generationMetadataComplete"] = False
                continue
            cost = record.get("total_cost")
            category = classify_generation(record, model)
            if not isinstance(cost, (int, float)) or cost < 0 or category == "unknown":
                problems.append(f"{model}: unknown generation cost or model")
                row["generationMetadataComplete"] = False
                continue
            row[f"verified{category.title()}CostUsd"] += cost
            provider = record.get("provider_name")
            if isinstance(provider, str) and provider:
                row[f"{category}Providers"].add(provider)
            verified += cost
        if row["generationMetadataComplete"] and abs(verified - reported) > 0.00000001:
            problems.append(f"{model}: Ori and OpenRouter cost differ")
    tests = [{"name": item.get("name"), "status": item.get("status")}
             for item in data["tests"]]
    if sum(item["status"] == "pass" for item in tests) != sum(row["gradedPasses"] for row in models.values()):
        problems.append("test outcomes and attributed model grades differ")
    for row in models.values():
        if row["runs"] != expected // len(MODELS):
            problems.append("candidate case counts differ")
        row["avgDurationMs"] = round(sum(row.pop("durationMs")) / row["runs"]) if row["runs"] else None
        for field in ("reportedCostUsd", "verifiedCandidateCostUsd", "verifiedAncillaryCostUsd"):
            row[field] = round(row[field], 9)
        for field in ("candidateProviders", "ancillaryProviders"):
            row[field] = sorted(row[field])
    return {"phase": phase, "tests": tests, "models": models, "problems": sorted(set(problems))}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ori", default=shutil.which("ori"))
    parser.add_argument("--bun", default=shutil.which("bun"))
    parser.add_argument("--preflight-only", action="store_true", help="list and parse fixture without model calls or a key")
    parser.add_argument("--full", action="store_true", help="run the full four-case comparison after a two-case pilot")
    parser.add_argument("--allow-uncapped", action="store_true", help="explicitly waive the provider-limit check for this bounded run")
    parser.add_argument("--output", type=Path, help="write the aggregate JSON report to this path")
    args = parser.parse_args()
    if not args.ori or not args.bun:
        raise EvalError("pinned Ori and Bun executables are required (--ori and --bun)")
    ori, bun = Path(args.ori).resolve(), Path(args.bun).absolute()
    fixture_digest = hashlib.sha256(FIXTURE.read_bytes()).hexdigest()
    if fixture_digest != FIXTURE_SHA256:
        raise EvalError("fixture differs from the reviewed version; update the pin only after review")
    key = os.environ.get("OPENROUTER_API_KEY")
    if not args.preflight_only and not key:
        raise EvalError("OPENROUTER_API_KEY is required for model calls")
    if not args.preflight_only and os.environ.get("KYBER_POD_UID"):
        raise EvalError("model calls must run on a disposable external runner, not inside a Kyber agent pod")
    capped = None if args.preflight_only else check_key(key, args.allow_uncapped)
    with tempfile.TemporaryDirectory(prefix="kyber-ori-eval-") as temporary:
        workspace = Path(temporary)
        (workspace / "evals").mkdir()
        (workspace / "features").mkdir()
        (workspace / "temporary").mkdir()
        shutil.copy2(FIXTURE, workspace / "evals" / FIXTURE.name)
        env = {name: value for name, value in os.environ.items() if name in ("PATH", "HOME", "LANG", "LC_ALL")}
        env["PATH"] = str(bun.parent) + os.pathsep + env.get("PATH", "")
        env.update({"TMPDIR": str(workspace / "temporary"), "ORI_EVAL_OPT_IN": "1", "ORI_DISABLE_UPDATES": "1",
                    "ORI_TELEMETRY": "0", "ORI_FORCE_OPENROUTER_API_KEY": "1", "ORI_FALLBACK_MODELS": "",
                    "ORI_MAX_TURNS": "1"})
        if key:
            env["OPENROUTER_API_KEY"] = key
        verify_binary(ori, bun, env)
        common = ["--hermetic", "--no-history", "--features", str(workspace / "features"),
                  "--timeout", "90000", str(workspace / "evals")]
        code, _ = run_ori(ori, ["--list", "--allow-no-key", str(workspace / "evals")], workspace, env, 30)
        if code:
            raise EvalError("Ori fixture discovery failed")
        code, dry_payload = run_ori(ori, ["--dry-run", *common], workspace, env, 40)
        if code:
            error_code = (dry_payload or {}).get("error", {}).get("code", "unknown")
            raise EvalError(f"Ori fixture import check failed ({error_code})")
        report = {"fixtureSha256": fixture_digest,
                  "oriVersion": ORI_VERSION, "bunVersion": BUN_VERSION, "providerLimitPresent": capped,
                  "uncappedWaiverUsed": bool(args.allow_uncapped and capped is False), "preflightPassed": True,
                  "scope": "synthetic model screening; no Kyber agent or model change"}
        if not args.preflight_only:
            code, payload = run_ori(ori, ["--pilot", "1", *common], workspace, env, 130)
            if not payload or not isinstance(payload.get("data"), dict):
                raise EvalError("Ori pilot did not return a structured result")
            report["pilot"] = summarize(payload["data"], key, "pilot")
            report["pilotExitCode"] = code
            if code or report["pilot"]["problems"] or any(t["status"] != "pass" for t in report["pilot"]["tests"]):
                report["stoppedAfterPilot"] = True
            elif args.full:
                code, payload = run_ori(ori, common, workspace, env, 180)
                if not payload or not isinstance(payload.get("data"), dict):
                    raise EvalError("Ori full comparison did not return a structured result")
                report["full"] = summarize(payload["data"], key, "full")
                report["fullExitCode"] = code
        report["observedTotalCostUsd"] = round(sum(sum(row["reportedCostUsd"] for row in phase["models"].values())
                                                    for phase in (report.get("pilot"), report.get("full")) if phase), 9)
        serialized = json.dumps(report, indent=2, sort_keys=True) + "\n"
        if args.output:
            output_fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            os.fchmod(output_fd, 0o600)
            with os.fdopen(output_fd, "w") as output_file:
                output_file.write(serialized)
        else:
            sys.stdout.write(serialized)
        if any(phase.get("problems") or any(t["status"] != "pass" for t in phase["tests"])
               for phase in (report.get("pilot"), report.get("full")) if phase):
            return 1
        if report.get("pilotExitCode", 0) or report.get("fullExitCode", 0):
            return 1
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (EvalError, OSError, subprocess.TimeoutExpired, urllib.error.URLError) as exc:
        print(f"ori-eval-comparison: {exc}", file=sys.stderr)
        raise SystemExit(2)
