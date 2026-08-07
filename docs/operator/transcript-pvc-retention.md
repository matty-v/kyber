# Transcript PVC retention (operator runbook)

How the on-PVC transcript backlog is bounded, and how to tune or disable it
(kyber#471).

## Background

Each agent's Claude Code session transcripts (`~/.claude/projects/<project>/*.jsonl`)
accumulate on the agent `persist` PVC and were never pruned — they grew
~4MB/day/agent unbounded. That on-PVC backlog inflates the transcript-tailer's
discovery set (the amplifier behind the kyber#466 cold-start re-ship OOM) and the
read-side scan cost over time.

The **transcript-pruner sidecar** bounds it. One extra container per agent pod
runs an internal poll loop and removes session JSONL the tailer has already
shipped to the durable archive (`transcripts/<agent>/<date>/`) once it passes the
retention policy. It is a *separate* container from the transcript-tailer: the
tailer keeps its **read-only** PVC mount (kyber#446) untouched, while the pruner
gets its own read-write mount. The PVC access mode is unchanged (`ReadWriteOnce`).

> **Why a sidecar, not a CronJob.** The `persist` PVC is `ReadWriteOnce` and only
> reliably mountable from the pod that already holds it on its node. On-PVC
> pruning must run inside the agent pod — the same reason the tailer is a sidecar.

## The safety property

The pruner only ever deletes **local** copies the object store already holds:

- The durable archive copy is the authoritative record and is **structurally out
  of the pruner's reach** — the default path carries **no object-store
  credentials** and the pruner never touches archive objects.
- The default age threshold (`maxAgeDays: 7`) is ~10,000× the minutes-scale
  tailer→Vector ship lag, so an older file is demonstrably fully shipped.
- The **active (newest) session file is never pruned** — an agent never ends up
  with zero session files.

Therefore pruning **cannot reduce what `GET /api/v1/agents/{name}/logs?source=transcript`
returns** — reads are served from the archive, not the PVC. A pruner bug at worst
deletes an already-archived local copy that served no read purpose; blast radius
is one agent's PVC, never the audit record.

## Configuration

Enabled by default (see the deploy-review rationale in kyber#471 — this bounds
unbounded growth, like a memory limit, so it self-heals on every cluster). Tune
or disable per-cluster in `kyber-deploy environments/<cluster>/values.yaml`:

```yaml
transcripts:
  retention:
    enabled: true            # set false to opt a cluster out entirely
    maxAgeDays: 7            # prune *.jsonl older than this (>> ship lag; active file exempt)
    maxBytesPerAgent: ""     # optional size ceiling, e.g. "200Mi"; "" = age-only
    pruneIntervalMinutes: 60 # sidecar poll cadence
    archiveCrosscheck: false # belt-and-suspenders ship-confirmation before delete (off by default)
```

- **`maxAgeDays`** — primary policy. Files older than this (by mtime) are
  prune-eligible. The active/newest file is always exempt.
- **`maxBytesPerAgent`** — optional secondary ceiling (a k8s quantity, e.g.
  `"200Mi"`). When set, age-eligible files are pruned **oldest-first only until
  the on-PVC transcript total is at/under the ceiling**, so some archived files
  may be retained if already under budget. Never prunes recent/active files.
- **`archiveCrosscheck`** — when `true`, a file is only pruned if the tailer's
  **local ship checkpoint** confirms it was fully shipped (line count shipped ≥
  current line count), in addition to the age threshold. This needs no
  credentials (it reads the tailer's pod-lifetime offsets, mounted read-only),
  and an old-but-unconfirmed file is retained. Off by default — the age threshold
  alone is archive-safe.

Changing values takes effect on the next control-plane release (the env is read
at startup and threaded to newly-created agent pods). Existing pods pick up a
changed sidecar config when they are next recreated.

## Recommended per-cluster tuning

- **gcp (prod):** consider `maxBytesPerAgent: "200Mi"` as a hard ceiling — GCE-PD
  storage costs money, and a size cap protects against atypically large sessions
  without waiting for age expiry. At ~4MB/day × 7d ≈ 28MB steady-state this rarely
  bites, but it's cheap insurance.
- **razer / falcon:** defaults are appropriate; local-path storage is effectively
  free at these sizes.

## Verification (not "pods up")

1. **Sidecar present:**
   `kubectl get pod agent-<name> -n kyber-system -o jsonpath='{.spec.containers[*].name}'`
   → includes `transcript-pruner`.
2. **#446 invariant holds:** the `transcript-tailer` container's `persist` mount
   still shows `readOnly: true`; the `transcript-pruner`'s `persist` mount is
   read-write.
3. **Prune decision correct:** on a non-critical agent, `touch -d "8 days ago"` a
   synthetic `*.jsonl` under the projects root; within `pruneIntervalMinutes` (or
   after exec-running the prune script) the aged file is gone while a fresh
   session file remains.
4. **No audit loss:** `GET /api/v1/agents/{name}/logs?source=transcript` over a
   window covering the pruned file still returns its content (served from the
   archive).
5. **Not crash-looping:** the `transcript-pruner` container shows 0 restarts after
   a `pruneIntervalMinutes` wait.

## Rollback / safety

- Set `transcripts.retention.enabled: false` (chart or per-cluster). The sidecar
  is no longer injected into new pods; existing pods drop it on their next
  recreate. No data migration, nothing to undo — deletions were of already-archived
  local copies.
- If a file was pruned that shouldn't have been, the authoritative copy is in the
  object store; `?source=transcript` reads serve from there. No operator action
  needed beyond disabling the sidecar.

## See also

- [`../architecture/log-retention.md`](../architecture/log-retention.md) §
  "PVC-side retention (kyber#471)" — how this relates to object-store lifecycle
  expiry and transcript compaction (the three distinct retention mechanisms).
- [`transcript-compaction.md`](transcript-compaction.md) — the object-store
  sibling (reclaims duplicate archive objects).
