#!/opt/hermes/.venv/bin/python
"""Use Hermes' provider metadata and parser without constructing an agent."""
import json
import os
import re
import subprocess
import sys

sys.path.insert(0, '/opt/hermes')


def effective_levels(caps, ladder, parse, clamp):
    if not isinstance(caps, dict) or caps.get('supports_reasoning') is not True:
        return []
    supported = caps.get('supported_efforts')
    if supported is not None and not isinstance(supported, list):
        return []
    levels = []
    for level in ladder:
        if not isinstance(level, str) or not re.fullmatch(r'[a-z][a-z0-9_-]{0,31}', level):
            continue
        parsed = parse(level)
        if parsed == {'enabled': True, 'effort': level} and clamp(level, supported) == level:
            levels.append(level)
    # 'none' is a real native value, distinct from removing Kyber's override.
    if caps.get('mandatory') is False and parse('none') == {'enabled': False}:
        levels.insert(0, 'none')
    return levels


def parse_version(output):
    match = re.search(r'^Hermes Agent v([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)', output, re.MULTILINE)
    if not match:
        raise ValueError('missing Hermes release version')
    return match.group(1)


def catalog():
    if os.environ.get('HERMES_PROVIDER') != 'openrouter' or os.environ.get('KYBER_INFERENCE_BASE_URL'):
        raise ValueError('route not qualified')
    from agent.reasoning_effort import EFFORT_LADDER, clamp_effort
    from hermes_constants import parse_reasoning_effort
    from hermes_cli.models import openrouter_model_reasoning_capabilities
    model = os.environ.get('HERMES_INFERENCE_MODEL', '')
    if not model or len(model) > 128:
        raise ValueError('missing model')
    result = subprocess.run(['hermes', '--version'], capture_output=True, timeout=5, check=True)
    version = parse_version(result.stdout.decode())
    caps = openrouter_model_reasoning_capabilities(model, allow_fetch=True, timeout=6)
    if caps is None:
        raise ValueError('model metadata unavailable')
    levels = effective_levels(caps, EFFORT_LADDER, parse_reasoning_effort, clamp_effort)
    return {'version': version, 'models': [{'id': model, 'reasoningEffort': {
        'source': 'hermes-native-catalog', 'harnessVersion': version, 'levels': levels}}]}


def main():
    try:
        data = catalog()
        if sys.argv[1:] == ['--catalog']:
            print(json.dumps(data))
            return 0
        if len(sys.argv) == 3 and sys.argv[1] == data['models'][0]['id'] and sys.argv[2] in data['models'][0]['reasoningEffort']['levels']:
            return 0
    except Exception:
        pass
    print('[kyber] Native model effort could not be verified; refresh model information or reset effort to Default', file=sys.stderr)
    return 43


if __name__ == '__main__':
    raise SystemExit(main())
