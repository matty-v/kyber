#!/usr/bin/env python3
"""Read only whitelisted native SDK model metadata; never perform inference."""
import json
import os
import re
import selectors
import signal
import subprocess
import sys
import time

LEVEL = re.compile(r"[a-z][a-z0-9_-]{0,31}\Z")
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?\Z")
# Older SDKs omitted resolvedModel. Keep only the identities qualified for the
# baked-in client; effort levels always come from that client's initialize reply.
LEGACY_IDENTITIES = {
    "2.1.119": {"default": "claude-sonnet-4-6", "sonnet[1m]": "claude-sonnet-4-6",
                "opus[1m]": "claude-opus-4-7"},
}


def normalize_models(models, version):
    if not isinstance(models, list) or len(models) > 100:
        raise ValueError("invalid native catalog")
    result = {}
    for model in models:
        if not isinstance(model, dict):
            raise ValueError("invalid native model")
        alias = model.get("value")
        resolved = model.get("resolvedModel") or LEGACY_IDENTITIES.get(version, {}).get(alias)
        levels = model.get("supportedEffortLevels")
        if model.get("supportsEffort") is not True:
            continue
        if not isinstance(levels, list) or not levels or len(levels) > 16:
            continue
        if any(not isinstance(x, str) or not LEVEL.fullmatch(x) for x in levels) or len(set(levels)) != len(levels):
            raise ValueError("invalid native efforts")
        for identity in (alias, resolved):
            if identity is None:
                continue
            if not isinstance(identity, str) or not identity or len(identity) > 128:
                raise ValueError("invalid native identity")
            # The launch script opts in to the context variant. Context does not
            # change the native effort dial; this suffix is Claude's own syntax.
            identities = (identity, identity + "[1m]") if not identity.endswith("[1m]") else (identity, identity[:-4])
            for value in identities:
                evidence = {"id": value, "reasoningEffort": {"source": "claude-sdk-initialize", "harnessVersion": version, "levels": list(levels)}}
                if value in result and result[value] != evidence:
                    raise ValueError("ambiguous native identity")
                result[value] = evidence
    return list(result.values())


def catalog(timeout=20):
    version_run = subprocess.run(["claude", "--version"], capture_output=True, timeout=5, check=True)
    version = version_run.stdout.decode().split()[0]
    if len(version) > 64 or not VERSION.fullmatch(version):
        raise ValueError("invalid native version")
    env = {**os.environ, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1"}
    command = ["claude", "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
               "--setting-sources", "", "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
               "--settings", '{"disableAllHooks":true}']
    process = subprocess.Popen(command, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, start_new_session=True)
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ)
    deadline = time.monotonic() + timeout
    buffer = b""
    total = 0
    try:
        request = {"type": "control_request", "request_id": "kyber-native-effort", "request": {"subtype": "initialize", "hooks": {}}}
        process.stdin.write(json.dumps(request).encode() + b"\n")
        process.stdin.flush()
        while time.monotonic() < deadline:
            if not selector.select(max(0, deadline - time.monotonic())):
                break
            chunk = os.read(process.stdout.fileno(), 65536)
            if not chunk:
                break
            total += len(chunk)
            if total > 2 * 1024 * 1024:
                raise ValueError("native catalog too large")
            buffer += chunk
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                data = json.loads(line)
                if data.get("type") != "control_response":
                    continue
                response = data.get("response", {})
                if response.get("request_id") != "kyber-native-effort" or response.get("subtype") != "success":
                    raise ValueError("native initialize failed")
                return {"version": version, "models": normalize_models(response.get("response", {}).get("models"), version)}
        raise ValueError("native initialize unavailable")
    finally:
        selector.close()
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        process.stdin.close()
        process.stdout.close()


def main():
    try:
        data = catalog()
        if sys.argv[1:] == ["--catalog"]:
            print(json.dumps(data))
            return 0
        if len(sys.argv) == 3 and LEVEL.fullmatch(sys.argv[2]):
            matches = [m for m in data["models"] if m["id"] == sys.argv[1]]
            if len(matches) == 1 and sys.argv[2] in matches[0]["reasoningEffort"]["levels"]:
                return 0
    except Exception:
        pass
    print("[kyber] Native model effort could not be verified; refresh model information or reset effort to Default", file=sys.stderr)
    return 43


if __name__ == "__main__":
    raise SystemExit(main())
