# Agents and persistence

A Kyber agent is a long-lived worker with its own persistent filesystem, its own identity, and its own model. You declare what you want and Kyber keeps reality matching that intent, without you babysitting the machines underneath.

## Whole-filesystem persistence

An agent keeps its entire filesystem across restarts, upgrades, and preemption: installed packages, cloned repos, credentials, and memory. Stopping an agent parks it with its filesystem preserved. Restarting it replaces the underlying pod while preserving its work. This is the defining difference from a throwaway container: the agent picks up where it left off instead of starting from a blank disk.

## A lifecycle you can read at a glance

An agent moves through named phases. Most transitions are automatic: Kyber drives the agent toward your declared intent (run it, stop it, suspend it) and recovers it across machine interruptions. If the cheaper interruptible machine under an agent is reclaimed, Kyber drains the agent gracefully, parks it, and brings it back when a replacement machine is ready. You see the state change but do not have to act.

| Phase | What it means to you |
|---|---|
| `Creating` | The agent is being provisioned (storage, identity, pod). Wait; it proceeds on its own. |
| `Starting` | The agent's pod exists but is not ready yet. |
| `Running` | Up and healthy: the normal working state. |
| `Stopping` | Being gracefully shut down at your request. |
| `Stopped` | Not running; filesystem preserved. Stop is an authoritative kill switch: a stopped agent stays down, even if it was crash-looping, until you start it again. |
| `Restarting` | The pod is being replaced with the agent's work preserved. Usually operator-initiated, but Kyber also enters it on its own when the agent's runtime image is updated. |
| `Suspended` | Scaled to zero with the filesystem preserved, either idle or parked after a machine interruption. Start it to wake it; an inbound message does not. |
| `Draining` | Being gracefully drained ahead of its machine being reclaimed. Kyber is protecting its work. |
| `WaitingForMachine` | Waiting for a replacement machine after preemption. It resumes when capacity returns. |
| `NeedsAuth` | Its stored authorization is no longer valid. Re-authorize it to bring it back. |
| `MemoryExhausted` | Killed for exceeding its memory limit. Give it more memory, then restart it. |
| `Failed` | An unrecoverable error, or automatic restart attempts used up. Investigate, then restart. |
| `Deleted` | Fully removed, including storage and identity. |

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
