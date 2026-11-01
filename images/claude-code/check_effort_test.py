import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('claude_effort', Path(__file__).with_name('check-effort.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class NativeClaudeEffortTests(unittest.TestCase):
    def test_native_release_versions_are_bounded_and_include_prereleases(self):
        for version in ['2.1.316', '2.2.0-beta.1', '2.2.0-beta.1+build.5']:
            self.assertIsNotNone(probe.VERSION.fullmatch(version))
        for version in ['latest', '2.2.0-', '2.2.0;id', '2.2.0 beta']:
            self.assertIsNone(probe.VERSION.fullmatch(version))

    def test_resolved_model_and_alias_share_native_levels(self):
        rows = probe.normalize_models([{'value': 'opus', 'resolvedModel': 'claude-opus-5-5', 'supportsEffort': True,
                                       'supportedEffortLevels': ['low', 'xhigh', 'max'], 'account': 'must-not-leave'}], '2.1.316')
        self.assertEqual({r['id'] for r in rows}, {'opus', 'opus[1m]', 'claude-opus-5-5', 'claude-opus-5-5[1m]'})
        self.assertTrue(all(r['reasoningEffort']['levels'] == ['low', 'xhigh', 'max'] for r in rows))
        self.assertNotIn('account', json.dumps(rows))

    def test_old_identity_bridge_is_version_specific_and_never_invents_levels(self):
        model = {'value': 'opus[1m]', 'supportsEffort': True, 'supportedEffortLevels': ['high']}
        old = probe.normalize_models([model], '2.1.119')
        self.assertIn('claude-opus-4-7', {r['id'] for r in old})
        self.assertIn('opus', {r['id'] for r in old})
        new = probe.normalize_models([model], '2.1.120')
        self.assertNotIn('claude-opus-4-7', {r['id'] for r in new})
        self.assertEqual(probe.normalize_models([{'value': 'opus', 'supportsEffort': True}], '2.1.316'), [])

    def test_ambiguous_and_unsafe_levels_fail(self):
        row = {'value': 'opus', 'resolvedModel': 'claude-opus-5-5', 'supportsEffort': True, 'supportedEffortLevels': ['high']}
        for models in [[row, {**row, 'supportedEffortLevels': ['low']}], [{**row, 'supportedEffortLevels': ['$(id)']}],
                       [{**row, 'supportedEffortLevels': ['high', 'high']}]]:
            with self.assertRaises(ValueError):
                probe.normalize_models(models, '2.1.316')

    def test_native_initialize_never_sends_a_prompt_and_strips_private_fields(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'claude'
            response = {'type': 'control_response', 'response': {'subtype': 'success', 'request_id': 'kyber-native-effort',
                        'response': {'account': 'private', 'models': [{'value': 'sonnet', 'resolvedModel': 'claude-sonnet-5-5',
                        'supportsEffort': True, 'supportedEffortLevels': ['high']}]}}}
            path.write_text('#!/usr/bin/env python3\nimport json,sys\n'
                            'if "--version" in sys.argv: print("2.1.316 (Claude Code)");sys.exit(0)\n'
                            'r=json.loads(sys.stdin.readline())\nassert r["type"]=="control_request"\n'
                            'assert r["request"]["subtype"]=="initialize"\n'
                            'assert "--strict-mcp-config" in sys.argv\n'
                            'print(' + repr(json.dumps(response)) + ',flush=True)\n'
                            'import time;time.sleep(5)\n')
            path.chmod(0o700)
            with patch.dict(os.environ, {'PATH': directory + ':' + os.environ['PATH']}):
                result = probe.catalog(timeout=2)
            self.assertEqual(result['version'], '2.1.316')
            self.assertNotIn('private', json.dumps(result))


if __name__ == '__main__':
    unittest.main()
