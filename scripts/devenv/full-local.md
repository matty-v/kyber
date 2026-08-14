# Full local kyber (live agent pods) on k3d

The base [`scripts/devenv/`](./README.md) brings up a **control-plane / API only**
mock env (agent workloads are deferred there). This variant runs the **whole
stack locally** — the node-agent DaemonSet plus **live agent pods** that
schedule, mount whole-disk persistence, and run Claude Code — on the same k3d
cluster, with no cloud credentials.

## One command

```bash
scripts/devenv/up-full.sh              # cold (builds all images)
scripts/devenv/up-full.sh --skip-build # warm (reuse the control-plane image)
scripts/devenv/up-full.sh --compute-provider fake # recommended: managed lifecycle + runnable agents
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

## Compute modes

Use `--compute-provider fake` for normal full-stack testing. Fake instances
traverse the managed Machine controller but attach to the real k3d node, so
agents can schedule and run.

Use `--compute-provider gce-emulator` only when testing GCE-specific behavior.
It constructs the production `GCEAdapter` with a local REST endpoint and uses
synthetic Nodes for node-registration signals. These Nodes are tainted,
unschedulable, and accepted without kubelet heartbeats only under the explicit
emulator flag. They cannot host agent pods.

```bash
scripts/devenv/up-full.sh --skip-build --compute-provider gce-emulator
# Create a gce Machine in the PWA, then complete its simulated node join:
scripts/devenv/compute-scenario.sh attach-node <machine>
scripts/devenv/compute-scenario.sh apply <machine> preempted
# Replacement creates a new VM; attach its replacement Node as well:
scripts/devenv/compute-scenario.sh attach-node <machine>
```

If a GCE-emulator Machine remains Provisioning, run `compute-scenario.sh list`
to verify its instance is Running and then `attach-node <machine>`. Do not
assign an Agent to that Machine; switch the stack to `fake` and create a fake
Machine for runnable-agent testing.

## Local GitHub App for identity-repo testing

Your GitHub App credentials must never be committed. Register and install an
App using [the standard permissions and click path](../../docs/installation.md#6-register-the-kyber-github-app-optional),
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

Agents need a runnable Machine first, then an Agent. With the recommended fake
mode, create a fake Machine through the PWA; it maps to the real k3d node. The
legacy mock request is also available:

```bash
curl -sS -X POST http://localhost:18080/api/v1/machines \
  -H "Authorization: Bearer test-api-key-e2e" -H "Content-Type: application/json" \
  -d '{"name":"local","provider":"mock","capacity":{"cpu":"2","memory":"4Gi"}}'
# poll GET /api/v1/machines/local until status.phase == "Ready"
```

For a **working Claude agent** use OAuth (an API key also works for non-Telegram
agents; Telegram *requires* OAuth). Easiest: the **PWA creation wizard** at
`http://localhost:18080/` (or the hot-reload UI at `:5173`) — it runs the PKCE
flow, you log in at claude.ai, paste the code, and it creates the agent.

The pod should reach `4/4 Running`; confirm real auth in its log:

```bash
export KUBECONFIG=$(k3d kubeconfig write kyber-devenv)
kubectl logs -n kyber-system agent-<name> -c agent | grep -iE "model probe|credentials"
# expect: "credentials.json written" and "pre-flight model probe: ok"
```

### Telegram

Create the agent with `secrets.telegramEnabled: true` +
`secrets.telegramBotToken: <BotFather token>` (OAuth auth required). The control
plane injects a `kyber-mcp-telegram` **sidecar** into the agent's pod, which
bridges the chat to the agent session over MCP. `build-local-images.sh` builds
`kyber/mcp-telegram:local` and `values-full-local.yaml` already points
`image.telegramSidecar` at it, so nothing extra is needed here.

The pod gains one container when Telegram is enabled — confirm the sidecar is
actually there before concluding the bot is broken:

```bash
kubectl -n kyber-system get pod agent-<name> \
  -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}'
# expect: agent, kyber-mcp-telegram
```

(The channel sidecar is a normal container. Kyber's other sidecars — status,
transcript-tailer, session-saver, transcript-pruner — are `initContainers` with
`restartPolicy: Always`, so they show up in a different list.)

Message the bot from an account on the agent's allowlist to test. Messages from
anyone not on the allowlist are ignored silently — that is the primary access
control, not a bug.

> **This replaced an in-process plugin.** Claude Code used to carry a native
> `telegram@claude-plugins-official` plugin that polled getUpdates from inside
> the runtime container. That is retired (kyber#684): the runtime no longer
> receives a bot token, and the sidecar is now the only Telegram implementation
> for **both** runtimes. If you find docs or scripts referring to the plugin or
> to `OWNER_TELEGRAM_CHAT_ID`, they are stale.

## Notes

- The overlay uses `kubectl set env`-free config — everything is in the values
  file, so it survives `helm upgrade`.
- `down.sh` from the base devenv tears the whole thing down.
