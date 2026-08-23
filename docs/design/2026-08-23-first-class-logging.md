# First-class logging

Status: approved for implementation on 2026-08-23  
Issue: [#105](https://github.com/matty-v/kyber/issues/105)

## Problem

Kyber logging is currently a collection of component-local choices. Go binaries
mix `log` and `slog`, output is mostly unstructured text, verbosity configuration
is limited to the status sidecar, and the API/PWA can only discover a hard-coded
subset of agent and node-agent containers. Durable archive support is likewise
agent-specific. Operators must use `kubectl` for the control plane, channel
sidecars, storage pods, jobs, and new workload types.

Kyber needs one logging contract that covers every pod it manages, makes settings
and effective values visible, supports live and durable reads plus export, and is
consumable by standard Kubernetes log collectors without Kyber-specific parsing.
Agent transcript/session history remains a separate pipeline and is out of scope.

## Decisions

### Output contract

Kyber-owned Go processes write one JSON object per line to stdout or stderr using
the standard library's `log/slog`. The shared logger emits these base fields:

| Field | Meaning |
|---|---|
| `time` | RFC3339Nano UTC event time |
| `level` | lowercase `debug`, `info`, `warn`, or `error` |
| `msg` | concise human-readable event description |
| `component` | stable Kyber component name |

Context-specific fields are additive: `agent`, `machine`, `pod`, `container`,
`namespace`, `request_id`, and domain identifiers such as `binding`. Empty fields
are omitted. Field names use `snake_case`. Errors use `error`. Messages do not
repeat the component name or encode severity.

Third-party images such as Postgres, Redis, MinIO, and Vector keep their native
output. Kyber cannot honestly promise to rewrite it. The archive envelope supplies
uniform Kubernetes identity and timestamps around every line, while `format`
identifies `json` versus `text`. This preserves complete pod coverage without a
fragile parser for each dependency.

### Settings and precedence

V1 exposes verbosity at two useful levels:

1. `logging.level` is the global default (`info`).
2. `logging.components.<component>.level` overrides the global value.

Per-agent overrides are deferred. They complicate reconciliation and pod rolls,
make incidents harder to compare, and provide little value beyond component-level
debugging. Settings are declarative Helm values. A chart change rolls affected
workloads; agent pod-builder inputs participate in existing drift reconciliation.

The API exposes desired and effective levels read-only. The PWA settings surface
shows the global value, component overrides, effective value per component, and
whether a restart/roll is required. Runtime mutation through the API is deferred
until Kyber has a durable cluster-configuration store; silently mutating Helm-owned
configuration would create drift that ArgoCD later reverses.

Third-party containers report an effective level only where their image supports
a mapped verbosity setting; otherwise the API reports `unmanaged`.

### Workload identity and discovery

Every Kyber-managed pod template carries:

- `app.kubernetes.io/part-of: kyber`
- `app.kubernetes.io/component: <stable-component>`
- existing resource labels such as `kyber.io/agent` or `kyber.io/machine`

The control plane lists only pods in its configured namespace with
`app.kubernetes.io/part-of=kyber`. It discovers init and regular containers from
the live Pod spec. This is the default-on contract for future pod types: adding a
Kyber workload through the chart or controller makes it visible without updating
an API allowlist. Pod UID, not just pod name, distinguishes recreated instances.

### Storage and retention

There are three deliberately separate tiers:

1. **Live node storage.** The container runtime writes stdout/stderr under
   `/var/log/pods`. The kubelet owns rotation and retention. Kyber proxies this
   source and labels it ephemeral; it does not duplicate or prune it.
2. **Shipper working state.** The node-level Vector DaemonSet keeps checkpoints
   and a bounded 256 MiB disk buffer in `/var/lib/kyber-log-shipper`. This is
   transport state, not operator-visible history. Acknowledged data is reclaimed
   by Vector.
3. **Durable archive.** Vector writes NDJSON to GCS or an S3-compatible store.
   The object store is the durable source of truth. Its native lifecycle policy
   expires objects after `logging.archive.retentionDays`, default 30. Kyber does
   not run a second deletion scheduler.

The generic archive key is:

```text
logs/<component>/<workload>/<pod-uid>/<container>/<YYYY-MM-DD>/<object>.ndjson
```

Each archive record contains the normalized envelope fields plus the original
line in `message`. `workload` is a stable logical identity (for example an agent
name, machine name, Deployment, DaemonSet, StatefulSet, or Job owner). Values are
sanitized path segments. Date partitioning keeps window reads bounded.

The existing `agents/` and `transcripts/` roots remain readable during migration.
New platform logging never writes transcript content to `logs/`; the transcript
lane retains its independent retention and authorization contract.

GCS, AWS S3, and MinIO lifecycle configuration must all honor the same retention
value. For an operator-supplied S3 bucket, Kyber documents and reports the desired
policy but cannot assume permission to mutate bucket lifecycle rules.

### API

V1 adds resource-neutral endpoints:

```text
GET /api/v1/logging/settings
GET /api/v1/logging/targets
GET /api/v1/logging/logs
GET /api/v1/logging/export
```

Targets return stable workload identity, current pod instances, containers,
effective verbosity, supported sources, and live/archive availability. Log reads
identify one pod/container and source. Archive reads also accept an absolute
RFC3339 `since`/`until` window. Live reads retain `follow`, `tail`, `since`, and
`previous` semantics where Kubernetes supports them.

The viewer response remains bounded by line count, scanned bytes, and aggregate
concurrency. It signals truncation explicitly. Export shares selection and
authorization logic but streams object-by-object and line-by-line with a larger,
separate byte ceiling; it never assembles the result in control-plane memory.
Exports are one target/container at a time and set `Content-Disposition` with a
deterministic filename. Supported formats are:

- `ndjson` (default): preserves the normalized envelope for tooling.
- `text`: emits the original message field for human convenience.

An export exceeding its configured window or byte ceiling is rejected before
streaming where determinable. If an upstream limit is reached after streaming
begins, the response includes an explicit terminal NDJSON metadata record (or a
text marker) and the API emits a truncation trailer. Silent partial files are not
allowed. Concrete defaults will be selected from measured production log volume
and covered by load tests rather than guessed in this document.

Existing agent/machine log URLs stay functional as compatibility adapters until
the generic surface has shipped and callers migrate.

### PWA

A fleet-level Logs page provides:

- component/workload/pod/container selectors;
- live versus archive source selection;
- follow, pause, and absolute time-window controls;
- visible source retention and effective verbosity;
- truncation/error state; and
- NDJSON or text download for the selected target and window.

Resource detail pages may deep-link to the fleet Logs page. They do not maintain
a second implementation of discovery or export.

### Aggregation readiness

Kyber does not ship or operate Datadog, Loki, CloudWatch, or an OTel Collector as
part of this feature. JSON-per-line stdout, stable Kubernetes labels, normalized
identity fields, and documented multiline behavior are the integration contract.
Standard Kubernetes collectors can select `app.kubernetes.io/part-of=kyber` and
forward records without a Kyber-specific agent or API.

## Security and failure boundaries

- Discovery and reads are namespace-bound and require the existing authenticated
  operator API. Arbitrary namespace, pod, container, and object prefixes are not
  accepted from callers.
- The API resolves caller selections against discovered Kyber-managed pods before
  requesting kubelet logs.
- Archive keys are constructed from resolved target identity, never concatenated
  from unchecked query strings.
- Public errors do not include kubelet, object-store, or internal path details.
- Export/view concurrency and byte limits protect the control-plane monolith.
- Vector buffers object-store failure locally and cannot backpressure application
  containers. Losing a node before buffered data ships is an acknowledged gap.
- Log messages must not contain credentials or full request bodies. Structured
  logging does not make secret-bearing data safe.

## Compatibility and rollout

The rollout is additive first:

1. Add labels and shared logging without removing current routes or archive roots.
2. Add generic target discovery and live reads.
3. Expand Vector selection/archive layout and add generic archive reads.
4. Add export and the fleet PWA.
5. Migrate detail views and only then consider deprecating legacy routes.

Mixed-version archives are expected during rollout. Readers tolerate both native
JSON Kyber messages and third-party text wrapped in the normalized envelope.

## Execution plan

Each checkpoint is independently testable and is committed and pushed before the
next begins.

- [x] **Checkpoint 0 — investigation and design.** Inventory current logging,
  storage, settings, API/PWA behavior, and agree this design.
- [x] **Checkpoint 1 — shared logging foundation.** Add a small logging package,
  standard fields/level parsing, tests, and migrate representative binaries before
  mechanically moving the remainder.
- [x] **Checkpoint 2 — chart settings and identity.** Add global/component Helm
  values, inject effective levels and downward-API context, standardize managed-pod
  labels, and cover every rendered workload with Helm tests.
- [x] **Checkpoint 3 — settings and target APIs.** Implement read-only effective
  settings and label-based pod/container discovery; update OpenAPI and PWA wire
  types; add authorization and API tests.
- [ ] **Checkpoint 4 — generic live reads.** Add the bounded resource-neutral
  kubelet stream while preserving legacy agent/machine routes.
- [ ] **Checkpoint 5 — generic durable archive.** Expand Vector to all managed
  pods, introduce the `logs/` envelope/key contract and compatible reader, and
  align GCS/S3/MinIO lifecycle configuration.
- [ ] **Checkpoint 6 — export.** Add streaming NDJSON/text export, measured limits,
  content disposition, truncation contract, and memory/concurrency tests.
- [ ] **Checkpoint 7 — PWA.** Build the fleet Logs page and settings visibility,
  deep-link resource views, bump `pwa-views`, and add component/API-client tests.
- [ ] **Checkpoint 8 — migration docs and acceptance.** Document collector recipes,
  retention/failure behavior, and verify every Helm-rendered pod/container is
  discoverable, viewable, archived when enabled, and exportable.

## Acceptance criteria

- Every pod/container rendered or created by Kyber participates by label and is
  discoverable without a hard-coded container allowlist.
- Kyber-owned Go logs conform to the JSON schema and level settings.
- Operators can see desired/effective settings and retention in the PWA/API.
- Live and durable reads remain bounded and never expose another target's data.
- The selected window can be exported as valid NDJSON or text without buffering
  the complete file in control-plane memory.
- Object-store lifecycle policies bound durable retention; kubelet and Vector
  local behavior are documented distinctly.
- Existing agent/machine log and transcript surfaces remain compatible through
  migration.
- Standard Kubernetes collectors can select and parse Kyber output using only the
  documented labels/schema.
