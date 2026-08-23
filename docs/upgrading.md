# Upgrading Kyber

How to roll out new features, bug fixes, and schema changes to a running Kyber instance. This document is the source of truth — if a step doesn't match reality, fix the doc.

## Before upgrading past the agent-sandbox release: check your Kubernetes version

Agents now run in a Linux user namespace, and they **refuse to start** without
one (kyber#78). This needs **Kubernetes >= 1.33 and containerd >= 2.0** on every
node that schedules agents.

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,K8S:.status.nodeInfo.kubeletVersion,RUNTIME:.status.nodeInfo.containerRuntimeVersion
```

If any agent-scheduling node is below that, upgrading will stop every agent on
the cluster. The agents fail with a message naming the cause, and the control
plane reports them Failed — nothing is lost, and the volumes are untouched — but
they will not run until you either upgrade the nodes or opt out deliberately:

```yaml
agent:
  security:
    requireUserNamespace: false   # accept unisolated agents, knowingly
```

That is a real choice, not a formality: without a user namespace an agent's
in-pod root and `CAP_SYS_ADMIN` are valid against the node. Kyber will not make
that choice silently on your behalf, which is why the default is to stop rather
than to continue.

**Kyber's own Install button checks this before it acts.** `POST
/api/v1/updates/apply` runs a node preflight and refuses with a `409` naming the
offending nodes rather than installing an upgrade that would stop every agent on
the cluster. The refusal is the same information as the check above, arriving
while it can still be acted on. It does not apply when
`requireUserNamespace=false` — an operator who has accepted unisolated agents
does not need a second opinion — and an unreadable node or an unfamiliar version
string never blocks, because a preflight that becomes its own outage is worse
than the problem it guards against.

The first boot after the upgrade also seeds each agent's durable root from the
base image (~1.3 GB, a minute or two) and migrates any existing overlay upper
layer into it. The old upper layer is left in place, so
`agent.security.persistenceMode=overlay` remains a working rollback.

## Two delivery models

A cluster gets a new version by exactly one of two routes, and which one applies
is a property of **how the cluster was deployed**, not a setting:

| | **Push** (repo-driven) | **Pull** (self-updating) |
|---|---|---|
| Who decides | A release, automatically | **An operator, per cluster** |
| Mechanism | `deploy-bump-pr` writes pinned digests → ArgoCD syncs | Control plane runs `helm upgrade` on itself |
| Image versions | Pinned in the deploy repo's `values.yaml` | **Carried by the chart version** |
| Clusters today | `kyber-falcon` | `kyber-razer` (since 2026-08-13) |

The rest of this section documents the **pull** model — what actually happens when
an operator clicks *Install* in the Kyber UI. The push model is documented under
[Release lanes](#release-lanes) below.

---

## Upgrading from the Kyber UI

### What the operator does

**Settings → Updates.** The card shows the current version, the latest version on
the cluster's channel, and an *Install* button. That is the whole interaction.

> **Status:** the API described below is live. The Settings card ships in
> **kyber#53**; until that merges, the same operations are available over the API
> (`GET /api/v1/updates`, `PUT /api/v1/updates/policy`,
> `POST /api/v1/updates/check`, `POST /api/v1/updates/apply`). Everything else in
> this section describes behaviour that is already in the control plane.

Two things are deliberately **not** offered:

- **There is no automatic apply.** No mode installs an update on its own. The
  policy accepts a maintenance `window`, but nothing reads it — and `mode: auto`
  is *rejected at validation* rather than accepted and ignored, because a setting
  that silently does nothing is worse than one that refuses.
- **There is no "upgrade the fleet" action.** Each cluster is upgraded on its own.

### What happens when they click Install

`POST /api/v1/updates/apply` runs four guards, then creates a Job. **The control
plane does not run the upgrade itself** — it is the process being replaced, and a
process cannot reliably supervise its own termination. The Job survives the
control-plane restart, and **its log is the upgrade record.**

The Job runs the **current** control-plane image with a different entrypoint
(`/usr/local/bin/kyber-upgrade`), so there is no extra image to build or pin. The
target version's image is deliberately *not* used: it doesn't exist on the cluster
yet, and pulling an unverified binary to supervise the upgrade would be backwards.

**Guards, in the order the operator would want them explained:**

1. **Is self-upgrade configured here?** Requires `selfUpgrade.enabled=true`, which
   is what creates the Job's ServiceAccount. Surfaced as `applySupported` so the
   UI shows an honest state instead of a button that 503s.
2. **Is this cluster allowed to self-upgrade?** Kyber reads the control-plane
   Deployment's own annotations. An ArgoCD tracking-id means **refuse** — ArgoCD
   would revert the upgrade on its next sync, so it would appear to work and then
   silently undo itself. Unknown ownership also refuses.
3. **Is the cluster pinned?** A `pinnedVersion` means "do not move".
4. **Is another upgrade in flight?** Two concurrent `helm upgrade`s on one release
   corrupt the release history.

### What the Job does

Seven steps. Steps 1–4 all happen **before the cluster is touched**, so a bad
version, an unreachable registry, or a disqualifying values file fails against a
still-healthy cluster.

1. **Confirm this is a real Helm release** (`helm status`). Note it uses `helm
   upgrade`, **not** `upgrade --install`: on an ArgoCD cluster there is no release
   Secret, and `--install` would cheerfully create one over the top of resources
   ArgoCD owns. A missing release is fatal here by design.
2. **Capture the operator's values to a file.** Explicitly *not* `--reuse-values`,
   which carries values forward invisibly — the Job log and the captured file
   together state exactly what was applied.
3. **Refuse an upgrade that cannot change what runs.** If the values pin any
   `image.*.tag` or `image.*.digest`, the Job **stops**. See
   [Why pinned images are refused](#why-pinned-images-are-refused).
4. **Pull the target chart** from `selfUpgrade.chartRef` at the exact version.
5. **Apply the chart's CRDs.** Fail-closed, and **Helm will not do this** — Helm
   installs `crds/` once and never touches them again, so without this step a
   schema change would never reach the cluster.
6. **`helm upgrade --wait`.** Helm's `--atomic` is deliberately unused: it would
   create a second rollback path with different logging and timeouts. There is one
   rollback path, and it is always Kyber's.
7. **Verify against the running cluster** — not against Helm's opinion of it.

**Verification checks three separate things, because each has been true on these
clusters while another was false:**

- The control-plane Deployment is genuinely rolled out (`observedGeneration`
  caught up, every replica updated and ready).
- **The running container's image tag is the version we asked for**, read from the
  live Deployment. This is the check that catches an upgrade which rewrote the
  templates but left the old images running.
- `/healthz` answers 200 — the new binary is *serving*, not merely scheduled.

**Any failure after the release is written rolls back** to the revision that was
live when the Job started. The Job never retries (`backoffLimit: 0`): a failed
upgrade has already rolled back, so a second attempt would start from a cluster
that is already where it should be.

### How each component actually gets its new image

This is the part that is easy to get wrong, because the components do **not** all
update at the same moment or by the same mechanism.

| Component | How it updates | When |
|---|---|---|
| **Control plane** | Helm workload — replaced by the upgrade | During the Job |
| **Node agent** | Helm workload (DaemonSet) — replaced by the upgrade | During the Job |
| **CRDs** | Applied by the Job, before Helm runs | During the Job |
| **Agent runtime images** (`claude-code`, `codex`) | Controller-injected env var → running pods converge | **After** the Job |
| **Sidecars** (status, telegram, discord) | Controller-injected env var → running pods converge | **After** the Job |

Agent runtime images and sidecars are **not** Helm-deployed. The chart passes them
to the control plane as environment variables (`KYBER_AGENT_RUNTIME_IMAGE`,
`KYBER_CODEX_RUNTIME_IMAGE`, `KYBER_STATUS_SIDECAR_IMAGE`,
`KYBER_TELEGRAM_SIDECAR_IMAGE`, `KYBER_DISCORD_SIDECAR_IMAGE`). The upgrade gives
the *control plane* the new values; **existing agent pods are still running the old
ones** and are converged afterwards by the agent controller.

That convergence is paced, and the two paths differ in one way that matters:

- **Agent runtime image — no idle gate.** A drifted agent is rolled through the
  graceful capture-state-and-delete path, which preserves the agent's state. A
  single-agent install keeps its immediate roll-and-converge behaviour.
- **Status sidecar — idle gate.** A working agent is **never** interrupted for a
  sidecar version skew; the roll is deferred until the agent reports idle. An
  unknown activity state also holds, deliberately.

Both share a **cluster-wide budget of one pod deletion in flight**, across all
causes, so the fleet drains gradually rather than rebooting at once. Both sit
behind a **pullability canary**: one agent proves the new image can actually be
pulled before the rest follow, so a bad digest strands one pod instead of putting
every agent into `ImagePullBackOff`.

> **The practical consequence for an operator: a green upgrade means the CONTROL
> PLANE is on the new version. Your agents may still be on the old runtime — that
> is expected, and it is bounded by agent activity, not by the upgrade.**
>
> Verification does not check agents at all. To confirm the fleet has caught up,
> check the agent pods' images separately.

### Why pinned images are refused

Under the pull model **the chart version *is* the version**: chart `1.0.2` carries
the `v1.0.2` image tags in its own defaults.

A values file that also pins `image.controlPlane.tag` overrides those defaults. So
`helm upgrade` to a new chart would rewrite every template, report success, and
leave every container running the **old** build — the release history would say
1.0.2 while `/api/v1/version` said 1.0.1. That "half-works" outcome is the exact
failure the design exists to prevent, so the Job refuses and names the offending
keys.

This is not hypothetical: it is the shape of every ArgoCD-managed cluster, where
pinned digests are *correct*. Removing the pins is a required part of adopting a
cluster into the pull model, and this guard is what makes that a visible
precondition instead of a silent no-op.

### What it costs

Digest pinning. A pulling cluster pins an immutable release **tag** (`v1.0.2`)
rather than a `tag@sha256:…` pair. That is a smaller step than it looks — the
hazard digests guard against is a *mutable* tag like `:latest`, which is a
different thing from a release tag — but it is a real difference, and it is the
trade the model makes in exchange for the chart version being the single thing
that decides what runs.

---

## Release lanes

> **Superseded model — read this if you remember the old one.** Kyber used to run
> a `latest`/`stable` two-track scheme: a `stable` branch, `:stable` image tags,
> and a `promote-stable.yml` workflow. **None of that exists anymore.** There is no
> `stable` branch, no `:stable` tag, and no promote workflow. Release promotion is
> now semver-tag driven (kyber#591, kyber#449). If you find a doc or script still
> referencing them, it is stale — fix it.

> **Also superseded (2026-08-10): the canary lane.** `kyber-razer` (called
> `kyber-laptop` in older docs) tracked `:latest` and ran head-of-main
> continuously. It moved to the gated release lane
> (kyber#39 / kyber-deploy#139), and `sync-razer-latest.yml` — the */30 cron that
> chased `:latest` digests — is **deleted**. **As of 2026-08-13 it has moved again**,
> off ArgoCD entirely and onto the pull model documented above. `kyber-gcp` is parked with its VM
> terminated and is **out of the release matrix**. There is currently **no cluster
> running head-of-main**; a replacement canary is planned.

Every environment runs a **released chart** and **released images**:

| Lane | Chart source | Image pins | Used by | Advances when |
|---|---|---|---|---|
| **release (push)** | chart version `X.Y.Z` | `vX.Y.Z@sha256:…` | `kyber-falcon` | a semver tag is cut |
| **release (pull)** | chart version `X.Y.Z` | none — the chart version carries them | `kyber-razer` | **an operator installs it** |
| *(parked)* | — | frozen at last release | `kyber-gcp` | not advanced — VM terminated |

Advanced by `release.yml`, which opens and auto-merges a digest-pinned bump PR
against kyber-deploy for each cluster in the matrix. The full merge-to-release
sequence (release proposal → operator approval → `prepare-release.yml` cuts the
tag → `release.yml` builds and promotes) is documented in
[`operator/release-runbook.md`](operator/release-runbook.md) — that doc is the
authority on the release pipeline; this one covers what an operator does around it.

### The chart is a versioned artifact

`release.yml`'s `publish-chart` job pushes the Helm chart to
`oci://ghcr.io/matty-v/charts/kyber:X.Y.Z` on every release, at the same version
as the release itself (`prepare-release.yml` guarantees `Chart.yaml` is bumped
inside the tagged commit, kyber#591).

This closed a real gap. Before it, the chart was published **nowhere** — every
ArgoCD Application rendered it straight from this git repo at
`targetRevision: main` while pinning image tags to a release. Images were gated
and chart *templates* were not, so anything merged here reached every cluster
within minutes, ungated, with `selfHeal` on. `/api/v1/version` made it worse by
reporting `chartVersion` from the image's ldflags: it showed the pinned release
while the rendered templates were head-of-main.

Consequences to know:

- Cluster Applications pin the chart by version, not `main`. Bumping a cluster is
  one change covering both chart and images.
- A standalone cluster can install and upgrade with no git repo and no ArgoCD:
  `helm upgrade --install kyber oci://ghcr.io/matty-v/charts/kyber --version X.Y.Z -f values.yaml`.
  The chart carries `crds/` and template changes, which is why updates ship as a
  chart and not as an image-tag patch on the Deployment.
- **`helm upgrade` does not upgrade CRDs.** Helm installs `crds/` once, at
  install time, and never touches that directory again — by design. The CRDs
  in a newer chart do *not* ride along with an upgrade. Apply them yourself
  first:
  `helm pull oci://ghcr.io/matty-v/charts/kyber --version X.Y.Z --untar && kubectl apply -f kyber/crds/`,
  then upgrade. Skipping it ships new templates against an old schema, which
  half-works and fails later somewhere else. The self-upgrade path below does
  this step for you and refuses to continue if it fails.
- **The published chart carries its own image tags.** `release.yml` stamps the
  release version into all eight `image.*.tag` values before packaging, so the
  artifact installs as-is. The chart *source* still requires tags explicitly and
  refuses to guess from `Chart.AppVersion` (kyber#358/#457) — that guard is for
  people rendering from this repo, and it is why a bare `helm template
  deploy/helm/kyber` fails.
- A new GHCR package is **private** by default. `charts/kyber` had to be flipped
  to public once, by hand, after the first publish — the same one-time step every
  `kyber-*` image package needed. If operators report a 401 that reads like a bad
  chart reference, check package visibility first.

## Release flow

Kyber uses GitOps via ArgoCD. No manual `helm upgrade` is required for normal code changes.

```
feature branch → PR → test workflow green → merge to main
                                              │
                                              ▼
                           build.yml builds + pushes images to GHCR
                           (ghcr.io/matty-v/kyber-*:latest + :<sha>)
                                              │
                                              ▼
                    a release is proposed to the operator
                                         │
                                         ▼  (operator: "release approve")
                    prepare-release.yml — folds the Chart.yaml
                    version bump into the commit, pushes tag vX.Y.Z
                                         │
                                         ▼
                    release.yml — rebuilds all 8 images at :vX.Y.Z,
                    cuts the GitHub Release, publishes pwa-views,
                    publishes the chart to oci://…/charts/kyber:X.Y.Z
                                         │
                                         ▼
                    deploy-bump-pr matrix [falcon] — digest-pinned
                    bump PR on kyber-deploy, auto-merged
                                         │
                                         ▼
                    ArgoCD syncs falcon to vX.Y.Z

                    (razer is NOT here — it pulls; see above)
```

> No cluster sits between "merge to main" and "cut a tag" today. The canary that
> used to smoke-test the merge SHA was retired on 2026-08-10; until a replacement
> is stood up, a release is proposed off CI signal alone.

**CI does not deploy.** The `build.yml` workflow (`.github/workflows/build.yml`) only builds and pushes images. The deploy job was removed in commit `0a1958b` by design: CI builds, ArgoCD deploys.

The Helm chart (`deploy/helm/kyber/`) is the contract ArgoCD renders. Per-environment values and Application manifests live in [matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy).

## Promoting a release to falcon

Promotion is **automatic for every cluster in the matrix** once a tag is cut —
`release.yml`'s `deploy-bump-pr` job runs a `matrix: cluster: [falcon]`, and
each leg opens and squash-merges a digest-pinned bump PR against kyber-deploy.
**razer is deliberately not in the matrix**: it pulls its own updates, so a tag
publishes a chart it can be told to install rather than pushing one at it.
`fail-fast: false` keeps the legs independent, so one cluster failing doesn't
strand the other. Per-cluster `cluster-promoted` / `cluster-promote-failed` signals
can be sent to an inbound webhook (e.g. a release-automation agent) so progress
shows up in chat without opening CI — optional, operator-specific wiring.

GCP was manual-promote for a while — an intentional blast-radius gate — but it
drifted to v1.0.0 while falcon tracked v1.7.x, so kyber#449 folded it into the
same matrix. It was then dropped from the matrix entirely on 2026-08-10: the VM is
terminated, so every release was auto-merging a bump PR against a cluster that
could not sync it. `environments/gcp/` stays in kyber-deploy and stays valid,
frozen at its last release; re-bootstrapping gcp means adding it back to the
matrix. razer took its place there when it left the canary lane.

The human gate is **the operator's `release approve`** before the tag is cut, not
a second promotion step afterwards.

To hold a cluster back, pin its `environments/<cluster>/values.yaml` digests by
hand and revert the bump PR — there is no promote/hold workflow.

The remainder of this doc applies to every lane unless explicitly called out — the post-merge verification steps and rolling-restart procedures are the same.

## 1. Verify CI is green on `main`

```bash
gh run list --repo matty-v/kyber --branch main --limit 5
```

All workflows (`test`, `build`, `e2e`, `integration`) must be green on the commit you plan to ship. If any are red, fix them before merging.

## 2. Verify the image exists in GHCR

```bash
COMMIT_SHA=$(gh api /repos/matty-v/kyber/commits/main -q .sha)
echo "Target SHA: $COMMIT_SHA"

for img in kyber-control-plane kyber-node-agent kyber-status-sidecar \
           kyber-mcp-discord kyber-mcp-telegram kyber-runtime-base \
           kyber-claude-code kyber-codex; do
  docker manifest inspect "ghcr.io/matty-v/$img:$COMMIT_SHA" >/dev/null \
    && echo "$img OK" || echo "$img MISSING"
done
```

That is the full set of **8** published images. `build.yml` uses path filters, so a
given merge only rebuilds the images its changes touched — an image being absent at
*this* SHA is normal if nothing under its build context changed. What matters at
release time is that `release.yml` rebuilds all 8 at the tag.

If an image you expected is missing, the build workflow didn't publish it. See § "Image publishing" below.

> Every image is tagged both as `:latest` and as `:<commit-sha>`, but **no cluster
> consumes `:latest` any more** — razer was the last one and left that lane on
> 2026-08-10. Every cluster moves only when a semver tag is cut; see § "Release
> lanes".

## 3. Watch ArgoCD deploy

> **Cluster placeholders used from here on.** Kyber resources are prefixed with the
> ArgoCD release name, which is the cluster name. Set these once and the rest of the
> commands in this doc are copy-pasteable:
>
> ```bash
> export CLUSTER=kyber-razer   # or kyber-falcon
> export ENV=${CLUSTER#kyber-}   # the environments/<ENV>/ dir in kyber-deploy
> ```

Once the bump PR merges, ArgoCD syncs the new pins. Watch the rollout:

```bash
export KUBECONFIG=~/.kube/${CLUSTER}.yaml

# Watch Image Updater detect the new digest
kubectl -n argocd logs deploy/argocd-image-updater --tail=30 -f

# Watch the Application sync status
kubectl -n argocd get applications ${CLUSTER} -w

# Watch pod rollout
kubectl -n kyber-system get pods -w
```

Typical timing: bump PR merges → ArgoCD syncs → ~30 sec → pods roll.

## 4. Verify

```bash
kubectl -n kyber-system get pods
kubectl -n kyber-system rollout status deploy/${CLUSTER}-control-plane
kubectl -n kyber-system rollout status daemonset/${CLUSTER}-node-agent

# API smoke test
SERVICE_IP=$(kubectl -n kyber-system get svc ${CLUSTER}-control-plane -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
curl -s http://$SERVICE_IP:8080/healthz
curl -s -H "Authorization: Bearer $KYBER_API_KEY" http://$SERVICE_IP:8080/api/v1/fleet/summary
```

Watch the control-plane logs for state transitions on existing agents — they should resume reconciling within seconds of the new pod becoming Ready:

```bash
kubectl -n kyber-system logs deploy/${CLUSTER}-control-plane --tail=20 -f
```

## 5. Chart-level changes (values / new features)

For changes to environment config (new values keys, changed resource limits, feature flags), edit `environments/${ENV}/values.yaml` in the kyber-deploy repo, commit, and push. ArgoCD detects the change and syncs automatically.

```bash
cd ~/dev/kyber-deploy
# Edit environments/${ENV}/values.yaml
git add environments/${ENV}/values.yaml
git commit -m "chore(prod): update <thing>"
git push
```

ArgoCD will sync within its poll interval (default: 3 minutes). Force a manual sync via the ArgoCD UI or:

```bash
kubectl -n argocd patch application ${CLUSTER} --type merge \
  -p '{"operation":{"sync":{"revision":"HEAD"}}}'
```

### Opting into caller-level lifecycle authorization (kyber#474)

This release adds scoped API keys for agent lifecycle verbs. **No action is
required on upgrade** — it ships off by default (`api.authz.enforce: false`) and
the existing full-scope `api-key` keeps driving every lifecycle action, so PWA and
CLI clients are unchanged. To adopt it (a per-cluster, 🚦-gated operator step):

1. Add a `callers` JSON document to the `kyber-api-credentials` Secret (scoped
   keys — operator-managed `kubectl patch`, **not** chart-rendered). Schema and
   examples: [`api-keys.md`](api-keys.md).
2. Leave enforcement off and watch the audit log for `would-deny` lines to confirm
   no legitimate caller would be blocked.
3. Set `api.authz.enforce: true` in the cluster's `values.yaml` and sync. From
   then on an under-scoped caller gets **403** on the verbs it lacks scope for.

Rollback is instant: set `api.authz.enforce: false` and sync.

## Letting a cluster upgrade itself

A cluster that is a real Helm release can install its own updates. Turn it on in
values:

```yaml
selfUpgrade:
  enabled: true
  chartRef: "oci://ghcr.io/matty-v/charts/kyber"
```

Then `POST /api/v1/updates/apply` (or the button in the PWA) creates a Job that:

1. reads the release and captures its current values — explicitly, to a file,
   never `--reuse-values`, which hides what a release is configured with;
2. refuses if those values pin image tags (see below);
3. pulls the target chart and **applies its `crds/`**, failing closed;
4. runs `helm upgrade` with `--wait`;
5. verifies against the *running* cluster — the control-plane Deployment is
   rolled out, its container image is the version you asked for, and `/healthz`
   answers;
6. rolls back to the starting revision if any of that fails.

The Job's log is the record: `kubectl logs -n kyber-system job/<release>-upgrade-<version>`.

**It runs in a Job, not in the control plane**, because the control plane is the
process being replaced. It runs the control plane's *current* image with a
different entrypoint (`kyber-upgrade`), so there is no extra image to build or
pin.

### What it refuses, and why

- **ArgoCD-managed clusters.** `canSelfUpgrade` is false there. ArgoCD would
  revert the upgrade on its next sync, so it would appear to work and then
  silently undo itself. Bump the chart version in kyber-deploy instead.
- **Ownership it cannot determine.** Unknown is treated as "do not act".
- **Values that pin image tags.** The chart version carries the images. Values
  that set `image.<component>.tag` override the new chart, so the upgrade would
  rewrite the templates and leave the old containers running — Helm reporting
  success while nothing changed. The Job names the offending keys and stops.
  Remove them and let the chart decide.
- **A second upgrade while one is in flight**, and any version that is not a
  clean `X.Y.Z`.

`selfUpgrade.enabled: true` creates a ServiceAccount bound to **cluster-admin**.
That is not laziness: a Helm upgrade rewrites this chart's own RBAC and CRDs, and
Kubernetes forbids a subject from creating rules it does not hold, so a narrower
role would have to enumerate everything the chart grants and would break the
first time the chart gained a permission — mid-upgrade. Leave it off unless you
want it.

## Rolling back

ArgoCD tracks history. Roll back via the UI (Application → History → select a previous revision → Rollback), or via the CLI:

```bash
# List sync history
kubectl -n argocd get application ${CLUSTER} -o jsonpath='{.status.history[*].id}' | tr ' ' '\n'

# Roll back to a previous revision
argocd app rollback ${CLUSTER} <revision-id>
```

This reverts the cluster state to the desired state from that revision. For image rollbacks, you can also pin a specific SHA directly in `environments/${ENV}/values.yaml` in kyber-deploy and push — ArgoCD syncs immediately.

For **state rollback** (restore a PostgreSQL snapshot):

```bash
# Back up first
kubectl -n kyber-system exec -it deploy/${CLUSTER}-postgres -- \
  bash -c 'pg_dump -U kyber kyber' \
  > ~/kyber-backups/kyber-$(date -u +%Y%m%dT%H%M%SZ).sql

# Restore
kubectl -n kyber-system exec -i deploy/${CLUSTER}-postgres -- \
  bash -c 'psql -U kyber -d kyber' \
  < ~/kyber-backups/kyber-<timestamp>.sql
```

Restoring the database will lose any session briefs written since the snapshot. Agents in the middle of a restart may boot with a stale brief — they'll log the inconsistency via the `shutdown_type` field and continue.

## Rolling agent pods to a new agent image

Kyber's GitOps flow rolls the **control plane** and **node-agent** pods to new images automatically. It does **not** roll existing agent pods — those are created by the controller from the Agent CRD, and each pod pins whatever image tag the controller was using when the pod was created.

So when a change lands that affects the `claude-code` image (new plugins, new boot-script behavior, a new binary like `kyber-token-reporter`, new default env vars, etc.), existing agents keep running the old image until restarted. New agents created after the upgrade get the new image automatically.

### When to restart agents after an upgrade

Anything that changes `images/claude-code/Dockerfile`, `images/claude-code/start-claude.sh`, or the agent-side Go binaries baked into the image. Examples:

- New MCP plugin added to the boot script
- New background binary (e.g. `kyber-token-reporter`)
- Change to how `start-claude.sh` handles OAuth, channels, or tmux
- Updated Node.js / Claude Code CLI version
- Bugfix to anything under `cmd/token-reporter/` or `pkg/tokenreport/`

Control-plane-only changes (API routes, controller reconciler logic, PWA) do **not** require agent restarts.

### How to restart an agent

Via the PWA: Agent detail page → Restart button.

Via the API:

```bash
curl -s -X POST -H "Authorization: Bearer $KYBER_API_KEY" \
  https://kyber.your-tailnet.ts.net/api/v1/agents/<name>/restart
```

The restart:
1. Tells the controller to delete the current pod
2. Controller reconciles, spawns a fresh pod using the current image tag
3. The new pod pulls the latest `claude-code` image (or whatever tag the controller has configured)
4. `start-claude.sh` boots, keychain credentials + whole-disk overlay restore the session state
5. Agent returns to Running

Expected downtime per agent: 30-90 seconds depending on image cache state.

### Fleet-wide rollout

To restart every agent in the fleet after a major upgrade:

```bash
curl -s -H "Authorization: Bearer $KYBER_API_KEY" \
  https://kyber.your-tailnet.ts.net/api/v1/agents | \
  jq -r '.items[].id' | \
  while read name; do
    echo "Restarting $name..."
    curl -s -X POST -H "Authorization: Bearer $KYBER_API_KEY" \
      "https://kyber.your-tailnet.ts.net/api/v1/agents/$name/restart" > /dev/null
    sleep 2  # stagger so they don't all pull the new image simultaneously
  done
```

Stagger restarts by a few seconds so the cluster's image pull quota isn't hammered. For large fleets, consider restarting in waves.

## CRD migrations

Kyber CRDs live in `deploy/helm/kyber/crds/` (not under `templates/`). ArgoCD applies CRDs via `ServerSideApply=true` on every sync — no manual `kubectl apply -f crds/` step is needed.

**Adding a new CRD:** add the YAML to `deploy/helm/kyber/crds/`. ArgoCD will apply it on next sync.

**Adding a new field to an existing CRD (additive):** safe. The new field is `omitempty`, so old objects continue to validate. `make generate` regenerates `deploy/helm/kyber/crds/kyber.io_agents.yaml` and `kyber.io_machines.yaml`. Merge to main; ArgoCD picks it up automatically.

**Removing or renaming a field:** do not do this as part of a normal upgrade. Plan a migration:

1. Deploy a version that reads both the old and new fields
2. Backfill the data (manual or via a one-shot job)
3. Deploy a version that stops writing the old field
4. After all objects are migrated, deploy a version that drops the old field

One recorded exception: the 2026-08 removal of `spec.scaling` / `spec.idleTimeout` and the
`Suspended` phase skipped the 4-step migration deliberately — no code ever read the fields, so
there was nothing to backfill, and the structural schema prunes the stored values harmlessly.
The one thing that migration does require: **before upgrading, no Agent CR may have
`status.phase: Suspended` or `spec.desiredPhase: Suspended`.** A `desiredPhase: Suspended` is
recoverable after the fact (clear the field; the enum otherwise rejects spec writes). A
`status.phase: Suspended` is worse: status carries no enum so it survives the upgrade, but every
operator verb (start/stop/restart/force-needs-auth) then derives no event and the agent goes
inert — recovery is a status-subresource patch or delete/recreate, not a spec write.

## Image publishing

The `build.yml` workflow (rewritten 2026-04-14 with path filters + per-image jobs) runs on every push to `main`. It builds only the images affected by the changed paths and publishes them to GHCR. Typical build times:

- PWA-only change: ~3 minutes (skips all Go image builds)
- Single Go image change: ~4-5 minutes
- All images: ~10-12 minutes (was ~15 before the refactor)

Each job produces a tagged image:

```
ghcr.io/matty-v/kyber-control-plane:<sha>   + :latest
ghcr.io/matty-v/kyber-node-agent:<sha>      + :latest
ghcr.io/matty-v/kyber-status-sidecar:<sha>  + :latest
ghcr.io/matty-v/kyber-mcp-discord:<sha>     + :latest
ghcr.io/matty-v/kyber-mcp-telegram:<sha>    + :latest
ghcr.io/matty-v/kyber-runtime-base:<sha>    + :latest
ghcr.io/matty-v/kyber-claude-code:<sha>     + :latest   (FROM kyber-runtime-base)
ghcr.io/matty-v/kyber-codex:<sha>           + :latest   (FROM kyber-runtime-base)
```

`kyber-claude-code` and `kyber-codex` are both built **on top of**
`kyber-runtime-base`, so a runtime-base change rebuilds all three.

The push job requires a `GHCR_PAT` repo secret (a classic Personal Access Token with `write:packages` scope). `GITHUB_TOKEN` cannot write user-owned GHCR packages — only org-owned ones. See `.github/workflows/build.yml` for the inline comment explaining this.

> **Doc-only changes are skipped by CI.** The `build.yml` workflow uses `paths-ignore` to skip builds when only `docs/**` or `*.md` files change. A docs-only commit will show a green CI run that skips image building — this is intentional.

### Setting up `GHCR_PAT`

1. Go to https://github.com/settings/tokens and create a classic PAT (not fine-grained)
2. Scope: `write:packages` and `read:packages`
3. Expiration: 1 year (set a calendar reminder to rotate)
4. Copy the token value
5. `gh secret set GHCR_PAT --repo matty-v/kyber` and paste the value

### If the push job fails

- **401 Unauthorized:** the PAT is wrong or expired. Rotate and re-set the secret.
- **403 Forbidden on write:** the PAT is missing `write:packages`. Recreate it.
- **Image layer upload fails repeatedly:** GHCR is throttling or out of storage quota. Retry the workflow manually after a few minutes.

### First-time setup (before any images exist)

Before the first ArgoCD sync, the images must already be in GHCR. The install doc (`docs/installation.md` § 1) verifies this. If no images exist yet, push a commit to `main` to trigger the build workflow. Once images exist and are made public (see `docs/installation.md` § 1), the Application can sync.

## Database schema changes

Kyber uses a trivial Postgres schema (`session_briefs` table — a single JSON blob per agent). Schema evolution is handled in `pkg/briefstore/postgres.go` via the `Migrate()` method, which runs `CREATE TABLE IF NOT EXISTS` at control-plane startup.

For **additive changes** (new columns): add them to `Migrate()` as `ALTER TABLE ADD COLUMN IF NOT EXISTS`. Safe to run on every startup.

For **destructive changes** (dropping columns, changing types): write a one-shot migration job as a Helm `post-upgrade` hook, or run the SQL manually via `kubectl exec`. Do not put destructive migrations in `Migrate()` — that runs on every pod restart.

## Upgrading Terraform infrastructure

The Terraform small profile is a one-time provision. Normal upgrades to the running cluster go through ArgoCD, not Terraform. But if you need to change VM parameters (machine type, disk size, region), do it this way:

1. Update `~/.config/kyber/terraform.tfvars` or the `terraform apply` flags
2. `terraform plan` to see exactly what changes
3. **If the plan destroys and recreates the VM:** do not apply. The k3s install is on the VM's boot disk and will be lost. Instead:
   - Delete the ArgoCD Application first (`kubectl -n argocd delete application ${CLUSTER}`)
   - `helm uninstall ${CLUSTER} -n kyber-system`
   - `terraform destroy`
   - `terraform apply` with the new config
   - Re-run the installation doc from § 3 (re-install Kyber and ArgoCD)
4. **If the plan only updates in-place:** `terraform apply` and verify k3s is still running

## Upgrading k3s

k3s is installed by the Terraform startup script. The script does **not** auto-update k3s. To upgrade:

```bash
gcloud compute ssh kyber-small-k3s-server --zone=us-central1-a --command '
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=v1.34.6+k3s1 sh -
  sudo systemctl restart k3s
'
```

**Do not pin below v1.33 here.** Agents run in a user namespace and refuse to
start without one, so a cluster on an older k3s runs Kyber fine and schedules no
agents at all. Kubernetes also only supports one minor version per upgrade, so
getting from an old pin to a supported one may take more than one pass.

Pin the k3s version in the startup script (`infra/terraform/scripts/k3s-install.sh`) so new VMs get the same version. There's a TODO comment in the script to do this — do it before the first production install.

After upgrading k3s, verify cluster health:

```bash
kubectl get nodes
kubectl -n kyber-system get pods
kubectl -n argocd get pods
```

k3s upgrades are usually safe but occasionally change container runtime behavior. Test in dev first if possible.

## Rotating secrets

Rotating the API key requires both updating the k8s secret and updating any clients (PWA, external tooling) that use the old value:

```bash
NEW_API_KEY=$(openssl rand -hex 32)
kubectl -n kyber-system patch secret kyber-api-credentials \
  --type=merge \
  -p "{\"stringData\":{\"api-key\":\"$NEW_API_KEY\"}}"
kubectl -n kyber-system rollout restart deploy/${CLUSTER}-control-plane
# Update the PWA Settings page in your browser
```

Rotate the OAuth credentials for individual Claude Code agents. As of 2026-04-14, the credential store is a `<agent-name>-oauth` k8s secret with `access_token` and `refresh_token` keys (written by the PKCE exchange at agent-create time). The refresh token auto-rotates on each pod boot via `start-claude.sh` — manual rotation is only needed if the refresh token is revoked or expired:

```bash
# Option A — Re-authorize via the PWA (preferred):
# In the PWA, find the agent → click "Re-authorize" (only visible when agent is in NeedsAuth phase).
# This runs the same PKCE flow as Create Agent and updates the existing secret.

# Option B — Manual secret replacement (emergency):
kubectl -n kyber-system patch secret dave-oauth \
  --type=merge \
  -p "{\"stringData\":{\"refresh_token\":\"$NEW_REFRESH_TOKEN\",\"access_token\":\"placeholder\"}}"
# Trigger a restart so start-claude.sh picks up the new refresh token:
curl -s -X POST -H "Authorization: Bearer $KYBER_API_KEY" \
  http://$SERVICE_IP:8080/api/v1/agents/dave/restart
```

## Monitoring a deploy

Watch three things during an ArgoCD-driven rollout:

```bash
# Application sync status
kubectl -n argocd get applications ${CLUSTER} -w

# Pod status
watch kubectl -n kyber-system get pods

# Control plane logs
kubectl -n kyber-system logs deploy/${CLUSTER}-control-plane -f --tail=20
```

Healthy rollout signs:
- ArgoCD Application reaches `Synced` + `Healthy`
- New control-plane pod reaches `Ready` within 30 seconds
- Existing agents stay in their current phase (Running stays Running)
- No events of type `Warning` in `kubectl get events -n kyber-system`

Unhealthy signs:
- `ImagePullBackOff` → the image doesn't exist, or the GHCR package visibility was changed to private without updating `imagePullSecrets`
- `CrashLoopBackOff` → application error; inspect logs, consider rollback via ArgoCD
- Agents flapping between phases → state machine regression; roll back via ArgoCD

## Deleting the kyber-system namespace (escape hatch)

If you ever `kubectl delete namespace kyber-system` while Machine CRs exist, the Machine finalizer will short-circuit and self-remove **without** deprovisioning the underlying GCE instances. This escape hatch exists so the namespace terminator can't deadlock if the controller pod is already gone, but it means **you own the cleanup of any orphaned VMs**.

Every orphan emits a `Warning`-level event `ExternalResourceOrphaned` with the instance ID, zone, and the exact `gcloud compute instances delete` command to run. Scrape those events *before* the namespace fully terminates, or grep the control-plane logs for `ORPHANING external VM`:

```bash
# Capture orphaned-instance events before the namespace is fully GC'd
kubectl -n kyber-system get events --field-selector reason=ExternalResourceOrphaned -o wide

# Or scan control-plane logs after the fact
kubectl -n kyber-system logs deploy/${CLUSTER}-control-plane --tail=500 | grep "ORPHANING external VM"
```

The normal delete flow (delete individual Machine CRs while the namespace stays up) runs the full finalizer and deprovisions VMs correctly — this escape path only triggers when the *namespace* is in Terminating.

## Known upgrade pitfalls

- **Image Updater uses argocd write-back, not git.** Tag overrides are written directly to `Application.spec.sources[0].helm.parameters` — no PAT or git commits required. The tradeoff is that image bumps aren't in git history (they live in cluster state). If you reinstall ArgoCD, the overrides are regenerated on the next Image Updater poll — not lost, just briefly out of sync.
- **First-party Postgres StatefulSet password is in `${CLUSTER}-postgres` secret.** Do not rotate it — the `postgres` superuser credential is baked into initdb. Rotating requires a fresh install or a manual `ALTER USER` via psql.
- **Service names are release-prefixed.** The ArgoCD Application is named after the cluster (`kyber-razer`, `kyber-falcon`, `kyber-gcp`), so all resources are prefixed with it (e.g. `kyber-razer-postgres`, `kyber-razer-redis`, `kyber-razer-control-plane`). Scripts and DSNs that hardcode a bare name (`kyber-postgresql`, `kyber-redis-master`) or another cluster's prefix will break — update them.
- **k3s `LoadBalancer` port conflicts.** Only one service can bind port 8080 on the host. If you change the API port in values and don't change the firewall rule, the PWA becomes unreachable.
- **GHCR packages are public by default.** The chart's `imagePullSecrets` is `[]` in prod values — no secret needed for pulls. If you flip a package to private, add a `docker-registry` secret in `kyber-system` and reference it in values.yaml's `imagePullSecrets`. `ImagePullBackOff` with `unauthorized` usually means the package visibility changed or a secret was referenced but doesn't exist.

## Downgrading to a specific version

Pin a specific SHA in `environments/${ENV}/values.yaml` in kyber-deploy:

```yaml
image:
  controlPlane:
    tag: "<older-tag-or-sha>"
  nodeAgent:
    tag: "<older-tag-or-sha>"
  statusSidecar:
    tag: "<older-tag-or-sha>"
  agentBase:
    tag: "<older-tag-or-sha>"
  claudeCode:
    tag: "<older-tag-or-sha>"
  # Optional — only set if this install uses them:
  codex:
    tag: "<older-tag-or-sha>"
  telegramSidecar:
    tag: "<older-tag-or-sha>"
  discordSidecar:
    tag: "<older-tag-or-sha>"
```

Commit and push. ArgoCD syncs automatically.

Two cautions:

- **Verify the images actually exist at that reference first.** `build.yml` uses
  path filters, so not every commit SHA has every image. Downgrading to a
  **semver tag** (`vX.Y.Z@sha256:…`) is safer — `release.yml` guarantees a coherent
  set of all 8 at a tag.
- **(Retired 2026-08-10)** A hand-edited runtime-image pin on the canary cluster used to be
  overwritten within 30 minutes by `sync-razer-latest.yml`, so a downgrade silently reverted
  unless you disabled that workflow first. The cron is deleted and no cluster chases `:latest`,
  so hand-edited pins now stick. Noted in case you hit this in an older runbook.
