#!/usr/bin/env python3
"""Claude Code owns interpretation of its native configuration; emit booleans only."""
import json, os, pathlib, shlex

def load(path):
    try: return json.loads(path.read_text())
    except (OSError, ValueError): return {}
home=pathlib.Path.home()
hooks=load(home/'.claude/settings.json').get('hooks',{})
def has(event, command):
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
            if args and args[0] == command: return True
    return False

start=os.environ.get('KYBER_CRON_TURNSTART_CMD','/usr/local/bin/kyber-cron-turn-start')
stop=os.environ.get('KYBER_CRON_POSTRUN_CMD','/usr/local/bin/kyber-cron-postrun')
receipt=os.environ.get('KYBER_TASK_RECEIPT_CMD','/usr/local/bin/kyber-task-receipt')
server=load(home/'.claude.json').get('mcpServers',{}).get('kyber-request-reply',{})
print(json.dumps({'job-turn-hooks':has('UserPromptSubmit',start) and has('Stop',stop) and pathlib.Path(os.environ.get('KYBER_CRON_POSTRUN_SENTINEL','/persist/var/run/kyber-cron-postrun-enabled')).exists(), 'task-receipts':has('UserPromptSubmit',receipt), 'task-tools':bool(os.environ.get('KYBER_REQUEST_MCP_URL')) and server.get('url')==os.environ.get('KYBER_REQUEST_MCP_URL')}))
