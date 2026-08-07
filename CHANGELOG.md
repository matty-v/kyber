# Changelog

> Kyber's public versioning starts at **v1.0.0**. The project was developed
> privately through an internal release line (v1.3.6–v2.7.1) whose history,
> tags, and changelog live in a private archive repository. This changelog
> covers the open-source line only.

## Unreleased

Initial public release (targeting **v1.0.0**): the full Kyber platform —
control plane, node agent, Agent/Machine CRDs, Claude Code and Codex runtimes,
Telegram/Discord channel sidecars, the PWA, Helm chart, and Terraform
profiles — licensed under Apache 2.0.

Notable changes made on top of the final internal release (v2.7.1) while
preparing the code for open source:

- Fix (api, security): **`?token=` API-key auth is now accepted only on WebSocket upgrade requests.** The query-parameter fallback exists because browsers cannot set an `Authorization` header during the WS handshake, but it was honoured on every route — so a full-scope key pasted into a plain REST URL would land in proxy/ingress access logs and browser history. Plain REST requests must use `Authorization: Bearer`; the PWA already does, and its WebSocket connects (`/events`, exec) are unaffected.
- Docs: installation, operations, and architecture docs generalized for public use (placeholder domains/projects/IDs, generic cluster naming conventions, self-contained `helm install` path as the supported deployment route).
- Chore: repository licensed under Apache 2.0 with standard community files (SECURITY.md, CODE_OF_CONDUCT.md, CONTRIBUTING.md, issue/PR templates).
