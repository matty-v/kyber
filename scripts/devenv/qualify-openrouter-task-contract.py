#!/usr/bin/env python3
"""Check redacted evidence from a live Codex/OpenRouter task-contract run.

The caller records API task IDs, attempt IDs, state, and the model's tool-call
arguments. Distinct markers make a response copied from an earlier task fail.
This checker cannot prove which upstream provider OpenRouter selected.
"""

import json
import re
import sys
from pathlib import Path


ID = re.compile(r"^task_[a-f0-9]{32}$")
ATTEMPT = re.compile(r"^attempt_[a-f0-9]{32}$")
ACK = re.compile(r"^ack_[a-f0-9]{32}$")


def check(evidence):
    errors = []
    tasks = evidence.get("tasks", [])
    if len(tasks) != 3:
        return ["expected exactly two completion tasks and one cancellation task"]
    seen = set()
    markers = []
    for index, task in enumerate(tasks):
        label = f"task {index + 1}"
        task_id, attempt, marker = (task.get(key, "") for key in ("taskId", "attemptId", "marker"))
        if not isinstance(task_id, str) or not ID.fullmatch(task_id) or not isinstance(attempt, str) or not ATTEMPT.fullmatch(attempt):
            errors.append(f"{label}: invalid task or attempt ID")
        if (task_id, attempt) in seen:
            errors.append(f"{label}: repeated task/attempt pair")
        seen.add((task_id, attempt))
        if not isinstance(marker, str) or not marker or marker in markers:
            errors.append(f"{label}: marker must be unique and nonempty")
        prior_markers = markers.copy()
        markers.append(marker)
        receipt = task.get("receipt") or {}
        if receipt.get("taskId") != task_id or receipt.get("attemptId") != attempt:
            errors.append(f"{label}: receipt is missing or bound to a different task/attempt")
        if index < 2:
            completion = task.get("completion") or {}
            response = completion.get("response", "")
            if completion.get("taskId") != task_id or completion.get("attemptId") != attempt:
                errors.append(f"{label}: completion is missing or bound to a different task/attempt")
            if not isinstance(response, str) or marker not in response:
                errors.append(f"{label}: completion omitted its own marker")
            if isinstance(response, str) and any(previous in response for previous in prior_markers):
                errors.append(f"{label}: completion copied a previous task marker")
            if task.get("state") != "completed":
                errors.append(f"{label}: final state is not completed")
        else:
            ack = task.get("cancelAck") or {}
            if ack.get("taskId") != task_id or ack.get("attemptId") != attempt:
                errors.append(f"{label}: cancellation acknowledgment is missing or misbound")
            ack_id = ack.get("acknowledgmentId")
            if not isinstance(ack_id, str) or not ACK.fullmatch(ack_id):
                errors.append(f"{label}: cancellation acknowledgment ID has invalid schema")
            if task.get("completion"):
                errors.append(f"{label}: canceled task also emitted completion")
            if task.get("state") != "canceled" or task.get("ackSource") != "agent":
                errors.append(f"{label}: cancellation was not acknowledged by the agent")
    return errors


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: qualify-openrouter-task-contract.py <redacted-evidence.json>")
    evidence = json.loads(Path(sys.argv[1]).read_text())
    errors = check(evidence)
    if errors:
        print("FAIL\n" + "\n".join(errors))
        sys.exit(1)
    print("PASS: exact receipts, task-specific completions, and cancellation acknowledgment")


if __name__ == "__main__":
    main()
