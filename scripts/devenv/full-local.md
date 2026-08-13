# Full local kyber (live agent pods) on k3d

The base [`scripts/devenv/`](./README.md) brings up a **control-plane / API only**
mock env (agent workloads are deferred there). This variant runs the **whole
stack locally** — the node-agent DaemonSet plus **live agent pods** that
schedule, mount whole-disk persistence, and run Claude Code — on the same k3d
cluster, still with mock compute (no cloud).

## One command

```bash
scripts/devenv/up-full.sh              # cold (builds all images)
scripts/devenv/up-full.sh --skip-build # warm (reuse the control-plane image)
```

It creates the k3d cluster, builds + imports all images
([`build-local-images.sh`](./build-local-images.sh)), creates the internal
signing key, and installs the release with
[`values-full-local.yaml`](./values-full-local.yaml) layered on the base test
values. When it finishes, the API + PWA are at `http://localhost:18080/` (or
`--api-port`) with key `test-api-key-e2e`.

It is self-contained rather than a wrapper around [`up.sh`](./up.sh): up.sh's
`--skip-build` path skips importing the control-plane image, which would leave a
fresh cluster with no runnable control-plane — so this script imports every
image itself. Tear down with `scripts/devenv/down.sh` (deletes the k3d cluster).

## Local GitHub App for identity-repo testing

Your GitHub App credentials must never be committed. Register and install an
App using [the standard permissions and click path](../../docs/installation.md#5b-register-the-kyber-github-app),
then save its values into the git-ignored local bundle:

```bash
scripts/devenv/setup-github-app.sh \
  --app-id <app-id> \
  --installation-id <installation-id> \
  --owner <github-user-or-org> \
  --private-key /absolute/path/to/downloaded-key.pem

scripts/devenv/up-full.sh --skip-build
```

The second command creates or updates `kyber-github-app`, sets
`identityRepo.defaultOwner` for this Helm release, and restarts the control
plane so it reloads the key. The bundle lives at
`.kyber-local/github-app/`, which is ignored by git. Override that location
with `DEVENV_GITHUB_APP_DIR` when desired.

For the smallest test blast radius, create a dedicated local-testing App and
install it only on a dedicated private identity repo. If you also want to test
Kyber's create-from-template flow, the installation needs access to the
template plus newly created repositories; follow the main installation guide's
`All repositories` setup.

## What the overlay changes (vs the base mock env)

| Knob | Base | Full-local | Why |
|---|---|---|---|
| `storage.agentStorageClass` | `""` (cluster default) | `""` | unchanged — stated explicitly because it used to default to `kyber-pd`, which k3d has no provisioner for |
| `nodeAgent.enabled` | `false` | `true` | run the node-agent DaemonSet (no k3s join creds needed under mock compute) |
| `image.{claudeCode,statusSidecar,agentBase}` | placeholder | `kyber/*:local`, `pullPolicy: Never` | live pods need the real runtime + sidecar images |
| `internalAuth.graceMode` | off | `true` | let the in-pod status pipeline (:8082) work before every pod carries a token |
| `controlPlane.cors.allowedOrigins` | empty | `:5173,:5174` | let the vite dev servers call the API cross-origin |

Prereqs already satisfied by k3d on a typical host: the node container exposes
`/dev/fuse` (fuse-overlayfs persistence, tier 2) and allows privileged pods.

## Create a live agent

Agents need a Machine first (mock), then an Agent. The Machine maps to the k3d
node:

```bash
curl -sS -X POST http://localhost:8080/api/v1/machines \
  -H "Authorization: Bearer test-api-key-e2e" -H "Content-Type: application/json" \
  -d '{"name":"local","provider":"mock","capacity":{"cpu":"2","memory":"4Gi"}}'
# poll GET /api/v1/machines/local until status.phase == "Ready"
```

For a **working Claude agent** use OAuth (an API key also works for non-Telegram
agents; Telegram *requires* OAuth). Easiest: the **PWA creation wizard** at
`http://localhost:8080/` (or the hot-reload UI at `:5173`) — it runs the PKCE
flow, you log in at claude.ai, paste the code, and it creates the agent.

The pod should reach `4/4 Running`; confirm real auth in its log:

```bash
export KUBECONFIG=$(k3d kubeconfig write kyber-devenv)
kubectl logs -n kyber-system agent-<name> -c agent | grep -iE "model probe|credentials"
# expect: "credentials.json written" and "pre-flight model probe: ok"
```

### Telegram

Create the agent with `secrets.telegramEnabled: true` +
`secrets.telegramBotToken: <BotFather token>` (OAuth auth required). The agent
pre-seeds an owner allowlist and Claude Code auto-installs the
`telegram@claude-plugins-official` plugin on launch, which polls getUpdates
in-pod (no inbound webhook needed). Message the bot from the owner account to
test.

> Note: the owner chat ID currently falls back to a hardcoded default in
> `images/claude-code/start-claude.sh` — set `OWNER_TELEGRAM_CHAT_ID` for your
> own account until that default is removed.

## Notes

- The overlay uses `kubectl set env`-free config — everything is in the values
  file, so it survives `helm upgrade`.
- `down.sh` from the base devenv tears the whole thing down.
