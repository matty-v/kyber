import importlib.util
import unittest
from pathlib import Path


spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).with_name("qualify-openrouter-task-contract.py")
)
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)


def fixture():
    tasks = []
    for n in (1, 2, 3):
        task_id = "task_" + str(n) * 32
        attempt_id = "attempt_" + str(n) * 32
        item = {
            "taskId": task_id,
            "attemptId": attempt_id,
            "marker": f"UNIQUE-RESPONSE-{n}",
            "receipt": {"taskId": task_id, "attemptId": attempt_id},
        }
        if n < 3:
            item.update(state="completed", completion={
                "taskId": task_id, "attemptId": attempt_id,
                "response": f"UNIQUE-RESPONSE-{n}",
            })
        else:
            item.update(state="canceled", ackSource="agent", cancelAck={
                "taskId": task_id, "attemptId": attempt_id,
                "acknowledgmentId": "ack_" + "a" * 32,
            })
        tasks.append(item)
    return {"tasks": tasks}


class QualificationEvidenceTest(unittest.TestCase):
    def test_exact_contract_passes(self):
        self.assertEqual(qualification.check(fixture()), [])

    def test_wrong_attempt_or_task_fails(self):
        evidence = fixture()
        evidence["tasks"][0]["receipt"]["attemptId"] = evidence["tasks"][1]["attemptId"]
        evidence["tasks"][1]["completion"]["taskId"] = evidence["tasks"][0]["taskId"]
        errors = qualification.check(evidence)
        self.assertTrue(any("receipt" in error for error in errors))
        self.assertTrue(any("completion" in error for error in errors))

    def test_copied_response_fails(self):
        evidence = fixture()
        evidence["tasks"][1]["completion"]["response"] = "UNIQUE-RESPONSE-1"
        errors = qualification.check(evidence)
        self.assertTrue(any("omitted its own marker" in error for error in errors))
        self.assertTrue(any("copied a previous task" in error for error in errors))

    def test_malformed_or_stale_cancel_ack_fails(self):
        evidence = fixture()
        evidence["tasks"][2]["cancelAck"]["acknowledgmentId"] = "ack_cancel_20261011"
        evidence["tasks"][2]["cancelAck"]["taskId"] = evidence["tasks"][0]["taskId"]
        evidence["tasks"][2]["completion"] = evidence["tasks"][0]["completion"]
        errors = qualification.check(evidence)
        self.assertTrue(any("acknowledgment ID" in error for error in errors))
        self.assertTrue(any("misbound" in error for error in errors))
        self.assertTrue(any("also emitted completion" in error for error in errors))

    def test_malformed_evidence_is_reported_without_crashing(self):
        evidence = fixture()
        evidence["tasks"][0]["marker"] = None
        evidence["tasks"][1]["completion"]["response"] = 42
        self.assertTrue(qualification.check(evidence))


if __name__ == "__main__":
    unittest.main()
