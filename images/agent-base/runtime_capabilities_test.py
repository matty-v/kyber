"""Exercise real native-config probes without provider credentials or CLIs."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class NativeCapabilityContract(unittest.TestCase):
    def test_probe_evidence(self):
        for runtime in ('codex', 'claude-code'):
            with self.subTest(runtime=runtime), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                for name in ('start', 'stop', 'receipt'):
                    (root / name).write_text('#!/bin/sh\nexit 0\n')
                    (root / name).chmod(0o755)
                sentinel = root / 'enabled'
                sentinel.touch()
                env = dict(os.environ, HOME=str(root), CODEX_HOME=str(root / '.codex'),
                           KYBER_MANAGED_CODEX_CONFIG=str(root / 'managed.toml'),
                           KYBER_CRON_TURNSTART_CMD=str(root / 'start'),
                           KYBER_CRON_POSTRUN_CMD=str(root / 'stop'),
                           KYBER_TASK_RECEIPT_CMD=str(root / 'receipt'),
                           KYBER_CRON_POSTRUN_SENTINEL=str(sentinel),
                           KYBER_REQUEST_MCP_URL='http://127.0.0.1:8091/mcp')
                def probe():
                    script = Path(__file__).resolve().parent.parent / runtime / 'probe-capabilities.py'
                    return json.loads(subprocess.check_output(['python3', str(script)], env=env, timeout=5))
                self.assertEqual(probe(), {'job-turn-hooks': False, 'task-receipts': False, 'task-tools': False})
                hooks = {'UserPromptSubmit': [{'hooks': [
                    {'type': 'command', 'command': str(root / 'start')},
                    {'type': 'command', 'command': str(root / 'receipt') + ' ' + runtime}]}],
                    'Stop': [{'hooks': [{'type': 'command', 'command': 'env KYBER_CLEAR_SESSION_TEXT=/clear ' + str(root / 'stop')}]}]}
                def write_hooks():
                    if runtime == 'codex':
                        text = ''
                        for event, groups in hooks.items():
                            for group in groups:
                                text += f'[[hooks.{event}]]\n'
                                for entry in group['hooks']:
                                    text += f'[[hooks.{event}.hooks]]\ntype = "command"\ncommand = {json.dumps(entry["command"])}\n'
                        (root / 'managed.toml').write_text(text)
                    else:
                        (root / '.claude').mkdir(exist_ok=True)
                        (root / '.claude/settings.json').write_text(json.dumps({'hooks': hooks}))
                write_hooks()
                if runtime == 'codex':
                    (root / '.codex').mkdir()
                    config = root / '.codex/config.toml'
                    config.write_text('[mcp_servers.kyber_request_reply]\nurl="http://127.0.0.1:8091/mcp"\n')
                else:
                    config = root / '.claude.json'
                    config.write_text(json.dumps({'mcpServers': {'kyber-request-reply': {'type': 'http', 'url': env['KYBER_REQUEST_MCP_URL']}}}))
                self.assertEqual(probe(), {'job-turn-hooks': True, 'task-receipts': True, 'task-tools': True})
                sentinel.unlink()
                self.assertFalse(probe()['job-turn-hooks'])
                self.assertTrue(probe()['task-receipts'])
                sentinel.touch()
                hooks['UserPromptSubmit'][0]['hooks'][0]['command'] = 'echo ' + str(root / 'start')
                write_hooks()
                self.assertFalse(probe()['job-turn-hooks'], 'a mention of a command is not wiring')
                (root / 'receipt').unlink()
                self.assertFalse(probe()['task-receipts'])
                config.write_text('malformed config')
                self.assertFalse(probe()['task-tools'])


if __name__ == '__main__':
    unittest.main()
