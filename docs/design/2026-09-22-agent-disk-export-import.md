# MAT-87 / MAT-88: export an agent's disk and create an agent from it

MAT-87 exports an agent's persistent volume as a portable ZIP. MAT-88
creates a new agent from that ZIP on any machine and fresh PVC. Together they
are Kyber's migration path across machines and storage. Both must work on
every supported installation target: GKE, EKS, WSL2 and bare Linux (k3s).

## Where the bytes live

Object storage is optional and off by default, so it cannot be the only
home for archives. The chart ships a **Kyber archive store**: a one-replica
Deployment (`Recreate`, control-plane image, `kyber-archive-store` binary)
with its own PVC on the installation's default StorageClass. It exposes a
private, token-authenticated HTTP object API (put / ranged get / delete) on
the cluster network and holds nothing but archives. When
`agentArchives.storage.backend` is `s3` or `gcs`, the control plane streams to
that bucket instead and the store is not rendered.

The source PVC is never used as scratch. The export pod mounts it read-only
and streams the ZIP to the control plane; nothing is staged on the disk being
exported, so a nearly full disk exports the same as an empty one.

Stored archives are sealed with AES-256-GCM in 64 KiB chunks (per-job key,
wrapped with a key derived from the internal signing key and stored in the
job row). A leaked bucket or archive-store volume alone discloses nothing.
Ranged reads stay possible because each chunk is independently authenticated.
Downloads are unsealed on the fly and are a plain ZIP.

## Job model

Jobs are rows in Postgres (`agent_archive_jobs`), driven by a leader-gated
worker in the control plane. Every step is idempotent against the persisted
state, so a control-plane restart resumes or fails a job cleanly. No
Postgres means the feature reports itself unavailable in `/api/v1/config`.

Export states: `queued`, then `running` through the steps `pausing → archiving → verifying`, then `completed`, or
`failed`, `canceled`, `expired`. A running export holds the agent with the
`kyber.io/archive-hold` annotation: the reconciler will not create an agent pod
while it is present, whatever `desiredPhase` says.

1. **pausing** – record the agent's standing intent (its `desiredPhase`, or,
   when that is empty as the controller leaves it after a restart, what its
   phase implies); unless that intent is Stopped, set `desiredPhase: Stopped`
   and the `kyber.io/archive-paused` mark, and wait (bounded by
   `pauseTimeout`) for the pod to be gone. The PVC is then quiescent.
2. **archiving** – create a per-job token Secret and a same-node export pod
   (no service-account token, PVC read-only). It walks `/persist` through
   `os.Root`, streams the ZIP to `PUT /internal/archive-jobs/{id}/upload`, and
   the control plane seals it into the store while counting bytes against
   `maxArchiveBytes`. A file that changes or cannot be read fails the export.
3. **release** – as soon as the pod has succeeded the hold is removed and the
   prior intent restored, but only while the `kyber.io/archive-paused` mark
   is still there: every lifecycle verb, Stop included, clears it, so an
   operator's choice during the export wins. The pause is bounded by `jobTimeout`, which includes
   `pauseTimeout`; on any failure or cancellation the agent is released the
   same way.
4. **verifying** – an unprivileged pod with no volumes reads the stored
   archive back through `GET /internal/archive-jobs/{id}/archive` (ranged
   plaintext) and runs `diskarchive.Verify`: every ZIP entry has exactly one
   manifest record and vice versa, types, sizes and SHA-256 agree, paths are
   canonical, no duplicate names. It posts the manifest summary to
   `/summary`; only then is the job `completed` with an `expiresAt`. The
   check runs in a pod because a disk with millions of files has a manifest
   and ZIP directory the control plane must never hold in memory.

Each job advances in its own goroutine against a fresh read of its row. A
job ends in two saved steps: it first records the outcome it is heading for
(`finishing`, version-checked), then releases the agent, removes pods and
deletes bytes, then saves the terminal state; a restart in between re-runs
the idempotent cleanup. An archive pod that cannot start (image pull
failure, container config error, or still pending after 10 minutes) fails
the job at once instead of holding the agent until its deadline.

Cancellation deletes the pod, token Secret and partial object and releases
the agent. Retention GC deletes expired objects. Each job owns its object key,
so a retry can never overwrite an earlier successful export. One active export
per agent and `maxConcurrentJobs` installation-wide.

## Archive format (v1)

