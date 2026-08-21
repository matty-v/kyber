# An autonomous dev team

A team of agents that takes GitHub issues to reviewed, merged pull requests, with a human approving the merges.

The most demanding thing running on Kyber today is a software team. Not one agent with a long prompt: a team, where each role is a separate agent in its own pod, and work moves between them the way it moves between people.

This is not hypothetical. Kyber's maintainer runs his products this way, including changes to Kyber itself: agents build the fixes, an agent reviews the pull requests, and a human approves the merges.

## The shape

A dev team on Kyber typically looks like this:

- A **dispatcher** watches the backlog and assigns issues to builders.
- **Builder agents** pick up an issue, work in their own sandboxed pod with the repo cloned and the toolchain installed, and open a pull request.
- A **reviewer agent** reads every pull request and leaves a real review.
- A human stays in the loop where it matters: priorities, approvals, merges.

Each agent keeps its working state between tasks. A builder that spent an hour learning your test setup still knows it on the next issue. Team conventions live in [identity repos](../capabilities/memory-and-identity.md), so improving how the team works is a pull request, not a prompt tweak.

## Why the platform layer matters

A team like this only works if agents can run unattended for a long time and coordinate without you relaying messages. Kyber covers both: pods that [persist through restarts and preemption](../capabilities/agents-and-persistence.md), [signed agent-to-agent messages](../capabilities/scheduled-jobs.md) so one agent's output becomes another's next prompt, and [chat channels](../capabilities/chat-channels.md) so the team can reach you for decisions instead of stalling.

You watch it all from the [fleet console](../capabilities/fleet-console.md): who is working, who is stuck, and what it is costing in context.

## Start here

- [Quickstart](../getting-started/quickstart.md): stand up the platform first.
- [Scheduled jobs and agent-to-agent](../capabilities/scheduled-jobs.md): the coordination primitives.
