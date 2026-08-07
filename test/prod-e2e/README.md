# Prod-E2E Tests

These tests have moved to [matty-v/kyber-deploy](https://github.com/matty-v/kyber-deploy/tree/main/test/prod-e2e).

## Why they moved

Prod-e2e tests exercise the **deployed environment**, not the code — they belong alongside the deployment config (values.yaml, ArgoCD Application manifests, bootstrap scripts) rather than next to the source they were spun out of.

## How to run

Clone kyber-deploy and run from there. The workflow is manual (`workflow_dispatch`) on GitHub Actions.
