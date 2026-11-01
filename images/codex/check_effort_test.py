import importlib.util
import json
import os
from pathlib import Path
import tempfile
import sys
sys.dont_write_bytecode=True
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('effort',Path(__file__).with_name('check-effort.py'))
probe=importlib.util.module_from_spec(spec);spec.loader.exec_module(probe)

class NativeEffortTests(unittest.TestCase):
    def run_probe(self,models,*,error=False,stall=False):
        with tempfile.TemporaryDirectory() as d:
            path=Path(d)/'codex'
            program='#!/usr/bin/env python3\nimport json,sys,time\n'
            if stall:program+='time.sleep(10)\n'
            else:
                program+='for line in sys.stdin:\n req=json.loads(line)\n'
                program+=' if req.get("id")==1: print(json.dumps({"id":1,"result":{}}),flush=True)\n'
                value={'id':2,'error':{'message':'private-upstream-detail'}} if error else {'id':2,'result':{'data':models}}
                program+=' if req.get("id")==2: print('+repr(json.dumps(value))+',flush=True)\n'
            path.write_text(program);path.chmod(0o700)
            with patch.dict(os.environ,{'PATH':d+':'+os.environ['PATH']}):
                return probe.check('gpt-6.1-terra','ultra',timeout=.2 if stall else 3)
    def test_exact_native_model_and_level(self):
        self.assertTrue(self.run_probe([{'model':'gpt-6.1-terra','supportedReasoningEfforts':[{'reasoningEffort':'ultra'}]}]))
    def test_missing_wrong_duplicate_model_or_level_never_pass(self):
        match={'model':'gpt-6.1-terra','supportedReasoningEfforts':[{'reasoningEffort':'ultra'}]}
        for models in [[],[{**match,'model':'another-model'}],[match,match],[{**match,'supportedReasoningEfforts':[{'reasoningEffort':'high'}]}]]:
            self.assertFalse(self.run_probe(models))
    def test_error_and_timeout_are_not_support(self):
        self.assertFalse(self.run_probe([],error=True));self.assertFalse(self.run_probe([],stall=True))
    def test_bad_effort_and_missing_model_do_not_start_a_probe(self):
        with patch('subprocess.Popen',side_effect=AssertionError('must not launch')):
            self.assertFalse(probe.check('','high'));self.assertFalse(probe.check('gpt-6.1-terra','$(id)'))

if __name__=='__main__':unittest.main()
