#!/usr/bin/python3
"""Count native Claude turns and registered Bash children, never arbitrary PIDs.

Invoked as a native Bash override, a PreToolUse command hook, or by the native
mod. Commands are hashed locally and never written to disk or reported.
"""
from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import sys
import tempfile
from datetime import datetime, timezone

ROOT = Path(os.environ.get("KYBER_CLAUDE_WORK_ROOT", "/tmp/kyber-native-activity/claude"))
CONFIG = Path(os.environ.get("CLAUDE_CONFIG_DIR", str(Path.home() / ".claude")))
MAX_JOBS = 1024
MAX_BYTES = 1 << 20


def process(pid):
    raw = Path(f"/proc/{pid}/stat").read_text()
    fields = raw[raw.rfind(")") + 2:].split()
    return fields[19], int(fields[1]), fields[0]


def read_json(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd) as handle:
        data = handle.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError("observer record exceeds bound")
    return json.loads(data)


def atomic_json(path, body):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".work-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as handle:
            json.dump(body, handle)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def owner(explicit=None):
    """Only accept a native session in this helper's actual ancestry."""
    ancestor = os.getppid()
    for _ in range(24):
        if ancestor <= 1:
            break
        ticks, parent, state = process(ancestor)
        if state not in ("Z", "X") and (explicit is None or ancestor == int(explicit)):
            try:
                native = read_json(CONFIG / "sessions" / f"{ancestor}.json")
                version = tuple(int(p) for p in native["version"].split(".")[:3])
                if native["pid"] == ancestor and version >= (2, 1, 330):
                    return ancestor, ticks, native
            except (OSError, ValueError, KeyError, TypeError):
                pass
        ancestor = parent
    raise ValueError("no qualified native owner")


