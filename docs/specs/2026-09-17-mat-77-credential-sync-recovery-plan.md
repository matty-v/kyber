# MAT-77 — credential synchronization recovery plan

Status: implementation in progress.
Issue: https://linear.app/matty-v/issue/MAT-77
Parent: MAT-7.
Baseline: `88ead00` (`origin/main`).

## Outcome and boundaries

Make Claude Code and Codex credential rotation recover safely across runtime
restart and pod replacement without letting an older bootstrap Secret overwrite
a newer credential on persistent disk. A write-back retry must be idempotent,
must not overwrite a newer operator credential, and must never expose credential
material in logs, status, fixtures, or API responses.

This change will not add a CRD field, lifecycle phase, authentication mode,
dependency, or user-facing credential payload. It preserves provider-owned
credential formats and the existing per-agent Kubernetes Secrets. The persistent
credential file is the recovery copy while a rotation is pending; the Secret is
the durable bootstrap copy after a compare-and-set write-back succeeds.

Loss of both the persistent disk and the last successfully synchronized Secret
remains unrecoverable. The first boot after upgrading an agent that already has
a local credential but no seed marker is intentionally local-first because
overwriting a possibly rotated single-use token is the more destructive choice.
That one-time ambiguity is documented rather than hidden.

## Verified baseline and failure windows

- Claude refreshes an upstream token before it writes the rotated credential to
  `.credentials.json`. If the control-plane push fails, exit `45` occurs first
  and the only usable rotated refresh token is lost.
- Claude prefers injected Secret values whenever they are present. A stale
  Secret can therefore replace a newer local credential on restart.
- Codex already records the last seeded Secret hash and avoids that overwrite,
  but the reporter suppresses the initial push when the local file is newer.
  The Secret can remain stale until another native refresh happens.
- Both control-plane endpoints unconditionally update the current Secret. A
  delayed retry from an old pod can overwrite a newer reauthorization.
- Both reporters retain the changed local credential after a failed push, but
  their exponential delay is followed by the normal polling interval rather
  than an immediate retry. The nominal five-minute backstop therefore controls
  recovery after the first failure.
- Claude deduplicates only on `expiresAt`; Codex uses the opaque document hash.
  A rotated credential must be identified by its complete provider-owned state,
  not by expiry alone.

## Design

### 1. Ordered, idempotent Secret write-back

Add an optional expected credential hash to both private write-back requests.
The control plane computes the current Secret credential hash and applies these
rules atomically through the Kubernetes resource-version update:

1. If the Secret already equals the proposed credential, return success. This
   makes a lost HTTP response safe to retry.
2. If an expected hash is present and does not match the current Secret, return
   `409 Conflict` without changing it. A stale pod cannot overwrite a newer
   operator reauthorization or later rotation.
3. Otherwise write the complete credential and return success.

Hashes are SHA-256 identifiers of canonical credential state. They are control
metadata, never authentication material, and only short prefixes may appear in
diagnostic logs. Requests from older images without the precondition remain a
rolling-upgrade compatibility path; new images always send it.

### 2. Durable local recovery state

Claude gains the same private seed-marker concept already used by Codex. The
marker stores only the hash of the last Secret copy observed or successfully
written. Startup compares the injected Secret hash, local credential hash, and
marker:

- unchanged Secret plus different local file means local rotation is pending;
- changed Secret means explicit reauthorization wins and is seeded locally;
- no local file seeds from the Secret;
- no marker plus an existing local file adopts local state without clobbering
  it, recording the one-time upgrade ambiguity.

Claude writes a newly refreshed credential atomically to its persistent file
before attempting remote write-back. If write-back fails, exit `45` leaves the
usable token recoverable. The next bounded restart detects the pending local
copy and retries write-back without spending the single-use refresh token
again. The marker advances only after confirmed or idempotent success.

Codex retains its opaque-document treatment. Startup will request one initial
push whenever the local auth document is newer than the unchanged Secret, and
the reporter receives the Secret hash as its initial compare-and-set base.

### 3. Bounded reporter recovery

Refactor the two reporters around the same retry behavior while retaining their
provider-specific parsers:

- content-hash deduplication for the complete credential;
- immediate retry scheduling at `1s`, doubling to a `5m` cap independently of
  the normal fsnotify/poll backstop;
- one serialized in-flight push, with newer local content superseding an older
  pending snapshot;
- success advances both the local dedupe hash and expected Secret hash;
- `409 Conflict` is terminal for that local snapshot and is logged as a
  redacted, actionable supersession rather than retried forever;
- transport and 5xx failures remain retryable; malformed local files never
  replace a good Secret.

Boot-time Claude synchronization remains blocking because the runtime must not
start after consuming a single-use refresh token that has neither a persistent
local recovery copy nor durable Secret copy.

### 4. Evidence and residual guarantees

Update the v1 harness contract and conformance matrix with the exact ownership
and ordering rules. Fixture evidence will be recorded separately from the
combined MAT-7 staging pass. The documented residual window is loss of the
persistent volume after upstream rotation but before successful Secret
write-back; that requires reauthorization because Kyber cannot reconstruct a
single-use token that no durable copy retains.

## Test matrix

| Layer | Required cases |
|---|---|
| Control plane | compare-and-set success, idempotent replay, stale-writer conflict, Kubernetes update conflict/error, full-field atomicity, bounded/valid inputs |
| Claude startup | normal seed, stale Secret with newer local state, explicit reauthorization, first-upgrade adoption, refresh persisted before failed write-back, restart recovery, revoked credential, private file/marker modes |
| Codex startup | unchanged bootstrap preserves and re-pushes newer local auth, explicit reauthorization wins, device login initial push, first-upgrade adoption, empty/corrupt local recovery |
| Reporters | successful rotation, transient failure with immediate bounded retry, response-loss idempotency, stale-writer conflict, fsnotify and polling, malformed/oversized files, restart from pending local state |
| Secret safety | no credential values in logs, errors, status, fixtures intended for output, or API responses; hashes are never printed in full |
| Regression | existing authentication/startup/sync suites, full Go build/vet/test, integration CI, contract/product documentation checks |

## Checkpoints

- [x] Load MAT-77 and create a clean worktree from current `origin/main`.
- [x] Trace both startup scripts, both reporters, sidecar forwarding, Secret
  mutation endpoints, MAT-7 contract text, and existing recovery fixtures.
- [x] Record the implementation plan and explicit residual guarantee.
- [ ] Add failing API, reporter, and startup fixtures for ordering, retry,
  restart, pod replacement, revocation, and redaction.
- [ ] Implement compare-and-set write-back and provider-specific startup
  recovery without changing public API or CRDs.
- [ ] Run focused runtime/API tests, full build/vet/test, integration suites,
  and documentation checks.
- [ ] Review the complete diff for single-use-token safety, `set -euo pipefail`,
  Kubernetes conflicts, compatibility, and secret leakage; fix all findings.
- [ ] Push a consolidated PR, require green CI, and record closure evidence on
  MAT-77.
- [ ] After merge, leave live restart/pod-recovery validation to the combined
  MAT-7 staging pass for MAT-76 through MAT-79 before the next Kyber release.

