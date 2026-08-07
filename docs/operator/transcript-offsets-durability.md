# Transcript-offsets durability (operator runbook)

How the transcript-tailer's offset checkpoint survives pod recreation, and what
to know operationally (kyber#467).

## Background

The transcript-tailer sidecar ships each Claude Code session JSONL off the
agent's (read-only, #446) persist PVC to `transcripts/<agent>/<date>/`. To avoid
re-shipping a whole session on every restart it keeps a **per-file offset
checkpoint** (the last shipped line count) and resumes at `tail -n +N+1`.

- kyber#458 put that checkpoint on a pod-lifetime `emptyDir` — durable across a
  **sidecar (container) restart**, but **wiped on pod recreation**.
- Because the old session `*.jsonl` survive on the persist PVC, a recreated pod
  with no checkpoints re-found every historical file and re-shipped each from
  line 1, fanning out up to `MAX_TAILS=64` concurrent `tail|mawk` over multi-MB
  lines → OOMKill (the #466 incident). #466 bumped the tailer memory limit
  64Mi→512Mi to *tolerate* that burst.
- **kyber#467 (this fix)** makes the checkpoint **durable across pod recreation**
  by moving it to a dedicated per-agent **offsets PVC**, so every old file resumes
  at ≈EOF on recreation → **≈zero re-ship**.

## The mechanism

- **Offsets PVC:** `agent-<name>-offsets-pv`, a small RWO PVC (default **10Mi**),
  mounted **writable** at `/var/run/kyber-transcript-offsets` in the
  transcript-tailer container only. The agent persist PVC stays **read-only**
  (#446) — only this separate offsets volume is writable. The PVC is
  owner-referenced to the Agent CRD, so it is **garbage-collected on agent
  deletion**.
- **StorageClass:** defaults to the **cluster default (local-path) on ALL
  targets**, including gcp. Do **not** set it to `kyber-pd`: a GCE-PD has a ~1Gi
  minimum and would waste storage by four orders of magnitude for sub-1KB
  line-count checkpoints. local-path PVCs survive pod recreation (the point of
  this feature); they do not survive node replacement, but that just falls back
  to the bounded first-boot re-ship below.
- **`MAX_COLD_TAILS` (default 4):** caps how many files ship *from line 1
  simultaneously* — the memory-expensive case. With durable checkpoints this is
  rare (genuine first boot before any checkpoint exists, or a restore/migration
  where files appear at once); the cap keeps peak memory independent of backlog
  size. Forward-resume tails (checkpoint present, resuming near EOF) are exempt.
  A deferred cold tail logs `cold-tail cap (N) reached; deferring from-line-1 ship
  of <file>` to the tailer's stderr and is retried on the next poll — **no silent
  pacing, no dropped lines** (every deferred file still ships once a slot frees).
- **Stale-checkpoint clamp:** a durable offset can now outlive a file
  rotation/truncation. If the stored offset points past the file's current EOF,
  the tailer re-ships the file from line 1 rather than silently shipping nothing
  (a duplicate line is benign; a skipped line is an audit hole).

## Configuration (Helm)

```yaml
storage:
  transcriptOffsets:
    size: "10Mi"           # plenty for line-count integers at any backlog scale
    storageClassName: ""   # cluster default (local-path) — DO NOT set kyber-pd
```

Rendered into the control-plane Deployment as
`KYBER_TRANSCRIPT_OFFSETS_SIZE` / `KYBER_TRANSCRIPT_OFFSETS_STORAGE_CLASS`.

## Verifying after a deploy

```bash
# Offsets PVC is bound
kubectl get pvc -n kyber-system -l kyber.io/volume=transcript-offsets

# Persist PVC still read-only in the tailer (#446 invariant)
kubectl get pod agent-<name> -n kyber-system \
  -o jsonpath='{.spec.containers[?(@.name=="transcript-tailer")].volumeMounts}'

# Pod-recreation test on a large-backlog agent (e.g. lando):
kubectl delete pod agent-<name> -n kyber-system
# After recreation, peak memory stays well under 512Mi, no exit 137:
kubectl top pod agent-<name> -n kyber-system -c transcript-tailer
# and the object-store write volume during cold start is ~flat (no backlog re-ship).
```

## Rollback

Revert the control-plane image (and the paired kyber-deploy values). On the next
pod build the tailer reverts to the prior `emptyDir` checkpoint behavior.

- **Orphaned offsets PVCs:** owner-referenced to the Agent CRD → GC'd
  automatically when an agent is deleted. They are **not** removed by an image-only
  rollback (the agents still exist). If you need to reclaim them after such a
  rollback, sweep manually:

  ```bash
  kubectl delete pvc -n kyber-system -l kyber.io/volume=transcript-offsets
  ```

  This is safe: the offsets store holds only line-count integers. A swept agent
  simply runs one bounded (`MAX_COLD_TAILS`) re-ship on its next cold start.
- The persist PVC and all transcript data are untouched by this feature — no
  content-layer rollback is ever needed.
