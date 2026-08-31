# Task-scoped progress and typed multimodal results

**Status:** Accepted design
**Date:** 2026-08-30
**Tracker:** [MAT-20](https://linear.app/matty-v/issue/MAT-20/designplatform-task-scoped-progress-and-typed-multimodal-results)
**Depends on:** [MAT-19](2026-08-30-durable-agent-tasks.md)
**Origin:** [MAT-6](https://linear.app/matty-v/issue/MAT-6/spikeplatform-what-formal-a2a-protocol-support-would-require-for-kyber) and [A2A gap study](2026-08-30-a2a-protocol-support.md)

## 1. Decision

Extend Kyber's native durable task model with cooperative, task-scoped progress
and multiple named typed results. Version one supports:

- bounded inline UTF-8 text;
- bounded validated JSON;
- controlled files produced under an allowlisted `/persist` root and copied
  into Kyber-owned object storage; and
- media metadata sufficient to represent images, audio, video, documents,
  archives, patches, and other files without exposing the agent filesystem.

Files are in the first delivery slice by Matt's decision on 2026-08-30. Raw
filesystem paths, arbitrary remote URLs, and inline binary blobs are not part
of the public result contract.

Claude Code and Codex remain the execution engines. They already know how to
use tools and produce files. Kyber adds a thin, common task MCP surface that
validates, persists, authorizes, and presents their reported outputs. It does
not parse transcripts, hidden reasoning, shell output, or provider-specific
event streams into progress or results.

## 2. User outcomes

The first concrete consumer is an operator or automation that starts a
long-running task and polls it:

1. it creates a durable MAT-19 task;
2. the agent reports a short current status such as "rendering page 4 of 8";
3. the agent publishes a JSON manifest and a PDF report from `/persist`;
4. the caller reads the task, sees current progress and named result metadata;
5. the caller downloads the PDF through an authorized Kyber endpoint; and
6. terminal retention deletes the database metadata and object together.

Other native outcomes include a coding agent publishing a patch plus test
summary, an operations agent publishing diagnostic logs and a chart image, and
a media workflow publishing audio or video outputs. None require A2A at the
native boundary. An A2A adapter can later project the same result parts into
Artifacts without losing type information.

## 3. Current state and reusable boundaries

The bounded request/reply path accepts one string through
`kyber-request-reply.respond`, forwards it over the loopback status sidecar,
and completes one Redis-backed request atomically. Global runtime activity can
say that the harness is working, and transcripts contain incidental output,
but neither is correlated to a durable task or safe as a structured API.

Useful boundaries to retain are:

- the task/request ID supplied in the prompt rather than inferred from text;
- the status sidecar as the only holder of the pod-to-control-plane
  credential;
- loopback MCP tools usable by both Claude Code and Codex;
- strict body, count, and lifetime limits before persistence;
- explicit, idempotent agent reporting rather than transcript scraping; and
- `/persist` allowlist and path-validation techniques already used by the
  Telegram and Discord attachment senders.

Kyber already supports GCS and S3-compatible storage for log and transcript
archives. MAT-20 should reuse provider configuration and streaming client
patterns, not the archive namespace or retention policy. Task objects use a
separate prefix and lifecycle.

## 4. Semantics

### Cooperative progress

Progress is a claim made by the active agent attempt. It means only that the
harness invoked the task update tool with valid data. It is not proof that a
tool call, external write, deployment, payment, or other side effect occurred.

A progress update contains:

- `task_id` and the current execution `attempt_id`;
- a short `message` suitable for callers and operator UI;
- optional `percent` from 0 through 100; and
- an idempotency `update_id` generated for that report.

Percent is optional and not required to be monotonic because an agent may
discover more work. Kyber validates its range but does not turn it into a task
state. A task remains `dispatched` until explicit completion or failure.
Clients display the message, percent when present, and its timestamp as
cooperative status.

Version one retains only the latest accepted progress document on the task
plus a bounded append-only update record needed for audit and the later G7
event stream. Polling is the only public delivery mechanism in MAT-20. SSE,
resume cursors, and subscription retention belong to MAT-25.

### Results and parts

A task has zero or more immutable named results. Each result has a stable
`result_id`, caller-facing `name`, optional description, creation timestamp,
and one or more ordered parts. Part kinds in v1 are:

| Kind | Stored value | Typical use |
| --- | --- | --- |
| `text` | UTF-8 string | summary, markdown, patch, explanation |
| `json` | validated JSON value | manifest, metrics, structured automation output |
| `file` | Kyber object reference and metadata | image, audio, video, PDF, archive, arbitrary bounded file |

Results are immutable after publication. An identical retry of the same
`result_id` and content digest succeeds; different content conflicts. Agents
publish a replacement under a new result ID and name/version rather than
mutating bytes already observed by a caller. Completion does not rewrite
results: it atomically marks the task terminal after all referenced results are
durable.

Publishing a result while a task is `dispatched` is allowed. A result may be
visible before terminal completion, enabling polling clients to retrieve
useful partial outputs. No publication is accepted once the task is terminal.

## 5. Native API shape

MAT-19 Create and List remain unchanged. Get adds progress and result metadata:

```json
{
  "id": "task_...",
  "state": "dispatched",
  "progress": {
    "message": "rendering page 4 of 8",
    "percent": 50,
    "updatedAt": "2026-08-30T18:30:00Z"
  },
  "results": [
    {
      "id": "result_...",
      "name": "report",
      "description": "Final customer report",
      "parts": [
        {"kind": "json", "value": {"pages": 8}},
        {
          "kind": "file",
          "file": {
            "filename": "report.pdf",
            "mediaType": "application/pdf",
            "size": 481203,
            "sha256": "...",
            "downloadPath": "/api/v1/agents/acme/tasks/task_.../results/result_.../parts/1/content"
          }
        }
      ]
    }
  ]
}
```

`downloadPath` is an authorized Kyber route, not an object-store URL. The
server rechecks task visibility on every request, applies range and content
headers, and streams the object. A deployment may later redirect to a
short-lived signed URL after authorization, but no durable signed or remote URL
is stored in the result.

List returns only bounded result summaries: result ID, name, part kinds, total
bytes, and count. It does not inline text, JSON, descriptions, or download
paths. Get returns inline parts up to the task response limit and paginates
results if the configured count grows beyond the default page.

## 6. Runtime MCP and internal routes

Expose one `kyber-task` loopback MCP server through the unconditional status
sidecar, with five tools:

```text
report_progress(task_id, attempt_id, update_id, message, percent?)
publish_text(task_id, attempt_id, result_id, name, text, description?)
publish_json(task_id, attempt_id, result_id, name, value, description?)
publish_file(task_id, attempt_id, result_id, name, path, filename?, media_type?, description?)
complete(task_id, attempt_id, completion_id, summary?)
```

Separate tools are intentional model ergonomics. Their schemas are smaller and
make it difficult to confuse a local path with a public URL or JSON string.
They all map into one native result model and internal service.

For text, JSON, progress, and completion, the sidecar validates basic size and
shape, then forwards JSON to an authenticated internal route. The control
plane repeats all validation and owns state transitions.

For a file, the sidecar:

1. cleans and resolves the supplied path beneath an installation-configured
   allowlist whose default is `/persist/task-results`;
2. rejects symlinks, devices, sockets, directories, hard-link/path escapes,
   and files that change identity during open;
3. opens the file read-only, checks the compiled size cap, and never returns
   its path to the caller;
4. streams it to the internal control-plane upload route while computing
   SHA-256, byte count, and a bounded media sniff; and
5. reports success only after object finalization and metadata commit.

The sidecar may send a declared media type and filename as metadata. The
control plane verifies filename safety, reconciles declared type with sniffed
content where possible, and treats `application/octet-stream` as the safe
fallback. The agent cannot choose an object key, bucket, download URL,
retention, owner, or response headers.

All mutations require the current random `attempt_id` introduced by MAT-19.
The internal API binds the pod token to the task's agent and rejects stale
attempts. Each update/result/completion ID is independently idempotent.

## 7. File upload transaction

Use deterministic object keys that disclose no user filename:

```text
task-results/<namespace>/<agent>/<task-id>/<result-id>/<part-id>
```

The upload transaction is recoverable rather than pretending PostgreSQL and
object storage share one atomic commit:

1. lock the task and validate state, agent, attempt, quotas, and idempotency;
2. insert a `pending` object row with deterministic key and expiry;
3. stream to a temporary object key with a hard byte limit and digest;
4. server-side finalize or copy to the deterministic key;
5. in one database transaction, mark the object `ready`, insert immutable
   result/part metadata, increment task version, and append an update record;
6. delete the temporary object asynchronously; and
7. return the canonical result metadata.

An identical retry first reads by `result_id`; if the committed digest and all
metadata match, it returns success without uploading again. A differing retry
returns `409 result_conflict`. A retry finding a stale `pending` row may resume
or replace the temporary upload under a lease. It never exposes a pending
part.

Reconcilers delete expired temporary objects, pending rows whose leases died,
and orphaned deterministic objects. Database deletion is the source of truth
for visibility; object deletion retries until successful. Metrics and alerts
cover orphan age and cleanup failure.

## 8. Persistence model

Extend MAT-19 with normalized tables. Exact migration names may change, but
the constraints do not:

```sql
ALTER TABLE agent_tasks
  ADD COLUMN progress_message TEXT NOT NULL DEFAULT '',
  ADD COLUMN progress_percent SMALLINT,
  ADD COLUMN progress_updated_at TIMESTAMPTZ,
  ADD CONSTRAINT progress_percent_range
    CHECK (progress_percent IS NULL OR progress_percent BETWEEN 0 AND 100);

CREATE TABLE agent_task_results (
  id               TEXT PRIMARY KEY,
  task_id          TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  description      TEXT NOT NULL DEFAULT '',
  content_digest   TEXT NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL,
  UNIQUE (task_id, name)
);

CREATE TABLE agent_task_result_parts (
  id               TEXT PRIMARY KEY,
  result_id        TEXT NOT NULL REFERENCES agent_task_results(id) ON DELETE CASCADE,
  ordinal          INTEGER NOT NULL,
  kind             TEXT NOT NULL CHECK (kind IN ('text', 'json', 'file')),
  text_value       TEXT,
  json_value       JSONB,
  object_id        TEXT,
  UNIQUE (result_id, ordinal)
);

CREATE TABLE agent_task_objects (
  id               TEXT PRIMARY KEY,
  task_id          TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  object_key       TEXT NOT NULL UNIQUE,
  status           TEXT NOT NULL CHECK (status IN ('pending', 'ready', 'deleting')),
  filename         TEXT NOT NULL,
  media_type       TEXT NOT NULL,
  size_bytes       BIGINT,
  sha256           TEXT,
  lease_until      TIMESTAMPTZ,
  retain_until     TIMESTAMPTZ NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL,
  ready_at         TIMESTAMPTZ
);

CREATE TABLE agent_task_updates (
  task_id          TEXT NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
  sequence         BIGINT NOT NULL,
  update_id        TEXT NOT NULL,
  kind             TEXT NOT NULL CHECK (kind IN ('progress', 'result', 'completed')),
  safe_summary     JSONB NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (task_id, sequence),
  UNIQUE (task_id, update_id)
);
```

`safe_summary` contains no inline file bytes, filesystem path, hidden output,
or prompt. It seeds MAT-25's ordered event log but MAT-20 exposes no stream.
JSON uses PostgreSQL `JSONB` only after a streaming decoder rejects duplicate
keys, excessive depth, excessive token count, and non-finite numbers.

## 9. Limits

Recommended defaults and compiled maxima:

| Limit | Default | Compiled maximum |
| --- | ---: | ---: |
| Progress message | 1 KiB | 4 KiB |
| Progress updates per task | 200 | 2,000 |
| Results per task | 16 | 64 |
| Parts per result | 8 | 32 |
| Inline text part | 32 KiB | 128 KiB |
| Inline JSON part | 64 KiB | 256 KiB |
| File part | 25 MiB | 100 MiB |
| Total file bytes per task | 100 MiB | 500 MiB |
| Filename | 255 UTF-8 bytes | 255 UTF-8 bytes |
| Result name/description | 128 B / 1 KiB | 256 B / 4 KiB |
| JSON depth | 32 | 64 |

Installations may lower defaults. Raising them above compiled maxima requires a
new release and review of memory, proxy, database, object-store, malware, and
egress behavior. Upload and download paths stream; they never buffer a whole
file in the sidecar or control plane.

When a cap is hit, the mutation fails with a stable safe code and leaves the
task active so the agent can publish a smaller result or explicitly fail it.
Kyber does not silently truncate structured results or files.

## 10. Security, privacy, and content handling

- Reuse MAT-19 task visibility for progress, metadata, and downloads. MAT-23
  replaces provisional agent-level scopes with task ownership and explicit
  result-read permissions.
- The pod token can mutate only tasks assigned to its own agent and only with
  the current attempt token.
- Object storage is private. Runtime containers and status sidecars receive no
  bucket credentials; uploads pass through the authenticated control plane.
- Never fetch an agent-supplied URL. Importing remote content requires a later
  allowlisted fetch service with SSRF, redirect, DNS-rebinding, size, and
  egress controls.
- Never expose or store the submitted `/persist` path as task metadata, logs,
  traces, or MCP output.
- Sanitize filenames for headers, strip path components and control
  characters, and emit RFC-compliant `Content-Disposition`.
- Default downloads to `attachment`. Inline preview is limited to an explicit
  safe media allowlist and a sandboxed PWA origin; HTML, SVG, scripts, and
  unknown types are never active content.
- Validate claimed media type against bounded sniffing but do not claim that
  MIME detection proves content safety.
- Support an installation-configured scanner/quarantine adapter before an
  object becomes downloadable. Without one, metadata explicitly reports
  `scanStatus: not_configured`; Kyber never labels it clean.
- Audit safe mutation metadata: task/result/part IDs, agent, caller, byte count,
  digest, media type, outcome, and timestamps. Do not log content, paths, or
  signed URLs.
- Encrypt database/object transport and backups according to installation
  policy. Object retention follows the task; legal hold and permanent archive
  remain out of scope.

## 11. Retention and deletion

Every object inherits the parent task's `retain_until`. Extending task
retention extends ready object metadata transactionally and updates object
lifecycle where required. Shortening retention is allowed only within the
same authorization rules as deleting the task.

At expiry, the database transaction marks objects `deleting` and removes task
visibility. A bounded worker deletes objects idempotently, then removes rows.
If object storage is unavailable, callers still cannot retrieve expired data;
cleanup retries with backoff and alerts. Bucket lifecycle is a defense-in-depth
upper bound, not the only deletion mechanism.

Deleting a non-terminal task is out of MAT-20. Cancellation is MAT-21. An
operator's existing administrative deletion path may remove a terminal task
and its objects using the same reconciler.

## 12. Failure and concurrency matrix

| Failure or race | Required behavior |
| --- | --- |
| Duplicate progress `update_id`, same body | Return prior success; do not append twice |
| Duplicate progress ID, different body | `409 update_conflict` |
| Stale pod or attempt publishes | Reject without writing object or metadata |
| Progress races completion | Row lock orders them; update after terminal loses |
| File changes while being read | Fail upload; do not publish mismatched metadata |
| Sidecar dies during upload | Pending lease expires; temp object is reclaimed |
| Object write succeeds, DB commit fails | Object remains invisible and is reclaimed |
| DB result commits, response is lost | Retry by result ID returns canonical result |
| Object store unavailable | File publication fails safely; text/JSON may continue |
| Control plane restarts mid-upload | Client retries; pending lease/idempotency reconciles |
| Caller loses download authorization | Subsequent download is denied immediately |
| Retention expires during download | Authorization is checked before stream; no new stream starts |
| Task reaches byte/count quota | Reject new part without corrupting prior results |

## 13. Compatibility and A2A projection

MAT-19's compatibility `response` becomes a synthesized text result named
`response`. During migration, completion with only a legacy response creates
that result transactionally. Legacy Get continues returning the response
string when and only when the result contains one text part; multimodal tasks
require the native task API.

The A2A edge maps native results without owning their storage:

- native result -> A2A Artifact;
- text -> TextPart;
- JSON -> DataPart;
- file metadata plus authorized fetch mediation -> FilePart or supported A2A
  file reference; and
- progress message -> task status Message.

The adapter advertises only modes Kyber actually supports. It does not expose
raw Kyber download paths to an unauthorized A2A caller or accept arbitrary A2A
URLs into native storage.

## 14. Rollout and rollback

1. Add repository migrations and read-only result fields behind a feature
   gate; keep legacy response behavior.
2. Add progress and text/JSON internal mutations plus MCP tools.
3. Configure the separate task-object prefix and validate streaming upload,
   authorization, range download, retention, and orphan cleanup.
4. Enable file publication for purpose-built agents and exercise Claude Code
   and Codex with image, PDF, audio, and unknown binary fixtures.
5. Enable public result metadata/downloads, then migrate the legacy response
   into the synthesized result.

Rollback first disables new mutations and file uploads while preserving reads
and cleanup. Schema and objects remain until all retained tasks expire. Never
roll back by deleting result tables or buckets while visible tasks reference
them.

## 15. Test strategy

- repository tests for atomic task/update/result transitions, sequence
  allocation, attempt checks, idempotency, conflicts, quotas, and retention;
- JSON fuzz/property tests for depth, duplicate keys, numbers, Unicode, and
  canonical digest stability;
- path and file tests for traversal, symlinks, hard links, FIFOs, devices,
  rename/change races, sparse files, and cap enforcement;
- object-store contract tests against GCS and S3/MinIO for streaming upload,
  range download, metadata, copy/finalize, outage, and cleanup;
- authorization tests for cross-agent, cross-caller, expired, stale-attempt,
  and revoked access;
- crash tests at every upload transaction cut point in section 12;
- both-runtime tests proving Claude Code and Codex can report progress and
  publish text, JSON, image, document, and generic file results through the
  same MCP schemas; and
- A2A adapter tests later, using native fixtures rather than a second artifact
  store.

## 16. Cost and non-goals

Including controlled object-backed files raises the implementation estimate
from the original two to three weeks to approximately four to six engineer
weeks after MAT-19's durable task foundation:

- one to two weeks for progress, text/JSON, schemas, MCP, and APIs;
- two to three weeks for safe streaming files, provider backends, downloads,
  cleanup, and security tests; and
- one week for runtime, migration, rollout, and failure testing.

Out of scope:

- cancellation (MAT-21);
- multi-turn input/auth-required flows (MAT-22);
- final principal ownership policy (MAT-23);
- streaming/resumable subscriptions (MAT-25);
- caller webhooks;
- arbitrary URL ingestion, public buckets, unbounded inline bytes, mutable
  artifacts, or filesystem browsing; and
- implementation in this design issue.

## 17. Recommendation

Approve MAT-20 with files in v1. The result model is useful to Kyber without
A2A, matches the existing harness/tool model, and projects cleanly into A2A
Artifacts. The necessary platform work is deliberately narrow: task-scoped
validation, durable metadata, private object storage, authorized retrieval,
and lifecycle cleanup. Claude Code and Codex continue to do the content work.
