# Operator runbook — internal-API auth rollout & NetworkPolicy (kyber#566)

> Who may call the internal API (`:8082`), what identity is required, the
> default-deny posture, and the **CNI-enforcement** precondition. Architecture
> deep-dive: [`../architecture/internal-api-auth.md`](../architecture/internal-api-auth.md).

## What this controls

The internal API (`:8082`) authenticates every caller with a control-plane-signed
**pod-token** and enforces **act-on-self-only**: an agent may act only on its own
resources; the node-agent identity is the only caller admitted to the
machine/node routes. This is the load-bearing control. A `:8082`-scoped
**NetworkPolicy** is a second, defense-in-depth layer.

## Who may call `:8082`

| Caller | Identity presented | Admitted to |
|---|---|---|
| An agent's own pod / sidecar / init container | `<agent-name>` (mounted `<name>-pod-token`) | `/internal/agents/<agent-name>/*` only |
| node-agent DaemonSet | `node-agent` (`kyber-node-agent-token`) | `/internal/machines/*`, `/internal/nodes/*` only |
| anything else | — | nothing (401 unauthenticated, or 403 cross-identity) |

## One-time delivery — the signing key

The signing key is **not** in the chart (never commit key material). Create it
out-of-band (sealed-secret / kv-secrets) before enabling enforcement:

```bash
kubectl -n kyber-system create secret generic kyber-internal-signing-key \
  --from-literal=signing-key="$(openssl rand -hex 32)"
```

The control-plane env reads it with `optional: true`, so the pod still starts
before the key lands. **If the key is absent the internal API FAILS CLOSED** —
every `:8082` route returns `503`, and main.go logs a loud error
(`KYBER_INTERNAL_SIGNING_KEY is empty — internal API (:8082) FAILING CLOSED …`).
This is deliberate (Matt's security call): serving `:8082` unauthenticated on a
missing key would silently re-open the agent→agent hole this whole change closes,
so a misconfigured deploy refuses the internal API rather than opening it. The
gate is **scoped to `:8082`** — the control plane's other surfaces (public API,
health, metrics, controllers) keep running, so the process does not crashloop;
agent telemetry / OAuth rotation / brief fetch on `:8082` are simply refused
until the key is delivered. Confirm the error log line is **absent** (and a
test agent's sidecar telemetry flows) after delivery.

> **kyber#578 — fail-closed now PAGES, and it fails closed even under grace.**
> After the v2.1.0 incident (enforce shipped, key never delivered → the whole
> internal API 503'd silently for ~2h → fleet outage), a keyless startup raises a
> **one-shot alert** through the control-plane `alertSink` (LogAlertSink, plus the
> webhook sink when `KYBER_ALERT_WEBHOOK_URL` is set — provisioned by kyber#586):
> `InternalAuthFailClosed` (critical) when enforce, `InternalAuthGraceNoKey`
> (warning) when grace. The alert names the `kyber-internal-signing-key` Secret
> and the impact (`/internal/...` 503s); it never carries the key value, and it
> fires on the startup *condition* (not per request), so it cannot flood. **A
> missing key fails closed even in grace mode** (the conservative posture, Matt's
> Q1=NO) — grace never opens an unauthenticated window; it only changes behavior
> when the key *is* present (accepts unauthenticated stragglers while always
> blocking cross-identity 403).

> Tradeoff note: the gate is the `:8082` `503` rather than a whole-pod readiness
> fail, so the public API stays reachable while the key is missing (per "keep
> other routes available if feasible"). The misconfiguration surfaces via the
> startup error log + per-request `503`s + agents visibly failing on `:8082`,
> not via `kubectl get pods` showing the pod NotReady.

## Rollout sequence (GRACE-FIRST + key-gated — kyber#578)

A **new** internal-auth rollout defaults to `internalAuth.graceMode: true`
(grace-first). The cutover to enforce is a **two-step, verified** sequence — never
a single flip that fail-closes the whole internal API if the key is missing
(the v2.1.0 outage). Run the deploy gate at each step:

```bash
# Pre-apply gate — aborts a keyless ENFORCE cutover before it reaches the cluster.
scripts/preflight-internal-auth-key.sh <namespace> <graceMode:true|false>
# Post-apply check — confirms the control plane actually enabled auth.
scripts/preflight-internal-auth-key.sh <namespace> <graceMode:true|false> \
  --post-apply kyber-control-plane
```

1. **Deliver the signing-key Secret** (above).
2. **Release N — grace** (`internalAuth.graceMode: true`, the default for a fresh
   rollout). The pre-apply gate warns-but-proceeds if the key is somehow still
   absent (it will fail-closed + alert, paged not silent). The API accepts-and-logs
   *unauthenticated* stragglers (never softening a cross-identity 403) while the
   controller re-rolls every agent pod onto its mounted token. Confirm the
   post-apply check passes (control plane logged `internal API per-agent auth
   enabled`).
3. **Release N+1 — enforce** (`internalAuth.graceMode: false`), an **explicit,
   key-verified flip** (never an auto-flip). The pre-apply gate **ABORTS** if the
   signing-key Secret is absent in the target namespace. After apply, the
   post-apply check must pass. Confirm every agent pod has its `<name>-pod-token`
   mounted and `kyber-node-agent-token` exists.

> **Back-compat:** a cluster that already pins `graceMode` explicitly (the existing
> enforce clusters) is unchanged — only an unset/fresh rollout picks up the
> grace-first default.

### Responding to the fail-closed alert

If `InternalAuthFailClosed` (critical) or `InternalAuthGraceNoKey` (warning) pages:
the `kyber-internal-signing-key` Secret is not delivered (or not picked up), so
`:8082` is 503'ing. **Deliver the Secret** (the `kubectl create secret` above),
**restart the control plane**, and confirm the post-apply check passes. If agents
already cascaded to `NeedsAuth` on expired tokens, re-authorize them after `:8082`
is healthy.

## NetworkPolicy — enable + verify per target

`networkPolicy.enabled: true` renders the `:8082`-scoped policy. It only bites if
**the CNI enforces NetworkPolicy** — verify empirically per target; an unenforced
policy is security theater.

- **k3s (razer/falcon):** the bundled NP controller is active **unless**
  `--disable-network-policy` is set. Confirm it is **not** disabled.
- **GKE (gcp):** requires Dataplane V2 or Calico (NetworkPolicy enabled on the
  cluster) or the policy is inert.

The **hostNetwork node-agent** is not selectable by podSelector (its traffic comes
from the node IP). Admit it via the node/pod CIDR(s):

```yaml
networkPolicy:
  internalApi:
    allowedNodeCIDRs: ["10.42.0.0/16"]   # e.g. k3s default pod CIDR — set per target
```

Empty `allowedNodeCIDRs` **and** an enforcing CNI ⇒ the node-agent's `:8082`
calls are dropped (agent-pod auth is unaffected). The authz control does not
depend on the NetworkPolicy.

## Verification (capability, not "pods up")

- An agent's own sidecar still posts telemetry / reads its brief / rotates its
  token; the node-agent still reports.
- **Negative (the C1 proof):** from agent A's pod, a crafted call to
  `/internal/agents/B/refresh-token` and `/internal/agents/B/session-brief` is
  rejected (403).
- A non-allowed pod cannot TCP-connect `:8082` (only meaningful where the CNI
  enforces NP).

## Rollback

Set `internalAuth.graceMode: true` (or remove the signing key) to restore
availability without redeploying the image — but that re-opens C1, so prefer
fixing forward.
