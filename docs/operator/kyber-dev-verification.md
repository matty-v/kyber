# Pre-merge dev verification gate (test-in-dev)

> **Operator-specific process.** This describes the upstream maintainers' review
> automation. If you run your own fork, treat it as a reference pattern, not a
> requirement.

Every deployable change is functionally tested on a shared **devenv-kyber**
instance *before* merge to `main`. This is the pre-merge gate (SDLC steps 4–6,
kyber#531); the post-merge canary smoke-test stays as the backstop. Both exist —
neither replaces the other.

> **Retired (kyber#531):** this gate previously used per-PR preview clusters, a
> shadow-mode `dev-verify.yml` status check, and a `/dev-verify` command — all keyed
> on the never-built `kyber-dev-pr-<n>` substrate. That machinery is gone. The gate
> is now the reviewer's **test-in-dev** stage against the shared devenv-kyber.

## The flow

```
PR opened → deployable? → acquire dev-lock → /deploy-to-dev (devenv-kyber serves cp:<head-sha>)
  → reviewer test-in-dev (exercise the change on the devenv-kyber instance) → merge verdict
  → merge → canary post-merge smoke → propose-release
```

1. **Deploy.** A deploy agent's `/deploy-to-dev` deploys the PR's `:<head-sha>`
   control-plane image to devenv-kyber, waits until it is *serving that sha* (not
   just "pod up"), and posts a structured comment on the PR with
   `{ status, sha, url }`.
2. **Test.** The reviewer reads that comment, asserts `deployed_sha == PR head`
   (stale-deploy guard), exercises *the specific capability the PR changes* on
   the devenv-kyber instance, and records what was exercised.
3. **Verdict.** The result folds into the `merge: yes/no/hold` verdict —
   "merge: yes" ⇒ "I tested it running." A revision re-deploys + re-tests.
4. **Merge** (maintainer) → canary post-merge smoke → release proposal as usual.

## Coverage boundary

An image-tag swap deploys the PR's *compiled code*, not its Helm templates. A PR
that changes `deploy/helm/**` is deployed by image-tag only — its chart change is
**not** exercised running, and the verdict must not claim it was.

## What counts as "verified"

Not "pods are up." The reviewer exercises the **real capability the PR adds or
changes** on the live instance — the new endpoint returns correctly, the new PWA
control behaves, the bug no longer reproduces — and records *what* was exercised,
so it's auditable on the PR.

## Serialization + substrate

One PR is under test at a time, serialized by a dev-lock (a GitHub label
`under-test-in-dev` on the holder PR, with a single designated writer — which is
the actual mutual-exclusion guarantee). devenv-kyber provisioning, version
refresh, and the scoped deploy credential are operator/infra concerns documented
in the operators' private runbooks.
