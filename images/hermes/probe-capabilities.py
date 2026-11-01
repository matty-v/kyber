#!/usr/bin/env python3
import json
import os

try:
    with open(os.environ.get('KYBER_EFFORT_PROTOCOL_FILE', '/usr/local/share/kyber-reasoning-effort-protocol'), 'rb') as stream:
        supported = stream.read(3) == b'2\n'
except OSError:
    supported = False
print(json.dumps({'reasoning-effort': supported}))
