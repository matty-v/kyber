# Transcript compaction (operator runbook)

How to reclaim duplicate transcript objects in the log bucket (kyber#458 Part B).

## Background

The transcript-tailer sidecar ships each Claude Code session JSONL to
`transcripts/<agent>/<date>/` via Vector. Before kyber#458 **Part A**, a sidecar
restart re-shipped the whole session from line 1, leaving multiple overlapping
cumulative NDJSON objects under one `<agent>/<date>` prefix — storage
amplification, bounded only by the 30-day lifecycle expiry.

- **Part A (source fix, already in this release):** the tailer now persists a
  per-file offset and resumes after the last shipped line across restarts, so
  **new** duplicates stop. Nothing to run. (kyber#458 first put this checkpoint on
  a pod-lifetime `emptyDir`; kyber#467 moved it to a durable per-agent offsets PVC
  so it also survives **pod recreation** — see
  [transcript-offsets-durability.md](transcript-offsets-durability.md).)
- **Part B (this tool):** `transcript-compact` reclaims the **pre-existing**
  duplicates by merging each prefix's overlapping objects into one deduped
  superset and deleting the redundant copies.

> **You may not need Part B.** Once Part A is deployed, the pre-fix duplicates
> age out within the retention window (default 30 days) on their own. Run
> compaction only if you want the space back sooner — the dry-run report tells
> you how much there is to reclaim.

## What it does

Per `transcripts/<agent>/<date>/` prefix, it streams the objects (within the
kyber#456 memory caps — a prefix whose distinct content exceeds the cap is
**skipped**, never written lossy), dedups lines by the same stable id the read
path uses (`message.id` → `uuid` → `leafUuid`; id-less lines are always kept),
writes the superset to `compacted.ndjson`, **verifies the written object contains
every source id (read-back)**, and only then deletes the redundant sources
(write-then-verify-then-delete). A read over any window returns the same deduped
set afterward.

## Dry-run (safe, default)

The Job is helm-gated and OFF by default. Enable it in dry-run to get the
reclaimable-bytes report — it writes and deletes **nothing**:

```yaml
# kyber-deploy environments/<cluster>/values.yaml (or --set)
transcriptCompaction:
  enabled: true
  apply: false   # dry-run (default)
```

Read the Job's logs:

```bash
kubectl -n kyber-system logs job/kyber-transcript-compact-1
# WOULD  transcripts/lando/2026-05-24/ — 14 objects → 11644 lines, reclaimable 38.2 MiB
# ...
# Reclaimable total: 412.7 MiB across 23 prefix(es).
# Dry-run only — nothing was changed.
```

## Destructive run (`--apply`) — GATED on Matt's go

Running `apply: true` **merges and DELETES** durable transcript objects. It is
gated:

1. **Deploy Part A first** and let it settle, so you compact a no-longer-growing
   set.
2. Run the **dry-run** and review the reclaimable-bytes report.
3. Get **Matt's explicit go** — this mutates durable audit data.
4. On GCP, deletes also need the node SA's bucket IAM to allow object delete
   (GCP IAM + Dave); on S3/MinIO, the `logShipper.existingCredentialsSecret` key
   must have delete on the bucket. Scope creds to the log bucket.
5. Then:

```yaml
transcriptCompaction:
  enabled: true
  apply: true
  runId: "2"   # bump to launch a fresh Job
```

```bash
kubectl -n kyber-system logs job/kyber-transcript-compact-2
# DONE   transcripts/lando/2026-05-24/ — 14 objects → 11644 lines, reclaimed 38.2 MiB
# Reclaimed total: 412.7 MiB across 23 prefix(es).
```

Re-runs are idempotent (the prior `compacted.ndjson` is re-read and re-merged).
Scope to one agent with `transcriptCompaction.agent: "<name>"`.

## Rollback / safety

- Dry-run changes nothing.
- `--apply` is write-then-verify-then-delete: if the merged object fails the
  read-back id-superset check, the prefix is left untouched (nothing deleted).
- A delete is one-directional; that's why it's gated, dry-run-first, and verified.
  The 30-day lifecycle remains the backstop for anything left uncompacted.
- After a run, disable the Job (`transcriptCompaction.enabled: false`) so it
  isn't re-created on the next sync.
