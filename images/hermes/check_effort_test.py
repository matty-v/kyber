import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('hermes_effort', Path(__file__).with_name('check-effort.py'))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


def parse(value):
    return {'enabled': False} if value == 'none' else {'enabled': True, 'effort': value}


class HermesEffortTests(unittest.TestCase):
    def test_provider_clamped_levels_are_not_advertised(self):
        caps = {'supports_reasoning': True, 'supported_efforts': ['low', 'high'], 'mandatory': False}
        clamp = lambda level, supported: level if level in supported else 'high'
        self.assertEqual(probe.effective_levels(caps, ['low', 'high', 'ultra'], parse, clamp), ['none', 'low', 'high'])

    def test_unknown_and_non_reasoning_models_offer_no_controls(self):
        for caps in [None, {}, {'supports_reasoning': False}]:
            self.assertEqual(probe.effective_levels(caps, ['high'], parse, lambda x, _: x), [])

    def test_mandatory_reasoning_never_offers_none(self):
        caps = {'supports_reasoning': True, 'supported_efforts': None, 'mandatory': True}
        self.assertEqual(probe.effective_levels(caps, ['high', 'ultra'], parse, lambda x, _: x), ['high', 'ultra'])

    def test_native_parser_must_preserve_the_selected_value(self):
        caps = {'supports_reasoning': True, 'supported_efforts': None}
        self.assertEqual(probe.effective_levels(caps, ['ultra'], lambda _: {'enabled': True, 'effort': 'max'}, lambda x, _: x), [])


if __name__ == '__main__':
    unittest.main()
