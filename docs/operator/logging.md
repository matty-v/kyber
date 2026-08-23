# Operating Kyber logs

Kyber exposes one logging boundary for every platform-managed pod:

```text
app.kubernetes.io/part-of=kyber
```

Open **Logs** in the PWA to discover current components, workloads, pods, and
containers. The same authenticated surface is available under
`/api/v1/logging/*`; callers never submit an arbitrary namespace or object-store
prefix. Agent and machine detail pages link into this fleet view.

## Configure verbosity and retention

Logging is deployment-managed. Set the global level and optional component
overrides in Helm, then upgrade the release:

```yaml
logging:
  level: info
  components:
    control-plane:
      level: debug
  archive:
    retentionDays: 30
```

Valid levels are `debug`, `info`, `warn`, and `error`. The PWA and
`GET /api/v1/logging/settings` show the desired settings; target discovery shows
the effective level of each managed container. Third-party containers retain
their native logging configuration and display as `unmanaged`.

`retentionDays` is the durable object-store target, not a promise about the
kubelet ring buffer. GCS installations enforce it through bucket lifecycle.
Chart-managed MinIO installs apply the same expiry to the bucket. For an
operator-supplied AWS S3 bucket, apply an equivalent expiration policy to all
three prefixes: `logs/`, `agents/`, and `transcripts/`.

## Storage and failure behavior

There are three distinct tiers:

| Tier | Purpose | Retention/failure behavior |
|---|---|---|
| Kubelet | Current pod stdout/stderr and follow | Node/runtime managed and normally short-lived; lost with node log rotation or node loss. |
| Vector buffer | Delivery working state | Node-local disk checkpoints survive shipper restarts. Object-store slowness fills this buffer but never backpressures application containers. Unshipped data can be lost with the node. |
| Object store | Durable archive and export | Lifecycle-bounded by `logging.archive.retentionDays`; remains readable across pod replacement. |

An archive outage does not change the live path. Archive reads return an error;
Vector continues buffering up to its configured disk limit. Log records can
contain operator or workload data, so object-store read access should be as
restricted as control-plane operator access. Never log credentials or complete
request bodies.

## External collector recipes

Kyber does not install or operate a third-party aggregation service. Configure
your existing Kubernetes collector to keep pods whose Kubernetes label
`app.kubernetes.io/part-of` equals `kyber`. Do not select only a fixed list of
container names: new managed components inherit the platform boundary.

Kyber-owned binaries emit one JSON object per line. Parse JSON when possible and
retain native text in a `message` field when parsing fails. The stable fields
are `timestamp`, `level`, `component`, `message`, `namespace`, `pod`, and
`container`; workload-specific fields may be added. Kubernetes metadata remains
authoritative for routing.

Example Vector transform after a normal `kubernetes_logs` source:

```yaml
transforms:
  kyber_only:
    type: filter
    inputs: [kubernetes_logs]
    condition: '.kubernetes.pod_labels."app.kubernetes.io/part-of" == "kyber"'
  kyber_json:
    type: remap
    inputs: [kyber_only]
    source: |
      parsed, err = parse_json(.message)
      if err == null { . = merge(., parsed) }
```

For Fluent Bit, use the Kubernetes filter to enrich records, then a `grep`
filter matching `$kubernetes['labels']['app.kubernetes.io/part-of']` to `^kyber$`
and the JSON parser on `log`. For Grafana Alloy, Datadog, or another managed
collector, apply the same label selection and JSON-first/text-fallback rule in
that product's Kubernetes integration.

## API examples

First resolve an exact current pod UID and container from discovery:

```sh
curl -H "Authorization: Bearer $KYBER_API_KEY" \
  https://kyber.example/api/v1/logging/targets
```

Then use the returned `pod`, `podUid`, and container `name`. Live reads accept a
bounded `tail` and optional `follow=true`. Archive reads require an absolute
RFC3339 `since`/`until` window. Exports use the same identity and window with
`format=ndjson` or `format=text`. A stale pod UID is rejected rather than
silently reading a replacement pod.

The viewer caps retained browser lines, and the server separately bounds live
reads and exports. When a cap is reached, the UI shows truncation and exports
include an explicit terminal marker; partial output is never silent.

Legacy `/api/v1/agents/{name}/logs`, `/api/v1/machines/{name}/logs`, and agent
transcript reads remain available during migration.
