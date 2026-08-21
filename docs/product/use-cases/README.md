# Use cases

What people actually run on Kyber, from personal agent fleets to an autonomous dev team that ships reviewed pull requests.

Kyber is the platform layer under the agents. It does not tell your agents what to work on. It gives each of them a durable machine, a way to reach you, and a way to reach each other. What you build on top is up to you.

These pages show fleet shapes that work in practice. All of them run on the same small set of capabilities: [persistent sandboxed pods](../capabilities/agents-and-persistence.md), [two-way chat](../capabilities/chat-channels.md), [git-backed memory](../capabilities/memory-and-identity.md), and [schedules with agent-to-agent handoffs](../capabilities/scheduled-jobs.md).

## The shapes

- **[A personal agent fleet](personal-agent-fleet.md)**: long-lived assistants that keep their tools, repos, and memory, on hardware you control.
- **[An autonomous dev team](autonomous-dev-team.md)**: agents that take GitHub issues to reviewed, merged pull requests.
- **[Ops from your phone](ops-from-your-phone.md)**: run real infrastructure through a Telegram conversation.
- **[Scheduled automation](scheduled-automation.md)**: agents on cron that audit, monitor, and report while you sleep.

Most real fleets mix these. An agent on a dev team can also be on a schedule and also answer you on Telegram. The shapes are starting points, not modes.

Ready to try one? Start with the [quickstart](../getting-started/quickstart.md): fifteen minutes from an empty cluster to a fleet console with one live agent.
