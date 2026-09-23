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

An agent keeps its entire filesystem on its persistent volume across pod restarts and upgrades: installed packages, cloned repos, credentials, and memory. Stopping an agent parks it with its filesystem preserved. Restarting it replaces the underlying pod while preserving its work. Recovery on a replacement machine depends on storage: portable cloud volumes can reattach, while node-local storage cannot survive loss of its node. Pushed identity-repo content survives either case.

## Switch the harness on the same agent

Agent Detail offers **Switch harness** for agents in Running, Stopped, Failed,
or NeedsAuth. The API equivalent is `POST /api/v1/agents/{name}/switch-runtime`
with `{"runtime":"codex"}` (or any other configured runtime). It requires
`lifecycle:write`. Kyber verifies the target image, current authentication
mode, and enabled channel compatibility before preparing the target on the
same persistent volume. A failed preparation leaves the original runtime
selected. The agent name, volume, identity checkout, skills, local files,
scheduled jobs, previous credential Secret, and old transcript trees remain.

The switch clears the old harness's model and version overrides, stops its
current pod, and enters NeedsAuth. Complete the target runtime's OAuth,
device-login, or API-key authorization flow, or use **Retry startup** if that runtime already has a valid credential
Secret from an earlier switch. Agents using a custom inference endpoint keep
its existing Secret and can retry startup directly; they do not need a provider
key for the new harness. The target starts a fresh session unless it
already has its own resumable transcript on disk. If scheduled jobs use
`exclusive` or `clearContextAfter` and the target lacks job turn hooks, those
flags are inert; the switch response and console call this out.

## Export an agent's disk

Agent Detail's **Disk export** card, or `POST /api/v1/agents/{name}/exports`,
starts an export of the agent's whole persistent disk: its durable root, home
directory, identity checkout, skills, local work and retained sessions. The
request returns at once with a job to poll. A running agent is stopped while
its disk is read, then returned to what it was doing before (Kyber keeps any
newer instruction you gave it in the meantime), and the export can be
canceled at any point. A failed or canceled export always releases the agent.

When the job completes, Kyber has checked every file in the archive against
its manifest. Download it as a ZIP from the card, or mint a link with
`POST /api/v1/agents/{name}/exports/{id}/download-link`; links expire after
ten minutes and completed archives after the installation's retention window
(72 hours by default).

The archive's `kyber-export/manifest.json` records the source agent and
runtime, the source volume, a non-secret copy of the agent's settings, every
file's path, type, size, ownership, permissions, timestamps and checksum, and
every mount the agent could see with whether it was archived and why.
Kubernetes Secrets, the agent's credential Secrets, platform-rendered
configuration and ephemeral mounts are never in the archive; the manifest and
the console list them as not included. Sockets and other special files are
listed as left out rather than dropped silently. An archive can still contain
local credential files from the disk itself, so exporting and downloading need
the dedicated `archives:admin` permission.

Archives are stored encrypted by the installation, in Kyber's own archive
store by default (on every installation target) or in the object storage the
operator configures. See [agent disk archives](../../operator/agent-disk-archives.md).

## Create an agent from a disk archive

Create Agent can start from a disk archive instead of an empty disk: a
completed export, or a Kyber disk archive ZIP uploaded from anywhere (which
is how an agent moves between installations). The API equivalent is
`POST /api/v1/agent-imports` with the archive and an ordinary create request;
upload a ZIP first with `POST /api/v1/archive-uploads`. Kyber rejects an
unsupported archive version, a disk too small for the archive, a name or
volume that already exists, and an archive that fails verification, all
before creating anything.

The new agent is created on the machine and storage you choose, with a fresh
volume, and does not start until its disk has been restored and every file
checked against the archive. Ownership, permissions, timestamps and links
are restored as they were. The source agent is never touched, and a failed or
canceled restore deletes the new agent and its volume.

Nothing that would make two agents act at once is copied or turned on. Kyber
lists these on the new agent's page as a cutover checklist: scheduled jobs,
chat channels, webhook bindings, crontabs the agent installed itself, and a
shared identity repo. Kubernetes Secrets are never copied. Files that look
like credentials (harness logins, SSH keys, Git, cloud and registry
credentials) and crontabs the agent installed itself are left out by default,
so the new agent authorizes itself and scheduled work does not run twice;
each can be restored on request.

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
| `DiskExhausted` | Disk reserve reached; the harness pauses while Shell remains available for cleanup. Free space or expand supported storage. |
| `BrokenRuntime` | The harness executable is missing or unusable. Use runtime repair before resuming work. |
| `Failed` | An unrecoverable error, or automatic restart attempts used up. Investigate, then restart. |
| `Deleted` | Fully removed, including storage. An identity repo, if the agent has one, is preserved. |

Recovery states identify the action needed: renew authorization for `NeedsAuth`, increase memory for `MemoryExhausted`, restore disk headroom for `DiskExhausted`, or repair the harness for `BrokenRuntime`. Repeated restarts alone do not resolve these conditions.

Deletion is guarded the other way: it requires an explicit confirmation matching the agent's name, so a stray click or script cannot destroy an identity, because a confirmed delete is irreversible and removes the agent's storage. If the agent has an [identity repo](memory-and-identity.md), that repo is preserved.

## Safe upgrades

When the agent runtime image changes for a whole environment, Kyber rolls agents onto the new image in canary-gated waves. One agent rolls first; the rest keep running on the old image until the canary comes up healthy, and Kyber records an event explaining any hold. A bad image takes down one canary, not the fleet. A restart also re-syncs the agent's identity repo to its default branch, so changes merged while the agent was busy land on the restarted agent; the agent's own branches and uncommitted work are preserved.

Kyber also self-heals the helper containers that run alongside each agent. If a monitoring or transcript sidecar dies, it restarts automatically, so an agent never keeps working invisibly behind a frozen heartbeat.

Manage all of this from the [fleet console](fleet-console.md).

## Learn more

- [Memory and identity](memory-and-identity.md): the git-backed identity that survives even a full teardown.
- [How the lifecycle works](../../architecture/agent-lifecycle.md): the state machine behind these phases.
- [What an agent itself is told about the platform](../../agent-manual.md)