`persist/…` holds one entry per object under the PVC root, parents before
children. `kyber-export/manifest.json` is written last and records format
version, source agent/runtime identity, export time, source disk (PVC,
StorageClass, capacity, access modes), a non-secret config allowlist, the
pod's mount inventory (each mount archived or not, with a reason), explicit
exclusions under the root, and per entry: path, type, size, mode, uid, gid,
mtime, symlink target and SHA-256 for regular files.

Excluded by design: sockets, FIFOs and device nodes (listed with a reason),
names that cannot be carried safely (not UTF-8, or that read as traversal
when a backslash is taken as a separator; listed with a reason),
`lost+found`, Kubernetes Secrets and ConfigMaps, the transcript-offsets PVC,
and image-provided paths outside `/persist`. Hard links are archived as
independent files. Symlinks are archived as links, never followed.

## Authorization and download

A new `archives:admin` scope (not implied by `lifecycle:admin`) gates start,
cancel, download and import. Download is a two-step: an authorized
`POST /api/v1/agents/{name}/exports/{id}/download-link` returns a URL carrying an HMAC token
bound to the job ID and a 10-minute expiry (the job ID identifies the agent). The token is in the path's last
segment, which request logging redacts. The token never grants access to
another job.

## Create from export (MAT-88)

`POST /api/v1/agent-imports` with either a completed export's job ID or an
uploaded ZIP (streamed to the store and verified before anything is created;
same retention). The response shows the manifest summary. `POST
/api/v1/agent-imports/{id}/create` takes the new name, machine, runtime,
model and disk size; it rejects unsupported format versions, a disk smaller
than the archived bytes, duplicate names and existing PVCs before creating
anything.

It then creates the Agent with the `kyber.io/archive-hold` hold and a
pre-created PVC, runs a same-node restore pod that extracts through `os.Root`
(no absolute paths, traversal, special files, duplicate paths, entries
outside the manifest, or symlink-parented writes; symlinks created last),
re-scans the tree and compares it to the manifest, and only then removes the
hold. The new agent starts `NeedsAuth` unless its credentials were supplied
in the create request. Kubernetes Secrets, channel bindings and operator
tokens are never copied. Scheduled jobs and channel bindings from the source
config are shown as a cutover checklist and are not activated. A failed or
canceled restore deletes the new Agent and PVC; the source is never touched.

Credential files on the disk (for example `~/.claude/.credentials.json`,
`~/.codex/auth.json`) are flagged in the job summary as sensitive. Whether a
restore keeps or scrubs them, and how that interacts with the new agent's own
credential Secret at boot, is decided and verified in the MAT-88 work.

Scope for v1: import within one installation, and upload of a Kyber export
ZIP from anywhere (which is how an archive moves between installations).

## Checkpoint: 2026-09-22 (MAT-87)

Built: `pkg/diskarchive` (format, writer, verifier, safe extractor),
`pkg/archivestore` (sealed chunks, built-in store server/client, S3, GCS),
`pkg/archivejob` (Postgres + memory job stores), the export worker and routes
in `pkg/api`, the reconciler hold, `kyber-disk-archive` and
`kyber-archive-store` binaries in the control-plane image, the chart's
`agentArchives` values and archive store, the Agent Detail Disk export card,
OpenAPI, and docs. Tests cover the archive format (traversal, absolute paths,
duplicates, special files, symlink-parented writes, zip bombs, corrupt
content, unsupported versions, capacity), sealing (tamper, truncation, wrong
key), the full export lifecycle against a fake cluster (pause, same-node
read-only pod, upload auth, release, verification, download links, expiry,
retention), cancel, pause timeout, deadline, oversized and corrupt uploads,
operator intent during release, scope and agent-resource checks, the
reconciler hold (envtest), chart rendering, and the PWA card.

Not yet verified on a real cluster: export of a real agent on k3s local-path,
GKE PD and EKS EBS, a nearly full disk, and a control-plane restart mid-job.
Those need a canary run after deployment; the test agent is Matt's call.

## Review checkpoint: 2026-09-22 (MAT-87)

An adversarial review found, and this branch fixes: an empty `desiredPhase`
left the agent stopped after export; an operator's Stop during an export was
undone on release; a stale job snapshot could fail a good export and delete
its bytes; backslash and non-UTF-8 names failed verification after the agent
had been stopped; verification ran serially in the control plane and could
hold other agents or exhaust its memory; a pod stuck on image pull held the
agent for the full timeout and got no pull secrets; a second upload attempt
was not refused; an empty manifest passed inspection; and restrictive
directory modes were applied before their children were touched. Each has a
regression test.
