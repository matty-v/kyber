import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
from datetime import datetime, timedelta, timezone


SOURCE = Path(__file__).with_name("helper.py")
spec = importlib.util.spec_from_file_location("claude_work_helper", SOURCE)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class NativeWorkTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.pid = os.getpid()
        self.ticks = helper.process(self.pid)[0]
        self.native = {"status": "idle"}
        self.root_patch = patch.object(helper, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        self.owner_patch = patch.object(helper, "owner", return_value=(self.pid, self.ticks, self.native))
        self.owner_patch.start()
        self.addCleanup(self.owner_patch.stop)

    def frame(self, main=False, **overrides):
        return {"nativePID": str(self.pid), "observedAt": datetime.now(timezone.utc).isoformat(),
                "main": main, "agents": [], "complete": True, **overrides}

    def snapshot(self):
        return helper.read_json(self.root / f"{self.pid}-{self.ticks}.json")["snapshot"]

    def call(self, identity="call-1", background=False):
        helper.hook({"tool_name": "Bash", "tool_use_id": identity,
                     "tool_input": {"command": "sleep 60", "run_in_background": background}})

    def child(self):
        child = subprocess.Popen(["sleep", "60"])
        def close():
            if child.poll() is None:
                child.terminate()
                child.wait(timeout=5)
        self.addCleanup(close)
        # A real registered child, not a fixture that counts every OS process.
        with patch.object(helper.os, "getpid", return_value=child.pid):
            helper.register_shell(["-c", "eval 'sleep 60' < /dev/null && pwd -P >| '/tmp/claude-abcd-cwd'"])
        return child

    def test_native_subagent_scheduling_owns_foreground_and_background(self):
        self.native["status"] = "busy"
        agents = [{"id": "sync", "status": "running", "background": False},
                  {"id": "async", "status": "running", "background": True}]
        helper.publish(self.frame(main=True, agents=agents))
        self.assertEqual((self.snapshot()["foreground"], self.snapshot()["background"]), (2, 1))
        self.assertTrue(self.snapshot()["complete"])
        del agents[0]["background"]
        helper.publish(self.frame(main=True, agents=agents))
        self.assertFalse(self.snapshot()["complete"])

    def test_return_before_shell_registration_preserves_native_handoff(self):
        self.call()
        helper.returned({"nativePID": str(self.pid), "ids": ["call-1"]})
        child = self.child()
        self.native["status"] = "shell"
        helper.publish(self.frame())
        self.assertTrue(self.snapshot()["complete"])
        self.assertEqual(self.snapshot()["background"], 1)

    def test_ended_owner_reconciliation_retains_live_or_uncertain_children(self):
        old_pid, old_ticks = 999999999, "1"
        old = self.root / f"{old_pid}-{old_ticks}.json"
        with patch.dict(os.environ, {"KYBER_POD_UID": "canary"}):
            def source():
                helper.atomic_json(old, {"version": 1, "pid": old_pid, "startTicks": old_ticks,
                                        "podUID": "canary", "snapshot": {"complete": True}})
            source()
            with helper.registry(old_pid, old_ticks) as data:
                data["calls"]["pending"] = {"returned": True}
            helper.reconcile_ended_owners(self.pid, self.ticks)
            self.assertTrue(old.exists())
            with helper.registry(old_pid, old_ticks) as data:
                data["calls"].clear()
                data["jobs"]["live"] = {"pid": self.pid, "startTicks": self.ticks}
            helper.reconcile_ended_owners(self.pid, self.ticks)
            self.assertTrue(old.exists())
            with helper.registry(old_pid, old_ticks) as data:
                data["jobs"]["live"]["startTicks"] = "prior-life"
            helper.reconcile_ended_owners(self.pid, self.ticks)
            self.assertFalse(old.exists())
            source()
            with helper.registry(old_pid, old_ticks) as data:
                data["complete"] = False
            helper.reconcile_ended_owners(self.pid, self.ticks)
            self.assertTrue(old.exists())

    def test_manual_handoff_and_last_job_exit(self):
        self.call()
        child = self.child()
        self.native["status"] = "busy"
        helper.publish(self.frame(main=True))
        self.assertEqual((self.snapshot()["foreground"], self.snapshot()["background"]), (1, 0))
        # A native tool-result row means the call returned. If its registered
        # Bash process remains alive, it was handed off rather than completed.
        helper.returned({"nativePID": str(self.pid), "ids": ["call-1"]})
        helper.publish(self.frame(main=True))
        self.assertEqual((self.snapshot()["foreground"], self.snapshot()["background"]), (1, 1))
        self.native["status"] = "shell"
        helper.publish(self.frame())
        self.assertEqual((self.snapshot()["foreground"], self.snapshot()["background"]), (0, 1))
        last = self.snapshot()["lastWorkedAt"]
        child.terminate()
        child.wait(timeout=5)
        self.native["status"] = "idle"
        helper.publish(self.frame())
        result = self.snapshot()
        self.assertTrue(result["complete"])
        self.assertEqual(result["background"], 0)
        self.assertEqual(result["lastWorkedAt"], last)

    def test_ambiguous_call_match_is_incomplete_until_children_exit(self):
        self.call("first", True)
        self.call("second", False)
        child = self.child()
        self.native["status"] = "shell"
        helper.publish(self.frame())
        self.assertFalse(self.snapshot()["complete"])
        child.terminate()
        child.wait(timeout=5)
        self.native["status"] = "idle"
        helper.publish(self.frame())
        self.assertTrue(self.snapshot()["complete"])
        self.assertEqual(self.snapshot()["background"], 0)

    def test_pid_reuse_cannot_keep_a_job_alive(self):
        self.call(background=True)
        child = self.child()
        with helper.registry(self.pid, self.ticks) as data:
            next(iter(data["jobs"].values()))["startTicks"] = "different-life"
        helper.publish(self.frame())
        self.assertEqual(self.snapshot()["background"], 0)
        child.terminate()
        child.wait(timeout=5)

    def test_missing_native_shell_and_idle_roster_are_not_zero_work_proof(self):
        self.native["status"] = "shell"
        helper.publish(self.frame())
        self.assertFalse(self.snapshot()["complete"])
        self.native["status"] = "idle"
        helper.publish(self.frame(agents=[{"id": "teammate", "status": "idle"}]))
        self.assertTrue(self.snapshot()["complete"])
        self.assertEqual(self.snapshot()["background"], 0)

    def test_stale_frame_does_not_get_a_new_timestamp(self):
        helper.publish(self.frame())
        path = self.root / f"{self.pid}-{self.ticks}.json"
        before = path.read_bytes()
        old = (datetime.now(timezone.utc) - timedelta(seconds=20)).isoformat()
        with self.assertRaises(ValueError):
            helper.publish(self.frame(observedAt=old))
        self.assertEqual(path.read_bytes(), before)

    def test_input_wait_pauses_foreground_but_not_owned_background(self):
        self.call(background=True)
        child = self.child()
        self.native.update(status="waiting", waitingFor="permission prompt")
        helper.publish(self.frame(main=True))
        result = self.snapshot()
        self.assertTrue(result["needsInput"])
        self.assertEqual((result["foreground"], result["background"]), (0, 1))
        child.terminate()
        child.wait(timeout=5)

    def test_unrelated_shell_invocations_do_not_register_work(self):
        self.assertIsNone(helper.native_command(["-c", "echo operator shell"]))
        self.assertIsNone(helper.native_command(["-l", "-c", "source snapshot.sh"]))
        self.assertIsNone(helper.native_command(["--version"]))
        helper.publish(self.frame())
        self.assertEqual(self.snapshot()["background"], 0)


class NativeShellBoundaryTest(unittest.TestCase):
    def test_exec_preserves_stdin_arguments_cwd_status_and_signal_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            wrapper = Path(temporary) / "observed-bash"
            wrapper.write_bytes(SOURCE.read_bytes())
            wrapper.chmod(0o755)
            result = subprocess.run([str(wrapper), "-c",
                'read -r value; printf "%s|%s|%s" "$value" "$1" "$PWD"; exit 7',
                "native", "argument with spaces"], input="input with spaces\n", text=True,
                cwd=temporary, capture_output=True)
            self.assertEqual(result.returncode, 7)
            self.assertEqual(result.stdout, f"input with spaces|argument with spaces|{temporary}")
            child = subprocess.Popen([str(wrapper), "-c", "exec sleep 30"])
            try:
                for _ in range(100):
                    if Path(f"/proc/{child.pid}/comm").read_text().strip() == "sleep":
                        break
                    time.sleep(0.01)
                else:
                    self.fail("native shell did not exec the command")
                child.send_signal(signal.SIGTERM)
                self.assertEqual(child.wait(timeout=5), -signal.SIGTERM)
            finally:
                if child.poll() is None:
                    child.kill()
                    child.wait()

    def test_qualified_owner_must_be_in_actual_process_ancestry(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config"
            (config / "sessions").mkdir(parents=True)
            pid = os.getpid()
            (config / "sessions" / f"{pid}.json").write_text(json.dumps({"pid": pid, "version": "2.1.330", "status": "idle"}))
            env = {**os.environ, "CLAUDE_CONFIG_DIR": str(config), "KYBER_CLAUDE_WORK_ROOT": str(root / "work")}
            frame = {"nativePID": str(pid), "observedAt": datetime.now(timezone.utc).isoformat(),
                     "main": False, "agents": [], "complete": True}
            result = subprocess.run([sys.executable, str(SOURCE), "publish"], input=json.dumps(frame),
                                    text=True, env=env, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            (config / "sessions" / f"{pid}.json").write_text(json.dumps({"pid": pid, "version": "2.1.119", "status": "idle"}))
            result = subprocess.run([sys.executable, str(SOURCE), "publish"], input=json.dumps(frame),
                                    text=True, env=env, capture_output=True)
            self.assertEqual(result.returncode, 1)


if __name__ == "__main__":
    unittest.main()
