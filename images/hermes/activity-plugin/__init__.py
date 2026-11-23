"""Native in-process Hermes work observer. Never changes hook results/context."""
from __future__ import annotations

import atexit
import json
import threading
import os
from pathlib import Path
import tempfile
from datetime import datetime, timezone


def utc_now():
    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


class WorkObserver:
    def __init__(self, registry):
        self.registry = registry
        self.lock = threading.Lock()
        self.turns = set()
        self.complete = True
        self.last_worked = None

    @staticmethod
    def turn_key(payload):
        session = payload.get("session_id")
        turn = payload.get("turn_id")
        if not isinstance(session, str) or not session or not isinstance(turn, str) or not turn:
            return None
        return session, turn

    def begin(self, **payload):
        key = self.turn_key(payload)
        with self.lock:
            if key is None or len(self.turns) >= 1024:
                self.complete = False
            else:
                self.turns.add(key)
                self.last_worked = utc_now()
        # None preserves the hook's original prompt/context.

    def end(self, **payload):
        key = self.turn_key(payload)
        with self.lock:
            # CLI exit also fires reduced legacy shapes. They do not erase
            # another turn; a missing canonical finalizer leaves coverage dark.
            if key is not None:
                if key in self.turns:
                    self.turns.discard(key)
                    self.last_worked = utc_now()

    def snapshot(self):
        with self.lock:
            foreground = len(self.turns)
            complete = self.complete
        background = 0
        try:
            # This is the live registry in the harness process, not a fresh
            # registry imported by a separate shell-hook subprocess.
            jobs = self.registry.list_sessions()
            if not isinstance(jobs, list) or len(jobs) > 4096:
                raise ValueError("invalid native inventory")
            ids = set()
            for job in jobs:
                status = job.get("status")
                if status not in ("running", "exited"):
                    raise ValueError("unknown native process state")
                if status == "running":
                    identity = job.get("session_id")
                    if not identity or identity in ids:
                        raise ValueError("incomplete native process identity")
                    ids.add(identity)
                    if job.get("detached"):
                        # Crash-recovered handles need additional qualification;
                        # never silently call their inventory empty/complete.
                        complete = False
                    background += 1
            if background > 1024:
                raise ValueError("native inventory exceeds bounds")
        except Exception:
            complete = False
            background = 0
        now = utc_now()
        if foreground or background:
            self.last_worked = now
        return {"observedAt": now, "lastWorkedAt": self.last_worked,
                "foreground": foreground, "background": background,
                "complete": complete, "needsInput": False}

    def run(self):
        # Each native process publishes separately. The external reporter
        # verifies PID start ticks and aggregates all live publishers, so a
        # second CLI cannot overwrite another session's foreground/background.
        pid = os.getpid()
        stat = Path(f"/proc/{pid}/stat").read_text()
        start_ticks = stat[stat.rfind(")") + 2:].split()[19]
        root = Path("/tmp/kyber-native-activity/hermes")
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
        destination = root / f"{pid}-{start_ticks}.json"
        def publish(closed=False):
            tmp = None
            try:
                body = {"version": 1, "pid": pid, "startTicks": start_ticks,
                        "podUID": os.environ.get("KYBER_POD_UID", ""),
                        "closed": closed, "snapshot": self.snapshot()}
                fd, tmp = tempfile.mkstemp(prefix=".work-", dir=root)
                with os.fdopen(fd, "w") as handle:
                    json.dump(body, handle)
                os.replace(tmp, destination)
            except Exception:
                pass
            finally:
                if tmp and os.path.exists(tmp):
                    os.unlink(tmp)
        shutdown = threading.Event()
        done = threading.Event()
        def close():
            shutdown.set()
            # A failed/native-blocked observer cannot hold harness exit hostage.
            # An unfinished close remains an explicit coverage gap.
            done.wait(0.1)
        atexit.register(close)
        while not shutdown.is_set():
            publish()
            shutdown.wait(5)
        publish(closed=True)
        done.set()


def register(ctx):
    from tools.process_registry import process_registry
    observer = WorkObserver(process_registry)
    ctx.register_hook("pre_llm_call", observer.begin)
    ctx.register_hook("on_session_end", observer.end)
    threading.Thread(target=observer.run, name="kyber-native-work", daemon=True).start()
