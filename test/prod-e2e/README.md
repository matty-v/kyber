# Prod-E2E Tests

Production end-to-end tests live **out of this repo**, alongside deployment
configuration in the operator's deploy repo (per-environment `values.yaml`,
ArgoCD Application manifests, bootstrap scripts).

## Why out-of-repo

Prod-e2e tests exercise the **deployed environment**, not the code — they
belong next to the environment's deployment config rather than next to the
source they were spun out of. If you operate your own Kyber install via a
GitOps repo, that repo is the natural home for your prod-e2e suite too.

(The maintainer's suite lives in a private deploy repo; this directory is kept
as a pointer so the repo layout documents where that testing tier sits.)
