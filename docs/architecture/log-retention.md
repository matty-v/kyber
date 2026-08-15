# Durable Agent Log Retention

How kyber retains agent logs **off-cluster** so an operator can pull a single
agent's output for an **absolute time window** that survives pod restarts —
alongside, not replacing, the live kubelet tail.

**Source of truth:**

- Shipper infra: `infra/terraform/profiles/small/storage.tf` (GCS bucket +
  lifecycle + bucket-scoped IAM), `deploy/helm/kyber/templates/log-shipper/`
  (Vector DaemonSet, ConfigMap, ServiceAccount + RBAC),
  `deploy/helm/kyber/templates/minio/` (optional in-cluster MinIO, gated),
  `deploy/helm/kyber/values.yaml` (`logShipper:` + `minio:` blocks).
- Read path: `pkg/api/routes_logs.go` (`handleAgentLogs` `source` branch,
  `parseArchiveWindow`, `handleAgentLogsArchive` / `handleAgentLogsTranscript`
  over the shared `serveWindowedLines`), `pkg/api/archive_reader.go`
  (`ArchiveReader`, `GCSArchiveReader`, `S3ArchiveReader`, `rootPrefix`-keyed),
  wired in `cmd/control-plane/main.go` from `KYBER_LOG_ARCHIVE_BACKEND` (+ bucket/
  endpoint/credential config), built once per surface root (`agents/` archive,
  `transcripts/` transcript).
