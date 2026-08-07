# Upgrading Kyber

How to roll out new features, bug fixes, and schema changes to a running Kyber instance. This document is the source of truth — if a step doesn't match reality, fix the doc.

## Release lanes

> **Superseded model — read this if you remember the old one.** Kyber used to run
> a `latest`/`stable` two-track scheme: a `stable` branch, `:stable` image tags,
> and a `promote-stable.yml` workflow. **None of that exists anymore.** There is no
> `stable` branch, no `:stable` tag, and no promote workflow. Release promotion is
> now semver-tag driven (kyber#591, kyber#449). If you find a doc or script still
> referencing them, it is stale — fix it.

Every environment renders the **same chart from `main`**. What differs is how each
one's *image pins* advance:

| Lane | Chart source | Image pins | Used by | Advances when |
|---|---|---|---|---|
| **canary** | `main` HEAD | `latest@sha256:…` | `kyber-laptop` | continuously — automatic |
| **release** | `main` HEAD | `vX.Y.Z@sha256:…` | `kyber-falcon`, `kyber-gcp` | a semver tag is cut |

All three Application manifests in [matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy)
set `targetRevision: main` for the chart source. The lane is a property of the
digests written into `environments/<cluster>/values.yaml`, **not** of a branch.

The laptop cluster is deliberately the bleeding edge: it runs unreleased `main` and is the
smoke-test surface a release is proposed from. Falcon and GCP only ever move to
a tagged release.

### How each lane advances

**Canary (laptop)** — two mechanisms, split by whether the image is a Helm resource:

- `control-plane` and `node-agent` are real workloads in the Application's
  resource graph, so **ArgoCD Image Updater** digest-pins them (`write-back: argocd`,
  tracking `:latest`).
- `agentBase`, `claudeCode`, `statusSidecar`, `codex`, `telegramSidecar` are
  **controller-injected runtime images** — not Helm Deployments, so Image Updater
  can't see them. A scheduled job in kyber-deploy,
  `.github/workflows/sync-laptop-latest.yml` (every 30 min), resolves each one's
  current `:latest` digest and commits it to `environments/laptop/values.yaml`.
  Image Updater's force-update path used to intermittently write an **empty** tag
  here, which kyber#370's required-tag check then hard-failed, wedging the whole
  Application (kyber-deploy#38). The git-native sync job is the wedge-safe
  replacement.

  > **Any image you add to `environments/laptop/values.yaml` must also be added to
  > that workflow's key list**, or it stays git-pinned forever and silently never
  > advances. This exact miss happened to `telegramSidecar` (added to values in
  > kyber#684, added to the sync loop only in kyber#688).

**Release (falcon, gcp)** — advanced by `release.yml`, which opens and auto-merges
a digest-pinned bump PR against kyber-deploy for **both** clusters. The full
merge-to-release sequence (canary smoke-test → release proposal → operator
approval → `prepare-release.yml` cuts the tag → `release.yml` builds and promotes) is
documented in [`operator/release-runbook.md`](operator/release-runbook.md) — that
doc is the authority on the release pipeline; this one covers what an operator does
around it.

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
                    ┌─────────── canary lane (laptop) ───────────┐
                    │                                            │
                    ▼                                            ▼
     Image Updater digest-pins            sync-laptop-latest.yml (*/30) commits
     control-plane + node-agent           runtime-image digests to laptop values
                    │                                            │
                    └────────────────────┬───────────────────────┘
                                         ▼
                            ArgoCD syncs; laptop runs main HEAD
                                         │
                                         ▼
                    canary is smoke-tested on the merge SHA;
                    a release is proposed to the operator
                                         │
                                         ▼  (operator: "release approve")
                    prepare-release.yml — folds the Chart.yaml
                    version bump into the commit, pushes tag vX.Y.Z
                                         │
                                         ▼
                    release.yml — rebuilds all 8 images at :vX.Y.Z,
                    refreshes control-plane :latest, cuts the GitHub
                    Release, publishes pwa-views
                                         │
                                         ▼
                    deploy-bump-pr matrix [falcon, gcp] — digest-pinned
                    bump PRs on kyber-deploy, auto-merged
                                         │
                                         ▼
                    ArgoCD syncs falcon + gcp to vX.Y.Z
```

**CI does not deploy.** The `build.yml` workflow (`.github/workflows/build.yml`) only builds and pushes images. The deploy job was removed in commit `0a1958b` by design: CI builds, ArgoCD deploys.

The Helm chart (`deploy/helm/kyber/`) is the contract ArgoCD renders. Per-environment values and Application manifests live in [matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy).

## Promoting a release to falcon and GCP

Promotion is **automatic for both production clusters** once a tag is cut —
`release.yml`'s `deploy-bump-pr` job runs a `matrix: cluster: [falcon, gcp]`, and
each leg opens and squash-merges a digest-pinned bump PR against kyber-deploy.
`fail-fast: false` keeps the legs independent, so a GCP failure doesn't strand
falcon. Per-cluster `cluster-promoted` / `cluster-promote-failed` signals can be
sent to an inbound webhook (e.g. a release-automation agent) so progress shows up
in chat without opening CI — optional, operator-specific wiring.

GCP was manual-promote for a while — an intentional blast-radius gate — but it
drifted to v1.0.0 while falcon tracked v1.7.x, so kyber#449 folded it into the
same matrix. The human gate is now **the operator's `release approve`** before the
tag is cut, not a second promotion step afterwards.

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

> Every image is tagged both as `:latest` and as `:<commit-sha>`. On **the canary cluster** the
> `:latest` digest reaches the cluster within ~2 min (control-plane, node-agent via
> Image Updater) or within 30 min (runtime images via `sync-laptop-latest.yml`).
> **Falcon and GCP** move only when a semver tag is cut — see § "Release lanes".

## 3. Watch ArgoCD deploy

> **Cluster placeholders used from here on.** Kyber resources are prefixed with the
> ArgoCD release name, which is the cluster name. Set these once and the rest of the
> commands in this doc are copy-pasteable:
>
> ```bash
> export CLUSTER=kyber-gcp   # or kyber-laptop / kyber-falcon
> export ENV=${CLUSTER#kyber-}   # the environments/<ENV>/ dir in kyber-deploy
> ```

Once the image is pushed, Image Updater detects it automatically. Watch the rollout:

```bash
export KUBECONFIG=~/.kube/${CLUSTER}.yaml

# Watch Image Updater detect the new digest
kubectl -n argocd logs deploy/argocd-image-updater --tail=30 -f

# Watch the Application sync status
kubectl -n argocd get applications ${CLUSTER} -w

# Watch pod rollout
kubectl -n kyber-system get pods -w
```

Typical timing: image push → Image Updater detects → ~2 min → ArgoCD syncs → ~30 sec → pods roll.

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
  curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=v1.31.0+k3s1 sh -
  sudo systemctl restart k3s
'
```

Pin the k3s version in the startup script (`infra/terraform/scripts/k3s-install.sh`) so new VMs get the same version. There's a TODO comment in the script to do this — do it before the first production install.

After upgrading k3s, verify cluster health:

```bash
kubectl get nodes
kubectl -n kyber-system get pods
kubectl -n argocd get pods
```

k3s upgrades are usually safe but occasionally change container runtime behavior. Test in dev first if possible.

## Rotating secrets

Rotating the API key or webhook secret requires both updating the k8s secret and updating any clients (PWA, external tooling) that use the old value:

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
- **Service names are release-prefixed.** The ArgoCD Application is named after the cluster (`kyber-laptop`, `kyber-falcon`, `kyber-gcp`), so all resources are prefixed with it (e.g. `kyber-laptop-postgres`, `kyber-laptop-redis`, `kyber-laptop-control-plane`). Scripts and DSNs that hardcode a bare name (`kyber-postgresql`, `kyber-redis-master`) or another cluster's prefix will break — update them.
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
- **On the canary cluster, a hand-edited runtime-image pin gets overwritten within 30 minutes**
  by `sync-laptop-latest.yml`. To hold the canary cluster on an older runtime image you must
  disable that workflow first, otherwise the downgrade silently reverts.
