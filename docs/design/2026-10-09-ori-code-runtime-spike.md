# MAT-111: Ori Code as a Kyber runtime

Status: offline assessment; **no live Ori Code conformance evidence or runtime implementation proposed**. Date: 2026-10-09.

## Recommendation

Defer a separate Ori Code runtime. The immediate product outcome, choosing OpenRouter models for an agent, is already addressed by Kyber's Hermes direct route and is being assessed for managed Claude/Codex in MAT-106. Ori Code has a distinct local coding TUI and headless JSONL turn mode, but no requested Kyber user journey currently needs it instead of those existing harnesses. Adding it would be a fourth long-lived runtime, with separate lifecycle, channel, transcript, auth, usage, and task integration work.

Reopen this decision if a concrete operator request requires Ori Code's own features and a disposable canary meets Kyber's [v1 harness contract](../architecture/agent-harness-contract.md). This is a product recommendation from inspected behavior, not a claim that Ori Code cannot be integrated.

## Evidence and limits

- Inspected pinned Ori CLI `0.15.6+4855f79` using `ori help code`, `ori code daemon --help`, and `ori --help`. The binary and checksum are recorded in [MAT-106 PR #311](https://github.com/matty-v/kyber/pull/311). Ori Code starts a local runtime against the current directory; with no prompt it opens a TUI. `--prompt` or `--prompt-file` runs one unattended turn, and `--output jsonl` emits events plus a terminal result. `--resume` requires an exact session ID. A per-user daemon can outlive a single turn.
- Ori Code's default `--approvals self-drive` automatically approves commands and runs its shell without its own sandbox. `--approvals manual` retains prompts and its available shell sandbox, but a headless turn cannot answer approval prompts and rejects calls that need approval. Kyber would need an explicit, tested tool policy and an OS-level agent boundary; neither default can be mapped blindly onto Kyber's unattended channel turns.
- [Kyber's runtime documentation](../runtimes.md) currently describes Claude Code, Codex, and a Hermes preview. Hermes already accepts an agent-scoped OpenRouter key and model; the existing Codex custom inference path accepts a managed Responses endpoint. This assessment did not establish a differentiated Ori Code user outcome.
- The available OpenRouter key has no provider-enforced spend cap; Matt waived that limit for bounded investigation. Gcloud access was restored for separate MAT-106 harness canaries. No Ori Code model turn, Kubernetes pod, channel message, restart, or feature conformance test was run because this assessment found no distinct Kyber user journey requiring a fourth runtime. CLI help is evidence of advertised behavior only.

## Contract gaps before a reconsideration

| Surface | Required proof or implementation |
| --- | --- |
| Image and lifecycle (HC-01/02) | Pin Ori and its dependencies in a dedicated image; establish daemon/session ownership per agent and prove tmux readiness, graceful exit, crash recovery, and pod replacement. A shared per-user daemon must not bridge agent boundaries. |
| Identity and prompts (HC-03/04) | Show that Kyber identity, skills, startup prompt, MCP, and channel input enter the intended Ori session exactly once; prove transcripts and exact resume after restart. Do not assume a headless one-turn process meets the persistent interactive profile. |
| Auth and billing (HC-05/10) | Use an agent-scoped Secret, reject missing/invalid credentials distinctly, pin model and provider policy, disable implicit fallbacks/updates, and reconcile Ori usage with OpenRouter billing. Use a provider-enforced cap by default for paid canaries or record an explicit operator waiver for a bounded investigation. |
| Jobs, tasks, cancellation (HC-06/07/08) | Implement and test correlated pre-model receipts and completion hooks before advertising durable tasks; define job hooks and cancellation evidence. JSONL events alone are not a Kyber receipt. |
| Capabilities and UI (HC-09/10) | Register truthful descriptor and availability probes, wire model selection and auth recovery, and add reporter, API, PWA, and contract fixtures only for supported features. |

## Reopening test

When a distinct use case exists, run a disposable, isolated Ori Code agent with `--approvals manual` first, using a capped key by default or an explicit bounded-investigation waiver. Verify a read-only turn, a blocked write/tool call, an allowed tool call under explicit policy, a channel round trip, session resume, pod replacement, auth rotation, model change, and usage against provider records. Test `self-drive` only after Kyber's OS and tool boundaries are demonstrated. Record failures and unsupported features in the lifecycle evidence catalog. Any implementation needs its own reviewed issue and rollout plan.

## Review notes

This proposal deliberately treats CLI help as interface evidence and labels every untested runtime behavior as a gap. It does not claim that Hermes and Ori Code are functionally equivalent; the current recommendation is based on the absence of a distinct Kyber user requirement and the work needed to validate another runtime.