- Transcript lane (kyber#446): the `transcript-tailer` sidecar
  (`pkg/controllers/agent/transcript_tailer.go`, injected by `reconciler.go`)
  ships the agent's Claude Code session JSONL on its own stdout stream; Vector
  routes it by `container_name` to the `transcripts/<agent>/<date>/` prefix; the
  control plane serves it via `?source=transcript`.

> **Provider-agnostic (kyber#437).** The archive backend is pluggable: `gcs`
> (GCS via node ADC, GCP installs) or `s3` (any S3-compatible store — MinIO/AWS —
> via Secret credentials, works on any cluster regardless of compute provider).
> Read semantics, the API surface, and per-agent isolation are identical across
> backends; only the object *source* and credential model differ. See
> [Pluggable backend](#pluggable-backend-gcs--s3minio) below.

> **Boundary vs. metrics.** This is the *logs* pipeline — raw agent stdout for
> human reconstruction. It is distinct from the *metrics* pipeline
> ([`metrics-data-flow.md`](metrics-data-flow.md), OTel/#256): metrics are
> numeric time series; logs are text lines. The two share no storage and no
> code path. Adjacent: off-cluster DR backups (#171) could later share bucket /
> credential plumbing, but do not today.

---

## Data flow

```
  agent pod stdout/stderr
        │  (written by the container runtime)
        ▼
  /var/log/pods/…  +  /var/log/containers/*kyber-system*   (on the k3s node)
        │
        ▼
  ┌──────────────────────────────────────┐   PR-A (infra only)
  │ log-shipper DaemonSet (Vector)        │   - reads node container logs (hostPath, read-only)
  │  - kubernetes_logs source             │   - keeps only kyber-system pods that
  │  - filter: ns kyber-system            │     carry the label kyber.io/agent
  │    + label kyber.io/agent             │   - lifts the agent name out of the label
  └──────────────────────────────────────┘
        │  node ADC (GCE node SA, cloud-platform scope — NO static key)
        ▼
  gs://<bucket>/agents/<agent>/<YYYY-MM-DD>/<ts>-<uuid>.ndjson
        │  ▲ lifecycle rule: auto-delete age > log_retention_days  (bounded retention)
        │
        ▼
  ┌──────────────────────────────────────┐   PR-B (API + PWA)
  │ GET /agents/{name}/logs?source=archive│   - lists objects under agents/<name>/ ONLY
  │   &since=<RFC3339>&until=<RFC3339>     │   - filters lines to [since, until]
  │  → control plane reads GCS via node ADC│  - 400 on bad/missing RFC3339 or until<since
  └──────────────────────────────────────┘
        │
        ▼
  PWA LogViewer "Archive" source toggle + from/to pickers
```

### Transcript lane (kyber#446) — what the agent actually *did*

The archive lane above ships agent **pod stdout** (the `[kyber]` boot wrapper).
The agent's real work — every user message, assistant turn, and tool call/result
— is written by Claude Code as JSONL at `~/.claude/projects/<project>/*.jsonl` on
the agent PVC, and never reaches container stdout. A dedicated sidecar ships it
on a **clean, isolated lane** that reuses the same pipeline (sidecar → Vector →
object store → windowed read), one tier up:

```
  agent pod
   ├─ agent (runtime)        stdout: [kyber] boot wrapper ─┐
   ├─ kyber-status-sidecar   heartbeats                    │  Vector (kubernetes_logs,
   └─ transcript-tailer (NEW) stdout: raw session JSONL ───┤   label kyber.io/agent)
        mounts persist RO         tails *.jsonl            │  remap routes by container_name:
        + offsets PVC RW (durable checkpoints)             │ transcript-tailer → transcripts/<agent>/<date>/
        reuses agent runtime image (root, reads root-owned JSONL) │ everything else → agents/<agent>/<date>/
                                                           └─ (durable offsets PVC survives pod recreation)
                                                                      │
   GET /logs?source=transcript ── rootPrefix-keyed ArchiveReader (root "transcripts/") ──┘
   GET /logs?source=archive    ── same reader, root "agents/" (unchanged)
```

- **Discovery + ship (single-process, active-set bounded — kyber#584).** The
  sidecar mounts the PVC **read-only** at `/agent-home` and polls two
  mode-dependent roots — `…/overlay/upper/home/kyber/.claude/projects` (overlay
  boot) or `…/home/.claude/projects` (bind-mount-HOME fallback), per
  `images/agent-base/entrypoint.sh` — captured as named constants in
  `transcript_tailer.go`. It is **one poll loop**, not a per-file `tail -F`
  follower-process fan-out: each poll walks every `*.jsonl` and ships only the
  un-shipped lines, in process, one file at a time. So the tailer's peak memory
  is bounded by the **active session set, not the total session-file count** —
  the kyber#584 invariant. (The old model ran one backgrounded `tail -F | mawk`
  follower per file, bounded `MAX_TAILS=64` concurrent plus the churn of
  starting-then-evicting hundreds more in one discovery pass; on an aged agent —
  r2-d2: 834 files / ~20 MB — that *per-file process* model OOM-looped the 512 Mi
  sidecar even though the byte backlog was tiny. Memory scaled with file COUNT.)

  Each newly-seen `*.jsonl` is shipped from a **persisted per-file offset**: the
  tailer writes each file's running shipped-line count to a checkpoint and on
  (re)start resumes one line past it (default line 1 when no checkpoint), never
  `-n 0`. **Active-set bounding (Phase A):** an *idle* file — one whose byte size
  is unchanged since its checkpoint (JSONL is append-only) — is skipped with a
  single `stat` and is **not held in any live tail set**; it is re-admitted
  automatically the poll its size grows again. The byte-size companion
  (`<checkpoint>.size`) that powers the idle-skip is **additive** — the offset
  format (the line-count checkpoint) is unchanged, so offsets stay valid across
  both an upgrade and a rollback. **Incremental read (Phase B):** an active file
  is read once through `mawk`, which streams a line at a time (bounded buffer,
  never a whole-file slurp), so peak RSS is O(one line) — independent of file
  count.

  The checkpoint store is a **dedicated, small, writable per-agent offsets PVC**
  (`agent-<name>-offsets-pv`, kyber#467) — so the offset survives **both a sidecar
  (container) restart AND a pod recreation**. This is the durable fix for the
  earlier failure mode: kyber#458 first put the checkpoint on a pod-lifetime
  `emptyDir`, which was **wiped on pod recreation**. Because the agent's old
  session `*.jsonl` survive on the (read-only) persist PVC, a recreated pod with
  no checkpoints re-found every historical file and re-shipped each from line 1,
  fanning out up to `MAX_TAILS=64` concurrent `tail|mawk` over multi-MB lines and
  OOM-killing the sidecar (the #466 incident; #466 bumped memory 64Mi→512Mi to
  *tolerate* the burst). With the durable PVC every old file now resumes at ≈EOF
  on recreation → **≈zero re-ship**.

  Two guards keep the durable checkpoint safe and bounded:
  - **Stale-checkpoint clamp.** A durable offset can outlive a file
    rotation/truncation. If the stored offset points past the file's current EOF,
    the tailer re-ships from line 1 rather than silently shipping nothing — a
    duplicate line is benign, a skipped line is an audit hole.
  - **Partial-line safety.** `total` is `wc -l` (newline-terminated lines only)
    and shipping stops before any half-written trailing line (no newline yet), so
    a partial line is never shipped or checkpointed — it ships the next poll once
    its newline lands. (This replaces the old `MAX_COLD_TAILS` cold-tail cap,
    which only mattered when many files could ship *concurrently*; the
    single-process reader ships sequentially, so peak memory is inherently bounded
    with no cap to tune — kyber#584.)

  Completeness holds: resume is from the last *shipped* line, never skipping. The
  agent persist PVC stays mounted **read-only** (#446); only the separate offsets
  PVC is writable, so no persist access-mode/StorageClass change is needed. The
  offsets PVC defaults to the **cluster-default StorageClass (local-path) on all
  targets** — deliberately *not* `kyber-pd` on gcp, whose 1Gi PD minimum would
  waste space for sub-1KB checkpoints (it is owner-referenced to the Agent CRD and
  GC'd on deletion).
- **Isolation by container.** Vector keys the object path off `container_name`,
  so transcript lines land under `transcripts/…` and every other container's
  stdout under `agents/…`; the two object sets never intermix.

## Object layout (the cross-component contract)

Objects are keyed:

```
agents/<agent>/<YYYY-MM-DD>/<filename>.ndjson
```

- `<agent>` is the value of the pod label `kyber.io/agent` (not the pod name).
- `<YYYY-MM-DD>` is the UTC date partition of the line's emit time.
- Each object is **NDJSON**: one JSON object per line, carrying at least
  `timestamp` (RFC3339) and `message`. The reader parses these two fields and
  ignores the rest; blank or unparseable lines are skipped.

The transcript lane (kyber#446) uses the **same key shape under a different
root**:

```
transcripts/<agent>/<YYYY-MM-DD>/<filename>.ndjson
```

Both roots are written by the **same** Vector sink, keyed by `container_name`
(`transcript-tailer` → `transcripts`, everything else → `agents`), and read by
the **same** `ArchiveReader` parameterized on its root prefix — so all the
invariants below hold identically for each lane, and the two never intermix.

This key is the contract between the shipper (writer) and the read path
(reader). Two invariants ride on it:

1. **Per-agent isolation.** The reader lists strictly under the
   `<root><agent>/` prefix (`objectPrefix(rootPrefix, agent)` →
   `agents/<agent>/` or `transcripts/<agent>/`), so a read for agent X can never
   enumerate agent Y's objects, and an archive read can never surface a
   transcript object (or vice versa). The trailing slash matters: `agents/dave/`
   is not a prefix of `agents/dave2/…`.
2. **Window-bounded list cost.** Because objects are date-partitioned, a window
   query lists only the day-prefixes the window spans
   (`dayPartitionPrefixes`), not the agent's whole history.

## Memory-bounded read (kyber#455)

A windowed read used to buffer **the whole window in memory**: the S3 path built
a `map[string][]byte` of every object, `io.ReadAll`'d each object whole, and
accumulated every parsed line before filtering. A large/wide read (the `lando`
coordinator's transcript) blew past the control-plane memory limit and the
kernel **OOM-killed the pod** — `CrashLoopBackOff`, 502s on *every* endpoint, not
just the log surface. Two layers now bound it:

1. **Stream one object at a time.** `windowScanner` reads each object's NDJSON
   line-by-line straight off the storage stream (`io.ReadCloser` — GCS object
   reader / `*minio.Object`), keeping only in-window lines, and the reader
   **closes each object before fetching the next**. At most one object's bytes
   are resident at a time — a single colossal object is never `io.ReadAll`'d
   whole. (Regression-guarded: the S3 reader test asserts at most one object is
   open concurrently.)
2. **Hard cap + truncation signal.** A read accumulates at most
   `defaultMaxReturnedLines` (**50 000**) in-window lines and scans at most
   `defaultMaxScannedBytes` (**128 MiB**) before stopping. The returned slice is
   therefore bounded regardless of window width or object count. When a cap is
   hit the read stops early and returns the bounded prefix with
   `ReadResult.Truncated = true`.

**Wire contract (ADDITIVE).** `GET /api/v1/agents/{name}/logs?source=archive` and
`?source=transcript` still stream `200 OK` newline-delimited `text/plain` (the
PWA reader is unchanged). When the result was capped, the response carries the
header **`X-Kyber-Log-Truncated: true`** so a caller can always tell it received
a bounded prefix rather than the whole window — there is no silent partial. A
complete read omits the header. No existing field or status changes, so this is a
backward-compatible (minor) addition.

> The cap is a safety ceiling, not the headroom: the control-plane
> `controlPlane.resources.limits.memory` was also raised (512Mi → 1Gi, with
> requests 128Mi → 256Mi) in the chart default — neither the falcon nor razer
> overlay overrides `controlPlane.resources`, so the chart default governs both.
> That is mitigation; the streaming + cap above is the actual fix.

### Aggregate read-concurrency cap (kyber#463)

The per-read caps above bound **one** read; they do not bound how many run at
once. N concurrent large reads each hold their own working set and each run a
CPU-heavy NDJSON parse, so a burst (Boba Fett measured ~6 × a 19 MB transcript on
a 256Mi/500m CP) collectively exhausts the control plane — and because the same
process serves the whole API, it **starves the liveness probe**, which kubelet
then SIGKILLs (`exit 137`), 502'ing `/version` and the agents list in the process.
That is the same isolation failure as kyber#455, now at the concurrency dimension.

The fix is a **counting-semaphore in-flight cap** on `serveWindowedLines` (the
shared archive+transcript chokepoint): at most `controlPlane.maxConcurrentReads`
(`KYBER_MAX_CONCURRENT_READS`, default **2**) windowed reads run at once. The slot
is acquired *after* the cheap window/nil checks but *before* `ReadAgentLines` (the
heavy step, before any byte is written) and released via `defer` on every return
path (including a client disconnect mid-stream, so a dropped client can't leak a
slot). This bounds **both** failure modes by construction — aggregate read memory
≤ K × the per-read cap, and concurrent parses ≤ K, so the probe goroutine is never
starved. The two layers are complementary: kyber#456 bounds each read, kyber#463
bounds their sum.

**Wire contract (ADDITIVE).** An over-cap read gets an immediate
`429 Too Many Requests` (`too_many_concurrent_reads`) with a `Retry-After` header
and **no body** — a clean backpressure signal to retry, never a partial or crashed
stream. The gate wraps **only** the read handlers, so `/version`, the agents list,
and the `:8081` probe listener are never throttled (endpoint isolation under
concurrency). This is a **CPU/contention bound**, not a memory one — a memory bump
would not fix the starvation; bounding concurrency does. Defense-in-depth: the
liveness probe's `timeoutSeconds` (1→2) and `failureThreshold` (3→4) were widened
so a transient spike can't SIGKILL before the cap takes effect.

> **Transcript read-side dedup (#454, landed).** Historically the transcript-
> tailer re-shipped each session JSONL from line 1 on every (re)start, so the
> cumulative objects under `transcripts/<agent>/<date>/` held each message 2-3x.
> kyber#458 first persisted a tail offset across sidecar restarts, and kyber#467
> made that offset durable across **pod recreation** too (dedicated offsets PVC;
> see the tailer section above), so new re-ships are largely eliminated — but
> read-side dedup stays valuable for any residual overlap (e.g. the bounded
> first-boot/restore re-ship under `MAX_COLD_TAILS`, the clamped re-ship of a
> rotated file, or pre-#458 objects not yet compacted/expired). The
> transcript read path now **dedupes by stable id** at the `windowScanner`
> accumulation step: each in-window line's id is the first non-empty of
> `message.id` → `uuid` → `leafUuid` (`extractStableID`); the first occurrence is
> kept, exact-id re-ships are dropped, and timestamp order is preserved. An
> id-less line (no `message.id`/`uuid`/`leafUuid`) is always kept — dedup only
> collapses exact repeats, never distinct content. Dedup is **scoped to the
> transcript root** (`transcripts/`); `source=archive` (`agents/`) runs with
> dedup off and is byte-for-byte unchanged. The `seen` set gains one entry only
> per *kept* line, so `|seen| ≤ |lines| ≤ maxLines` — it rides inside the
> existing line cap and reintroduces no unbounded-memory vector; #456's memory
> bound never depended on this dedup. The tailer's ship-from-beginning
> completeness guarantee is left intact by design — read-side dedup is what makes
> it safe, no RO-mount checkpoint needed.

## Pluggable backend (GCS / S3+MinIO)

`ArchiveReader` (`pkg/api/archive_reader.go`) is the seam. Two implementations
share the storage-independent core (a streaming `windowScanner` — per-line
`parseArchiveLine` + inclusive-window filter + cap, plus `objectPrefix` /
`validArchiveAgentName` / `dayPartitionPrefixes`); each impl only differs in *how
it lists and streams objects*. Each impl also carries a **`rootPrefix`** (default
`agents/`, empty ⇒ `agents/` for backward compatibility) so the **same**
implementation serves both the archive lane (`agents/`) and the transcript lane
(`transcripts/`) — the control plane constructs two instances against the same
bucket, differing only in this prefix (kyber#446).

> **Memory-bounded read (kyber#455).** `ReadAgentLines` returns a
> `ReadResult{Lines, Truncated}` (both surfaces share this contract). See
> *Memory-bounded read* below for why the read streams one object at a time, what
> the cap is, and how `Truncated` surfaces on the wire.

| | `GCSArchiveReader` | `S3ArchiveReader` (kyber#437) |
|---|---|---|
| Object source | GCS bucket | any S3-compatible store (MinIO, AWS S3) |
| SDK | `cloud.google.com/go/storage` | `github.com/minio/minio-go/v7` |
| **Credentials** | node ADC (GCE node SA) — **no static key** | **static access/secret key from a K8s Secret** |
| Endpoint | implicit (GCS) | operator-configured (`KYBER_LOG_ARCHIVE_ENDPOINT`) |
| Works on | GCP installs | any cluster (no cloud identity needed) |

Selected at startup by `KYBER_LOG_ARCHIVE_BACKEND` (default `gcs`,
backward-compatible) in `cmd/control-plane/main.go`. The writer half mirrors it:
the Vector sink is `gcp_cloud_storage` or `aws_s3` by `logShipper.backend`, both
writing the **same** `agents/<agent>/<date>/*.ndjson` layout. An optional
in-cluster **MinIO** (`deploy/helm/kyber/templates/minio/`, gated by
`minio.enabled`, default off) provides the S3 store on clusters without a cloud
object store (e.g. kyber-laptop, kyber-falcon). The per-cluster enablement
(`backend: s3` + endpoint/bucket/creds) lives in **kyber-deploy**
(`environments/{razer,falcon}/values.yaml`), not this chart.

```
   Vector DaemonSet sink (by logShipper.backend)        control-plane reader (by KYBER_LOG_ARCHIVE_BACKEND)
     gcs → gcp_cloud_storage  (node ADC)                  gcs → GCSArchiveReader (node ADC)
     s3  → aws_s3 (endpoint)  (Secret creds)              s3  → S3ArchiveReader  (Secret creds)
                         ▼                                                    ▼
            object store: GCS  |  MinIO/S3  —  layout agents/<agent>/<YYYY-MM-DD>/*.ndjson (identical)
```

## Credential path

**GCS backend** — both the shipper and the read path authenticate via
**Application Default Credentials** (the GCE node service account,
`cloud-platform` scope, attached in `k3s.tf`). **No static key file** is created,
mounted, or stored. Authorization is narrowed at the IAM layer:
`roles/storage.objectAdmin` granted **bucket-scoped**
(`google_storage_bucket_iam_member`), not project-wide.

**S3/MinIO backend (kyber#437)** — there is no cloud identity, so the store
authenticates with a **static access-key/secret-key pair sourced from a
Kubernetes Secret** (`secretKeyRef`), referenced by both the Vector writer
(`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`) and the control-plane reader
(`KYBER_LOG_ARCHIVE_ACCESS_KEY`/`_SECRET_KEY`). This is the material new risk vs
ADC: the key is long-lived and must **never** be committed to values/git. Apply
least privilege — the writer needs only put-object, the reader only get/list on
`agents/*`; MinIO supports per-key policy, so split writer/reader keys are
preferred (a single bucket-scoped key is acceptable for a first cut). Rotation =
swap the Secret and restart the two consumers. The in-cluster MinIO API/console
is reachable **only on the cluster network** (ClusterIP Service, no Ingress).
TLS is selected by the endpoint URL scheme (reader: `KYBER_LOG_ARCHIVE_USE_TLS`);
default on for external S3.

## Retention invariant

Bounded retention must hold on **every** backend — it is the only thing that
caps storage cost and how far back `source=archive` can reach.

- **GCS:** an Object Lifecycle rule deletes objects older than
  `var.log_retention_days` (default **30**). GCS enforces it; no cron.
- **S3/MinIO:** the chart's MinIO bootstrap Job applies a bucket object-expiry
  lifecycle (`minio.retentionDays`, default **30** — parity with GCS) via `mc
  ilm`. Without this the durable logs would accumulate unbounded on local
  clusters; retention parity is a product AC (kyber#437), not an option.

Compaction is an **optional, on-demand reclaim**, not a requirement: the object
store's expiry is the enforced backstop. The pre-kyber#458 tailer left duplicate
overlapping objects per `transcripts/<agent>/<date>` prefix (now stopped at
source by the offset fix above). `cmd/transcript-compact` (a helm-gated Job,
`transcriptCompaction.enabled`, default off) merges those into one deduped
superset and deletes the redundants — dry-run by default, destructive `--apply`
gated on Matt's go. It dedups on the same stable id as the read path and stays
within the kyber#456 memory caps. See
[transcript-compaction.md](../operator/transcript-compaction.md). Anything left
uncompacted still ages out via the lifecycle expiry.

### PVC-side retention (kyber#471)

The three retention mechanisms operate on **different storage** and must not be
conflated:

| Mechanism | Acts on | Enforced? | Purpose |
|---|---|---|---|
| Object-store lifecycle expiry (`minio.retentionDays` / GCS `log_retention_days`, 30d) | Durable archive objects | **Yes** — the backstop | Caps how far back `source=archive` reaches; bounds bucket cost |
| Transcript compaction (`transcriptCompaction`, on-demand) | **Duplicate** archive objects | No (optional reclaim) | Reclaims pre-#458 overlapping objects in the bucket |
| **PVC-side pruning (`transcripts.retention`, new)** | **On-PVC** `*.jsonl` working set | Default-on, self-healing | Bounds the per-agent on-PVC backlog (~4MB/day/agent) |

PVC-side pruning is a per-agent **transcript-pruner sidecar** (a separate
RW-mounted container in the agent pod — *not* the read-only tailer, kyber#446).
It runs an internal poll loop (a sidecar, not a CronJob, because the `persist`
PVC is `ReadWriteOnce` and only reliably mountable from the pod that holds it),
and removes session JSONL the tailer has already shipped to the archive once it
passes the age policy (default `maxAgeDays: 7`, ≫ the minutes-scale ship lag) or
an optional per-agent size ceiling (`maxBytesPerAgent`).

**Safety property (the load-bearing invariant):** the pruner deletes only
**local** copies the object store already holds — it carries **no object-store
credentials on the default path and never deletes archive objects**, so the
durable record is structurally out of its reach. The active (newest) session
file is never pruned. Therefore PVC-side pruning **cannot reduce what
`?source=transcript` returns** — reads are served from the archive, not the PVC.
By shrinking the on-PVC working set it also bounds the tailer's discovery set,
protecting the #454/#455/#463/#466 read-side and cold-start bounds over time.
The optional `archiveCrosscheck` adds a belt-and-suspenders gate using the
tailer's local ship checkpoint (proof a file was fully shipped) before delete; it
needs no credentials and is off by default. See
[transcript-pvc-retention.md](../operator/transcript-pvc-retention.md). The
#446 read-only-tailer invariant is preserved: the tailer's mount stays
read-only, the pruner is a distinct container, and the PVC access mode is
unchanged (`ReadWriteOnce`).

### Operator note — the `transcripts/` lane is secret-bearing (kyber#446)

- **New object prefix.** `transcripts/<agent>/<date>/*.ndjson` lives in the
  **same bucket** as `agents/…`, under the same backend/credentials/lifecycle
  rule (same `log_retention_days` / `minio.retentionDays` expiry applies). No new
  bucket, key, or IAM grant is introduced.
- **Storage footprint.** Full session transcripts are **materially larger** than
  boot stdout (every prompt, turn, and tool result — a single line can carry a
  large tool output). Budget retention accordingly; operators may want a
  **shorter lifecycle cap** on `transcripts/` than on `agents/` given the size
  and sensitivity. (`parseNDJSONLines` raises the scanner buffer to 4MB, which
  covers normal lines; a pathological single line beyond that is skipped.)
- **Read-only data mount; runs as root, but tightly scoped.** The
  `transcript-tailer` mounts the PVC **read-only**, so it cannot mutate agent
  state or transcript files. It runs as **root (uid 0)** — like the agent and
  `session-brief` containers — because the session JSONL is root-owned (the
  agent can become root in-pod) and the tailer must read it over the RO mount
  (kyber#451). Despite the root uid it is **not privileged**, adds **no
  capabilities**, and sets `allowPrivilegeEscalation: false` +
  `readOnlyRootFilesystem: true` — strictly more locked down than the agent
  container beside it. (A fully non-root tailer would require making the whole
  agent runtime non-root — image `USER` + chown — tracked as a kyber#451
  follow-up.)
- **Access classification.** Because `?source=transcript` returns full session
  content, treat `transcripts/` as a **secret-bearing store**: confirm
  archive-read access is already scoped to an audience cleared for that content.

## The `source` discriminator (read path)

`GET /api/v1/agents/{name}/logs` branches on `source`:

| `source` | Behavior | Window params |
|---|---|---|
| omitted / `kubelet` | live kubelet ring-buffer tail — **byte-for-byte unchanged** | relative: `since` is a Go duration; `tail`/`follow`/`previous`/`container` |
| `archive` | durable read via `ArchiveReader` rooted at `agents/` (GCS or S3/MinIO) | absolute: `since`/`until` are **required RFC3339** |
| `transcript` | durable read via the same reader rooted at `transcripts/` — the agent's Claude Code **session JSONL** (kyber#446) | absolute: `since`/`until` are **required RFC3339** |
| anything else | `400 invalid_source` (message lists `kubelet`, `archive`, `transcript`) | — |

Archive- and transcript-mode validation are **identical** (one shared handler,
`serveWindowedLines`): missing/malformed RFC3339 bound or `until < since` →
`400 invalid_window`; the reader unconfigured (required backend config absent) →
`503` whose body **names the missing config keys** (`ArchiveDisabledReason` /
`TranscriptDisabledReason`, set at startup — never their values, so a credential
can't leak); read failure → `502 archive_read_error`. Auth is inherited from the
protected `/api/` mux and the source branch runs **after** both the Bearer-key
middleware and the agent-existence check, so every source rejects unauthenticated
requests and 404s a missing agent identically.

> **`source=transcript` is secret-bearing.** Where `source=archive` exposes
> boot-stdout noise, `source=transcript` exposes the agent's **entire working
> content** — every user prompt, assistant turn, and tool result, which may
> contain secrets. Its authz is the **same protected mux** as `source=archive`
> (no broader), but operators must confirm archive-read access is already
> restricted to an audience appropriate for full session content before relying
> on it. See the operator note below.

## Failure modes

- **GCS unavailable (read):** the archive read returns `502 archive_read_error`;
  it never falls back to or corrupts the live kubelet path.
- **GCS slow (write):** Vector's disk buffer back-pressures locally on the node;
  agent pods are never touched.
- **Shipper restart:** the Vector data dir is a hostPath, so checkpoints survive
  and the node resumes shipping where it left off rather than re-shipping.
- **Bucket unset but `logShipper.enabled`:** the chart fails fast at render time
  (a misconfiguration should not deploy a shipper with nowhere to write).

## Deliberately out of scope

- Cross-agent correlation by issue/PR number (PR-C, deferred).
- Log scrubbing / PII redaction at ship time — retention now *persists* agent
  stdout off-cluster for the retention window, where before it died with the
  pod. Bounded retention + a private, uniform-access bucket cap the exposure;
  scrubbing is a conscious follow-up, not an omission.
- Archive-read pagination / hard response cap for abusively wide windows.
- **Authz tightening for `source=transcript`'s elevated sensitivity** (kyber#446):
  if a review of who currently holds archive-read access finds it broader than
  the audience appropriate for full session content, raising the bar for
  `source=transcript` is a separate follow-up — flagged, not gated into this
  additive feature.
- PWA UI for transcript content, retroactive backfill, transcript search /
  structured JSONL field parsing, and a streaming (long-poll) transcript
  endpoint (all per kyber#446).
- Downstream dedup-by-`uuid` for the rare sidecar-restart duplicate — only if
  duplicates prove noisy in practice.
