import importlib.util
from pathlib import Path
import unittest
import urllib.error
from unittest.mock import patch


MODULE_PATH = Path(__file__).resolve().parents[1] / "ori-eval-comparison.py"
SPEC = importlib.util.spec_from_file_location("ori_eval_comparison", MODULE_PATH)
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)


class ComparisonTests(unittest.TestCase):
    def test_uncapped_key_requires_explicit_waiver(self):
        with patch.object(module, "request_json", return_value={"limit": None, "usage": 0.1}):
            with self.assertRaises(module.EvalError):
                module.check_key("test-key", False)
            self.assertFalse(module.check_key("test-key", True))

    def test_generation_categories_are_explicit(self):
        candidate = {"model": "cohere/north-mini-code-20260617:free", "api_type": "completions"}
        ancillary = {"model": "typesafe/jev-1.13-20260917", "api_type": "decisions"}
        unknown = {"model": "another/paid-model", "api_type": "completions"}
        self.assertEqual(module.classify_generation(candidate, module.MODELS[0]), "candidate")
        self.assertEqual(module.classify_generation(ancillary, module.MODELS[0]), "ancillary")
        self.assertEqual(module.classify_generation(unknown, module.MODELS[0]), "unknown")

    def test_summary_separates_candidate_and_ancillary_cost(self):
        runs = []
        tests = []
        records = {}
        for index, model in enumerate(module.MODELS):
            for case in range(2):
                candidate_id, ancillary_id = f"gen-{index}-{case}", f"gen-dec-{index}-{case}"
                records[candidate_id] = {"model": model.split(":")[0] + "-20260617:free", "api_type": "completions", "total_cost": 0}
                records[ancillary_id] = {"model": "typesafe/jev-1.13-20260917", "api_type": "decisions", "total_cost": 0.000061404}
                runs.append({"model": model, "outcome": "passed", "durationMs": 1000,
                             "terminal": {"payload": {"usage": {"costUsd": 0.000061404,
                                                              "generationIds": [candidate_id, ancillary_id]}}}})
                tests.append({"name": f"case {case} on {model}", "status": "pass"})
        data = {"scope": {"models": list(module.MODELS), "runs": 4}, "results": runs, "tests": tests}
        with patch.object(module, "request_json", side_effect=lambda url, key, retries=0: records[url.split("id=")[1]]):
            summary = module.summarize(data, "test-key", "full")
        self.assertEqual(summary["problems"], [])
        self.assertEqual(summary["models"][module.MODELS[0]]["verifiedCandidateCostUsd"], 0)
        self.assertEqual(summary["models"][module.MODELS[0]]["verifiedAncillaryCostUsd"], 0.000122808)
        self.assertTrue(summary["models"][module.MODELS[0]]["generationMetadataComplete"])
        self.assertEqual(summary["models"][module.MODELS[1]]["gradedPasses"], 2)

    def test_rejects_unexpected_candidate_count(self):
        with self.assertRaises(module.EvalError):
            module.verify_scope({"scope": {"models": [module.MODELS[0]], "runs": 2},
                                 "results": [{}, {}], "tests": [{}, {}]}, 2)

    def test_missing_generation_metadata_is_visible_without_zero_cost_claim(self):
        results = [{"model": model, "outcome": "passed", "durationMs": 1000,
                    "terminal": {"payload": {"usage": {"costUsd": 0.000061404,
                                                     "generationIds": [f"gen-{index}"]}}}}
                   for index, model in enumerate(module.MODELS)]
        data = {"scope": {"models": list(module.MODELS), "runs": 2}, "results": results,
                "tests": [{"name": model, "status": "pass"} for model in module.MODELS]}
        missing = urllib.error.HTTPError("https://example.invalid", 404, "missing", {}, None)
        with patch.object(module, "request_json", side_effect=missing):
            summary = module.summarize(data, "test-key", "pilot")
        missing.close()
        self.assertEqual(summary["problems"], [])
        self.assertTrue(summary["warnings"])
        self.assertFalse(summary["models"][module.MODELS[0]]["generationMetadataComplete"])
        self.assertEqual(summary["models"][module.MODELS[0]]["reportedCostUsd"], 0.000061404)


if __name__ == "__main__":
    unittest.main()
