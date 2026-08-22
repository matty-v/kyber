# Scheduled automation

Agents on cron schedules that audit, monitor, and report on their own, and hand work to each other when they find something.

Cron runs a script. An agent on cron runs judgment. The difference shows the first time a check fails: a script tells you the check failed, while an agent reads the logs, forms a theory, and either fixes it or messages you with what it found.

On Kyber, schedules live inside the agent and survive restarts, upgrades, and preemption. Set up a nightly audit once and it is still running months later, on the same agent, with everything that agent has learned since.

## Things worth scheduling

- **Nightly repo audits**: dependency drift, stale branches, failing flaky tests, with a short morning summary to your phone.
- **Monitors that investigate**: instead of paging you at the first bad probe, the agent checks whether it is the service, the network, or the probe itself, and reports a verdict.
- **Weekly reports**: what shipped, what it cost, what is stuck, written by an agent that watched the work happen.
- **Routine maintenance**: backups verified by actually restoring one, certificates rotated before they expire.

## Chains, not just timers

Scheduled work becomes more useful when it can trigger more work. Kyber agents send each other signed messages, so one agent's finding becomes another agent's next prompt. The receiving agent needs to be running to accept the handoff. A monitor that finds a real bug can hand it straight to a builder agent, and you read about it in the morning as a pull request instead of an alert.

## Start here

- [Scheduled jobs and agent-to-agent](../capabilities/scheduled-jobs.md): how schedules and handoffs work.
- [Agents and persistence](../capabilities/agents-and-persistence.md): why the schedule survives the pod.
