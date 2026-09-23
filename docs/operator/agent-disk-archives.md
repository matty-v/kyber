# Agent disk archives

Kyber can export an agent's persistent disk as a portable ZIP (MAT-87). This
page covers where archives are kept, how to size and secure that storage, and
what an export does to a running agent.

## Requirements

- PostgreSQL (the chart's default). Job state lives in the
  `agent_archive_jobs` table; without Postgres the feature reports itself
  unavailable in `/api/v1/config` and the console.
- The internal signing key (`kyber-internal-signing-key`). It derives the key
  that encrypts stored archives and signs download links.
- A caller with the `archives:admin` scope whose `agentResources` include the
  agent. The legacy installation key has both.

## Where archives live

`agentArchives.storage.backend` selects the store:

| Backend | What it is | When to use it |
|---|---|---|
| `builtin` (default) | The chart's `kyber-archive-store` Deployment with its own `ReadWriteOnce` volume on the default StorageClass | Every target: GKE, EKS, WSL2, bare Linux. Needs nothing but a StorageClass |
| `s3` | Any S3-compatible bucket (AWS S3, MinIO) under `agentArchives.storage.prefix` | You already run object storage and want archives off-cluster |
| `gcs` | A GCS bucket via node application default credentials | GKE installs with a bucket the nodes can write |

Set `agentArchives.enabled: false` to turn the feature off; the control plane
then reports it unavailable.

Size the built-in volume (`agentArchives.storage.builtin.size`, 100Gi by
default) for the largest agent disk you expect to export times the number of
archives kept at once. An export that runs out of space fails cleanly and
releases its agent. On cloud targets the volume is billed like any other
persistent disk.

GitOps installs (Argo CD) should pre-create the store's token Secret and set
`agentArchives.storage.builtin.existingSecret`, the same pattern as MinIO and
Postgres; otherwise the chart's `lookup` fallback regenerates the token on
every sync.

## Security

- Stored archives are encrypted with a per-archive AES-256-GCM key, itself
  encrypted under a key derived from the internal signing key and kept in the
  job row. A copied bucket or archive-store volume alone discloses nothing.
  Rotating the signing key makes existing archives unreadable.
- Only the control plane can reach the built-in store (ClusterIP Service,
  NetworkPolicy, bearer token).
- The export pod runs on the agent's node with the volume mounted read-only,
  no service-account token, and one extra capability (`DAC_READ_SEARCH`, to
  read files regardless of their permissions). The verification pod mounts
  nothing and runs unprivileged. Both run the control-plane image with the
  chart's `imagePullSecrets`. It carries the agent's
  `kyber.io/agent` label, so the agent network policies apply to it, and a
  per-job token that can upload to its own job and nothing else.
- A download link is valid for ten minutes and for one job. The token is in
  the URL path, which the control plane redacts from its request log. Treat a
  downloaded archive as sensitive: it can contain local credential files
  (`~/.claude/.credentials.json`, `~/.codex/auth.json`, SSH keys). The console
  lists files that look like credentials.
- Kubernetes Secrets are never exported.

## What an export does to the agent

1. The job records what the agent is meant to be doing, sets the
   `kyber.io/archive-hold` annotation and, unless the agent is meant to be
   stopped, sets `desiredPhase: Stopped` with the `kyber.io/archive-paused`
   mark. The current session ends.
2. Once the agent pod is gone (within `agentArchives.pauseTimeout`, 5 minutes
   by default) a same-node pod archives the volume and streams it to the
   control plane. A file that changes or cannot be read fails the export
   rather than being left out.
3. As soon as the upload is stored the hold is removed and the agent goes
   back to what it was doing. If an operator used any lifecycle verb during
   the export, Stop included, their choice stands instead.
4. A verification pod reads the stored archive back, checks every entry
   against the manifest, and reports; then the job is completed.

An archive pod that cannot start (image pull failure, a container
configuration error, or still pending after 10 minutes because of node
capacity or volume attachment) fails the job at once and releases the agent.
Archives are bounded to `agentArchives.maxEntries` files (1 million) so the
export and verification pods fit their 2Gi memory limit and the manifest
stays within the 512 MiB a verifier decodes; a higher value lets an export
finish only to fail verification. File names that cannot be stored portably (not
UTF-8, or that look like path traversal to other ZIP tools) are listed as
left out.

While the hold is present the API refuses Start, Restart, force-NeedsAuth,
runtime repair and harness switching with `409 archive_in_progress`. Stop is
always allowed. Canceling, failing, or reaching `agentArchives.jobTimeout`
(2 hours) releases the agent the same way and deletes any partial bytes.

Completed archives are deleted after `agentArchives.retention` (72 hours).
`agentArchives.maxConcurrentJobs` (2) bounds exports running at once; more
get `429` with `Retry-After`.

## Troubleshooting

- **Export fails with "did not stop within the pause limit"**: the agent pod
  is stuck terminating. Check the pod's events; the agent is already released.
- **"the agent's machine has no node"**: the machine is stopped or being
  replaced. Start it and retry.
- **Export pod failed with a file path**: the file changed or was unreadable
  while archiving. Retry; if it repeats, check the path with the agent's Shell.
- **Unavailable in the console**: `/api/v1/config` → `archives.unavailableReason`
  names the missing piece.
