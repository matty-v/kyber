# Agents and persistence

A Kyber agent is a long-lived worker with its own persistent filesystem, its own identity, and its own model. You declare what you want and Kyber keeps reality matching that intent, without you babysitting the machines underneath.

## Start every session with an instruction

An agent can have an optional startup prompt configured through the API or the
console. Kyber sends it as the first user turn whenever a new harness session
starts, including a pod restart, an explicit session reset, or recovery after
the harness exits. When session resume is also enabled, resumed sessions get
the prompt too. Kyber delivers it into the restored conversation as the trigger
to continue interrupted work, so an agent that dies mid-task picks the task
back up instead of idling. This works the same way for Claude Code and Codex.

Changing the prompt does not interrupt the current session. The console marks
the agent as requiring a restart; the new value takes effect on the next
session. Startup prompts are operator-visible configuration, not secrets.

## Whole-filesystem persistence

An agent keeps its entire filesystem across restarts, upgrades, and preemption: installed packages, cloned repos, credentials, and memory. Stopping an agent parks it with its filesystem preserved. Restarting it replaces the underlying pod while preserving its work. This is the defining difference from a throwaway container: the agent picks up where it left off instead of starting from a blank disk.

## Curate what an agent promises publicly

An operator can publish a versioned capability manifest for an agent from its
detail page. Each capability has a stable ID, business description, accepted
and emitted media types, and supported durable-task features. Publication is
explicit and empty by default: Kyber never turns a skill, prompt, tool schema,
model claim, or filesystem observation into a public promise on its own.

Private evidence requirements can bind a declaration to healthy skills,
connectors, platform features, and a compatible Claude Code or Codex adapter.
The controller reports availability and drift without exposing that evidence
through the public manifest. Missing, stale, broken, or mismatched evidence
fails closed. Authenticated clients with `capabilities:read` and permission for
the exact agent resource can cache the safe projection from
`GET /api/v1/agents/{name}/capabilities` using its ETag.

## A lifecycle you can read at a glance

An agent moves through named phases. Most transitions are automatic: Kyber drives the agent toward your declared intent (run it, stop it) and recovers it across machine interruptions. If the cheaper interruptible machine under an agent is reclaimed, Kyber drains the agent gracefully, parks it, and brings it back when a replacement machine is ready. You see the state change but do not have to act.

| Phase | What it means to you |
|---|---|
| `Creating` | The agent is being provisioned (storage, identity, pod). Wait; it proceeds on its own. |
| `Starting` | The agent's pod exists but is not ready yet. |
| `Running` | Up and healthy: the normal working state. |
| `Stopping` | Being gracefully shut down at your request. |
| `Stopped` | Not running; filesystem preserved. Stop is an authoritative kill switch: a stopped agent stays down, even if it was crash-looping, until you start it again. |
| `Restarting` | The pod is being replaced with the agent's work preserved. Usually operator-initiated, but Kyber also enters it on its own when the agent's runtime image is updated. |
| `Draining` | Being gracefully drained ahead of its machine being reclaimed. Kyber is protecting its work. |
| `WaitingForMachine` | Waiting for a replacement machine after preemption. It resumes when capacity returns. |
| `NeedsAuth` | Its stored authorization is no longer valid. Re-authorize it to bring it back. |
| `MemoryExhausted` | Killed for exceeding its memory limit. Give it more memory, then restart it. |
| `Failed` | An unrecoverable error, or automatic restart attempts used up. Investigate, then restart. |
| `Deleted` | Fully removed, including storage. An identity repo, if the agent has one, is preserved. |

Two of those states are deliberately human-required, because a silent retry would only hide the real problem: an agent whose stored authorization has expired (`NeedsAuth`), and an agent killed for running out of memory (`MemoryExhausted`). Kyber stops and waits for you instead of retrying into a loop.

Deletion is guarded the other way: it requires an explicit confirmation matching the agent's name, so a stray click or script cannot destroy an identity, because a confirmed delete is irreversible and removes the agent's storage. If the agent has an [identity repo](memory-and-identity.md), that repo is preserved.

## Safe upgrades

When the agent runtime image changes for a whole environment, Kyber rolls agents onto the new image in canary-gated waves. One agent rolls first; the rest keep running on the old image until the canary comes up healthy, and Kyber records an event explaining any hold. A bad image takes down one canary, not the fleet. A restart also re-syncs the agent's identity repo to its default branch, so changes merged while the agent was busy land on the restarted agent; the agent's own branches and uncommitted work are preserved.

Kyber also self-heals the helper containers that run alongside each agent. If a monitoring or transcript sidecar dies, it restarts automatically, so an agent never keeps working invisibly behind a frozen heartbeat.

Manage all of this from the [fleet console](fleet-console.md).

## Learn more

- [Memory and identity](memory-and-identity.md): the git-backed identity that survives even a full teardown.
- [How the lifecycle works](../../architecture/agent-lifecycle.md): the state machine behind these phases.
- [What an agent itself is told about the platform](../../agent-manual.md)
