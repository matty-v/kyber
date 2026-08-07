# Codex CLI spike notes

- Verified 2026-08-03 with official `@openai/codex` 0.146.0 on Debian
  Bookworm and Node.js 22.
- ChatGPT subscription credentials are stored in `$CODEX_HOME/auth.json`.
  `codex login status` is the non-interactive credential check.
- Interactive Codex runs correctly inside the platform tmux session named
  `agent`; `codex resume --last` is available for later automatic resume work.
- Sessions and rollout JSONL live below `$CODEX_HOME/sessions` and include
  token-count events. A runtime-specific reporter remains required before the
  Metrics token card is considered complete.
- ChatGPT subscription model IDs verified from the current official manual are
  `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna`; older Codex model IDs are
  rejected for ChatGPT-authenticated sessions.
- The runtime forces file-backed credentials, no approvals, and full filesystem
  access because the Kubernetes pod is Kyber's isolation boundary.
