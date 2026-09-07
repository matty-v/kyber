#!/usr/bin/env python3
"""Deterministic test-only interactive harness. Never calls an LLM provider."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import tty
import urllib.request

root = Path(os.environ['FIXTURE_HOME'])
if os.environ.get('FIXTURE_KEY') != 'fixture-only-key':
    sys.exit(42)
root.mkdir(parents=True, exist_ok=True)

def shutdown(_signal, _frame):
    (root / 'shutdown').write_text('flushed')
    sys.exit(0)

signal.signal(signal.SIGHUP, shutdown)
signal.signal(signal.SIGTERM, shutdown)
tty.setraw(sys.stdin.fileno())
sys.stdout.write('\x1b[?2004h')
sys.stdout.flush()
(root / 'ready').touch()
while True:
    pending = b''
    while not pending.endswith(b'\x1b[201~\r'):
        chunk = os.read(sys.stdin.fileno(), 1)
        if not chunk:
            shutdown(0, None)
        pending += chunk
    start = pending.index(b'\x1b[200~') + len(b'\x1b[200~')
    prompt = pending[start:-len(b'\x1b[201~\r')].decode().replace('\r\n', '\n').replace('\r', '\n')
    payload = {'prompt': prompt, 'hook_event_name': 'UserPromptSubmit', 'session_id': 'fixture-session'}
    receipt = subprocess.run(['bash', os.environ['FIXTURE_RECEIPT_HOOK'], 'contract-process-fixture'], input=json.dumps(payload).encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=25)
    if receipt.returncode:
        (root / 'blocked').write_text(str(receipt.returncode))
        continue
    (root / 'transcript.jsonl').write_text(json.dumps({'role': 'user', 'content': prompt}) + '\n')
    (root / 'receipt-ready').touch()
    deadline = time.monotonic() + 20
    while not (root / 'complete-now').exists():
        if time.monotonic() > deadline:
            sys.exit(3)
        time.sleep(0.02)
    header = prompt.split('\n', 1)[0]
    task = header.split(':', 1)[1].split(']', 1)[0]
    attempt = header.split('attempt=', 1)[1]
    body = json.dumps({'task_id': task, 'attempt_id': attempt, 'response': 'fixture-complete'}).encode()
    req = urllib.request.Request(os.environ['FIXTURE_COMPLETE_URL'], data=body, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=5) as response:
        if response.status != 204:
            sys.exit(4)
    (root / 'completed').touch()
