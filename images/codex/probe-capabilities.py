#!/usr/bin/env python3
"""Codex owns interpretation of its native configuration; emit booleans only."""
import json, os, pathlib, shlex, tomllib

def load(path):
    try:
        with open(path, 'rb') as stream: return tomllib.load(stream)
    except (OSError, ValueError): return {}
managed=load(os.environ.get('KYBER_MANAGED_CODEX_CONFIG','/etc/codex/managed_config.toml'))
user=load(pathlib.Path(os.environ.get('CODEX_HOME',str(pathlib.Path.home()/'.codex')))/'config.toml')
hooks=managed.get('hooks',{})
def has(event, command, arguments=()):
    if not os.access(command, os.X_OK) or not isinstance(hooks, dict): return False
    groups = hooks.get(event, [])
    if not isinstance(groups, list): return False
    for group in groups:
        if not isinstance(group, dict): continue
        entries = group.get('hooks', [])
        if not isinstance(entries, list): continue
        for entry in entries:
            if not isinstance(entry, dict) or entry.get('type') != 'command': continue
            try: args = shlex.split(entry.get('command', ''))
            except (ValueError, TypeError): continue
            if args and args[0] == 'env':
                args = args[1:]
                while args and '=' in args[0]: args = args[1:]
            if args == [command, *arguments]: return True
    return False

start=os.environ.get('KYBER_CRON_TURNSTART_CMD','/usr/local/bin/kyber-cron-turn-start')
stop=os.environ.get('KYBER_CRON_POSTRUN_CMD','/usr/local/bin/kyber-cron-postrun')
receipt=os.environ.get('KYBER_TASK_RECEIPT_CMD','/usr/local/bin/kyber-task-receipt')
server=user.get('mcp_servers',{}).get('kyber_request_reply',{})
print(json.dumps({'job-turn-hooks':has('UserPromptSubmit',start) and has('Stop',stop) and pathlib.Path(os.environ.get('KYBER_CRON_POSTRUN_SENTINEL','/persist/var/run/kyber-cron-postrun-enabled')).exists(), 'task-receipts':has('UserPromptSubmit',receipt, ('codex',)), 'task-tools':bool(os.environ.get('KYBER_REQUEST_MCP_URL')) and server.get('url')==os.environ.get('KYBER_REQUEST_MCP_URL') and server.get('enabled',True) is True}))