@contextlib.contextmanager
def registry(pid, ticks):
    folder = ROOT / "registry"
    folder.mkdir(mode=0o700, parents=True, exist_ok=True)
    name = f"{pid}-{ticks}"
    fd = os.open(folder / f"{name}.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "r+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = folder / f"{name}.json"
        try:
            data = read_json(path)
        except FileNotFoundError:
            data = {"calls": {}, "jobs": {}, "complete": True, "lastWorkedAt": None}
        if len(data["calls"]) > MAX_JOBS or len(data["jobs"]) > MAX_JOBS:
            raise ValueError("observer registry exceeds bound")
        yield data
        atomic_json(path, data)


def command_hash(command):
    return hashlib.sha256(command.encode()).hexdigest()


def native_command(argv):
    # Native Bash execution ends with this generated cwd marker. Shell
    # snapshots, ordinary hook commands and unrelated Bash invocations do not.
    if "-c" not in argv or not argv:
        return None
    if not re.search(r"&&\s*pwd -P >\|", argv[-1]):
        return None
    tokens = shlex.split(argv[-1])
    if len(tokens) < 7 or tokens[-4:-1] != ["pwd", "-P", ">|"]:
        return None
    if not re.search(r"/(?:claude-[A-Za-z0-9_-]{4,64}-cwd|cwd-[A-Za-z0-9_-]{4,64})$", tokens[-1]):
        return None
    if tokens[-5] != "&&":
        return None
    index = max(i for i, token in enumerate(tokens[:-5]) if token == "eval")
    arguments = tokens[index + 1:-5]
    if arguments[-2:] == ["<", "/dev/null"]:
        arguments = arguments[:-2]
    if len(arguments) != 1:
        raise ValueError("native command shape is not qualified")
    return arguments[0]


def register_shell(argv):
    command = native_command(argv)
    if command is None:
        return
    pid, ticks, _ = owner()
    child = os.getpid()
    child_ticks, _, _ = process(child)
    with registry(pid, ticks) as data:
        matches = [key for key, value in data["calls"].items()
                   if value["hash"] == command_hash(command) and not value.get("job")]
        call = matches[0] if len(matches) == 1 else None
        if len(data["jobs"]) >= MAX_JOBS:
            data["complete"] = False
            return
        identity = f"{child}-{child_ticks}"
        data["jobs"][identity] = {"pid": child, "startTicks": child_ticks, "call": call}
        if call:
            data["calls"][call]["job"] = identity


def hook(body):
    if body.get("tool_name") not in ("Bash", "Monitor"):
        return
    pid, ticks, _ = owner(os.environ.get("CLAUDE_PID"))
    call = body.get("tool_use_id")
    arguments = body.get("tool_input", {})
    command = arguments.get("command")
    if not isinstance(call, str) or not call or len(call) > 256 or not isinstance(command, str):
        raise ValueError("native call identity unavailable")
    with registry(pid, ticks) as data:
        if call in data["calls"] or len(data["calls"]) >= MAX_JOBS:
            data["complete"] = False
            return
        data["calls"][call] = {"hash": command_hash(command),
                               "background": arguments.get("run_in_background") is True or body["tool_name"] == "Monitor",
                               "agent": body.get("agent_id"), "returned": False}


def returned(body):
    pid, ticks, _ = owner(body.get("nativePID"))
    ids = body.get("ids")
    if not isinstance(ids, list) or len(ids) > MAX_JOBS:
        raise ValueError("native results exceed bound")
    with registry(pid, ticks) as data:
        for identity in ids:
            if identity in data["calls"]:
                data["calls"][identity]["returned"] = True


def publish(body):
    pid, ticks, native = owner(body.get("nativePID"))
    observed = datetime.fromisoformat(body["observedAt"].replace("Z", "+00:00"))
    now = datetime.now(timezone.utc)
    if observed.tzinfo is None or not 0 <= (now - observed).total_seconds() <= 10:
        raise ValueError("native snapshot is not current")
    observed_at = observed.isoformat(timespec="microseconds")
    main = body.get("main")
    agents = body.get("agents")
    if not isinstance(main, bool) or not isinstance(agents, list) or len(agents) > MAX_JOBS:
        raise ValueError("native turn inventory unavailable")
    if any(not isinstance(a, dict) or not isinstance(a.get("id"), str) or not a["id"]
           or len(a["id"]) > 256 or not isinstance(a.get("status"), str) for a in agents):
        raise ValueError("native turn identity unavailable")
    ids = {a["id"] for a in agents}
    if len(ids) != len(agents):
        raise ValueError("native turn inventory duplicated")
    active_ids = {a["id"] for a in agents if a["status"] in ("running", "pending")}
    foreground = int(main) + sum(a["id"] in active_ids and a.get("background") is False for a in agents)
    background = sum(a["id"] in active_ids and a.get("background") is True for a in agents)
    ownership_known = all(isinstance(a.get("background"), bool) for a in agents if a["id"] in active_ids)
    needs_input = native.get("status") == "waiting" and native.get("waitingFor") in (
        "permission prompt", "input needed", "goal proposal", "worker request", "sandbox request")
    if needs_input:
        foreground = 0
    with registry(pid, ticks) as data:
        if body.get("eventGap"):
            data["complete"] = False
        complete = data["complete"] and body.get("complete") is True and ownership_known
        live_calls = set()
        for identity, job in list(data["jobs"].items()):
            try:
                current_ticks, _, state = process(job["pid"])
                alive = current_ticks == job["startTicks"] and state not in ("Z", "X")
            except FileNotFoundError:
                alive = False
            if not alive:
                del data["jobs"][identity]
                continue
            call_id = job.get("call")
            call = data["calls"].get(call_id)
            if call is None:
                complete = False
                continue
            live_calls.add(call_id)
            executing_owner = main if not call.get("agent") else call["agent"] in active_ids
            if call["background"] or call["returned"] or not executing_owner:
                background += 1
        for identity, call in list(data["calls"].items()):
            if call.get("returned") and identity not in live_calls:
                # Preserve a returned-but-not-yet-registered launch until the
                # live native session proves it has no owned work. A slow
                # child may still be entering this Bash override.
                if call.get("job") or (native.get("status") == "idle" and not main):
                    del data["calls"][identity]
        if native.get("status") == "shell" and not background:
            complete = False
        if native.get("status") == "busy" and not (foreground or background or needs_input):
            complete = False
        if any(a["status"] not in ("running", "pending", "idle", "waiting", "completed", "failed", "killed") for a in agents):
            complete = False
        if foreground > MAX_JOBS or background > MAX_JOBS:
            raise ValueError("native work inventory exceeds bound")
        last = body.get("lastWorkedAt")
        history = [datetime.fromisoformat(value.replace("Z", "+00:00"))
                   for value in (data.get("lastWorkedAt"), last) if value]
        if any(value.tzinfo is None or value > now for value in history):
            raise ValueError("invalid native work time")
        last = min(max(history), observed).isoformat(timespec="microseconds") if history else None
        if foreground or background:
            last = observed_at
        # A fresh native zero-work snapshot plus absence of all registered
        # children can recover a transient event gap. Quiet time alone cannot.
        if (native.get("status") == "idle" and not main and not background
                and not active_ids and not data["jobs"] and body.get("complete") is True):
            data["complete"] = True
            complete = True
        data["lastWorkedAt"] = last
        snapshot = {"observedAt": observed_at, "lastWorkedAt": last, "foreground": foreground,
                    "background": background, "complete": complete, "needsInput": needs_input}
        atomic_json(ROOT / f"{pid}-{ticks}.json", {"version": 1, "pid": pid, "startTicks": ticks,
                    "podUID": os.environ.get("KYBER_POD_UID", ""), "closed": body.get("closed") is True,
                    "snapshot": snapshot})
    reconcile_ended_owners(pid, ticks)


def reconcile_ended_owners(current_pid, current_ticks):
    """Recover same-pod CLI restarts only when every registered life ended.

    In-process turns cannot outlive their native owner. Bash children can, so
    their registered PID/start-time pairs must each be gone. An incomplete
    owner or a launch with no registered child retains the coverage gap.
    Nothing is re-timestamped and no process scan or cancellation occurs.
    """
    paths = list(ROOT.glob("*.json"))
    if len(paths) > 4096:
        return
    pod_uid = os.environ.get("KYBER_POD_UID", "")
    for path in paths:
        if path.name == f"{current_pid}-{current_ticks}.json":
            continue
        try:
            source = read_json(path)
            pid, ticks = source["pid"], source["startTicks"]
            if (not isinstance(pid, int) or pid <= 0 or not isinstance(ticks, str)
                    or not ticks.isdigit() or path.name != f"{pid}-{ticks}.json"
                    or not pod_uid or source["podUID"] != pod_uid
                    or source.get("version") != 1 or not source["snapshot"]["complete"]):
                continue
            try:
                actual, _, state = process(pid)
                if actual == ticks and state not in ("Z", "X"):
                    continue
            except FileNotFoundError:
                pass
            state_path = ROOT / "registry" / path.name
            if not state_path.is_file():
                continue
            with registry(pid, ticks) as data:
                if not data["complete"] or any(not call.get("job") for call in data["calls"].values()):
                    continue
                alive = False
                for job in data["jobs"].values():
                    try:
                        actual, _, state = process(job["pid"])
                        if actual == job["startTicks"] and state not in ("Z", "X"):
                            alive = True
                    except FileNotFoundError:
                        pass
                if not alive:
                    path.unlink(missing_ok=True)
            if not path.exists():
                state_path.unlink(missing_ok=True)
        except (OSError, ValueError, KeyError, TypeError):
            # A malformed source remains unavailable; it is not absence proof.
            continue


def observation_gap():
    try:
        pid, ticks, _ = owner()
        with registry(pid, ticks) as data:
            data["complete"] = False
    except Exception:
        pass


def main():
    # The observed-bash symlink forwards exactly the native argv, stdin,
    # environment, cwd, exit status and signal identity through exec. Unlike
    # CLAUDE_CODE_SHELL_PREFIX it does not change stdio MCP argument handling.
    if "bash" in Path(sys.argv[0]).name:
        try:
            register_shell(sys.argv[1:])
        except Exception:
            observation_gap()
        os.execv("/bin/bash", ["bash", *sys.argv[1:]])
    try:
        raw = sys.stdin.read(MAX_BYTES + 1)
        if len(raw) > MAX_BYTES:
            raise ValueError("native observer input exceeds bound")
        body = json.loads(raw)
        {"hook": hook, "publish": publish, "returned": returned}[sys.argv[1]](body)
    except Exception:
        observation_gap()
        if len(sys.argv) <= 1 or sys.argv[1] != "hook":
            raise SystemExit(1)
    if len(sys.argv) > 1 and sys.argv[1] == "hook":
        print("{}")


if __name__ == "__main__":
    main()
