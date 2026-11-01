#!/usr/bin/env python3
"""Verify the requested model/effort using the installed native client; no inference.

Only a fixed reason leaves this probe. Never emit native stdout/stderr, account
metadata or model descriptions into startup logs.
"""
import json
import os
import re
import selectors
import subprocess
import sys
import time


def check(model, effort, *, timeout=20):
    if not model or len(model)>128 or not re.fullmatch(r'[a-z][a-z0-9_-]{0,31}',effort):
        return False
    process=subprocess.Popen(['codex','app-server','--listen','stdio://'],stdin=subprocess.PIPE,
                             stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,start_new_session=True)
    selector=selectors.DefaultSelector();selector.register(process.stdout,selectors.EVENT_READ)
    deadline=time.monotonic()+timeout;buffer=b'';total=0
    def send(value):
        process.stdin.write(json.dumps(value).encode()+b'\n');process.stdin.flush()
    try:
        send({'method':'initialize','id':1,'params':{'clientInfo':{'name':'kyber','title':'Kyber','version':'1'}}})
        initialized=False
        while time.monotonic()<deadline:
            if not selector.select(max(0,deadline-time.monotonic())):return False
            chunk=os.read(process.stdout.fileno(),65536)
            if not chunk:return False
            total+=len(chunk);buffer+=chunk
            if total>2*1024*1024:return False
            while b'\n' in buffer:
                line,buffer=buffer.split(b'\n',1);data=json.loads(line)
                if data.get('error'):return False
                if data.get('id')==1 and not initialized:
                    initialized=True
                    send({'method':'initialized','params':{}})
                    send({'method':'model/list','id':2,'params':{'limit':100,'includeHidden':False}})
                elif data.get('id')==2:
                    matches=[m for m in data.get('result',{}).get('data',[]) if m.get('model')==model]
                    if len(matches)!=1:return False
                    return any(x.get('reasoningEffort')==effort for x in matches[0].get('supportedReasoningEfforts',[]))
        return False
    except (OSError,ValueError,TypeError,AttributeError):return False
    finally:
        selector.close()
        try:os.killpg(process.pid,9)
        except ProcessLookupError:pass
        process.wait();process.stdin.close();process.stdout.close()


def main():
    try:valid=len(sys.argv)==3 and check(sys.argv[1],sys.argv[2])
    except Exception:valid=False
    if not valid:print('[kyber] Native model effort could not be verified; refresh model information or reset effort to Default',file=sys.stderr)
    return 0 if valid else 43

if __name__=='__main__':raise SystemExit(main())
