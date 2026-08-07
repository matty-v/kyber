# ADR 0002 — File-Anatomy Index for Agent-Edited Codebases

**Status:** Accepted — close as marginal benefit, do not productionize · 2026-05-07
**Context issue:** [kyber#173](https://github.com/matty-v/kyber/issues/173)
**Decider:** Matt (via Dave)
**Related:** [ADR 0001](./0001-memory-system.md) (parent memory decision; see "do nothing" framing)

## Summary

Kyber#173 asked whether maintaining a per-codebase **file-anatomy index** — a markdown table of files with token estimates and one-line "purpose" descriptions, kept in sync via a hook on Write/Edit — would measurably reduce the agents' file-read cost when working on Kyber tasks. The pattern is borrowed from openwolf (cytostack/openwolf), which claims 71% repeated-read elimination and 65.8% token reduction across 20 projects.

**Recommendation: close as marginal benefit. Do not productionize today.**

The A/B spike below shows a real ~40% reduction in tool-call count when an agent has an anatomy in context — but tokens *increased* by ~47% (the anatomy table itself costs more than the reads it saved), and the agent without the anatomy actually identified one *additional* legitimate concern (GPU node-scheduling / tolerations) that the anatomy-primed agent missed. The pattern's claimed wins do not carry over to Kyber's environment for two structural reasons documented below.

This ADR closes kyber#173. Triggers that would reopen the question are listed in the Decision section.

## The spike

### Setup

Generated a heuristic anatomy of `pkg/`, `cmd/`, `images/`, `pwa/src/` — 194 files, sorted by token estimate desc, with first-comment "purpose" lines extracted. Total table size: ~5K tokens. Generation script: `find + wc + awk` extracting the first non-empty doc comment. No LLM calls.

Dispatched two parallel `general-purpose` subagents on the same task:

> "If a Kyber operator wanted to add a new field `Spec.Resources.GPU string` to the Agent CRD, what files would they need to change to wire it through end-to-end? Don't write any code — just enumerate the touch-points and explain the role of each."

- **Agent A** received only the task. No prior context.
- **Agent B** received the task plus the 5K-token anatomy in its initial prompt, with instructions to use it as a triage signal and skip Read calls where the anatomy already told it what it needed.

Both agents were instructed to self-report their tool-call counts and time-to-confidence at the end of their reply.

### Results

| Metric | Agent A (no anatomy) | Agent B (with anatomy) | Delta |
|---|---|---|---|
| Read calls | 7 | 7 | 0 |
| Bash calls (mostly `grep` sweeps) | 16 | 9 | **−44%** |
| Total tool uses | 34 | 20 | **−41%** |
| Total tokens | 67,821 | 99,915 | **+47%** |
| Time-to-confidence | ~12 min | ~10 min | −17% |
| Distinct touch-points identified | ~25 | ~25 | 0 |

Both agents converged on the same headline files (`agent_types.go`, `routes_agents.go`, `pod_builder.go`, `capacity.go`, `machine_available.go`, the PWA wizard chain, the `_test.go` fixture sweep). Agent A independently surfaced one concern Agent B missed: kubelet pod-scheduling for GPU resources (`nodeSelector` / `Tolerations` are not currently in `pod_builder.go`, so adding `Spec.Resources.GPU` doesn't fully wire up unless those are also added). Agent B's confidence in the anatomy let it skip the broader `grep "nvidia\|nodeSelector"` sweep that surfaced the gap for A.

Agent B also explicitly noted blind spots in the anatomy:

> "What it did NOT cover: `pkg/api/machine_available.go` (not in the table), the wizard's `types.ts` / `validation.ts` / `ReviewSection.tsx` / `WizardCapacityCard.tsx` / `capacity.ts` / `machineTypes.ts` (smaller files dropped by the heuristic), and all the `_test.go` files. Those forced me into Bash-grep sweeps. The anatomy is great at 'where does the real logic live' but blind to 'what tests assert these literals'."

### Why tokens went *up*, not down

The 5K anatomy table is a fixed cost paid every task. To break even on tokens, the anatomy would need to reduce per-task Read cost by ≥5K tokens. In this spike both agents Read the same number of files (7 each), with similar excerpt sizes — the anatomy didn't shrink Reads, it shrank `grep` sweeps. Bash/grep responses are cheap (a few hundred tokens each). 7 grep sweeps × ~300 tokens ≈ 2K saved, less than the 5K table cost.

Openwolf's 65.8% token-reduction claim (per cytostack/openwolf README) measures something different: their pattern catches *repeated* reads of the same file across long sessions. Kyber's agents typically work on focused, time-bounded tasks of ≤30 min where each file is read once. Claude Code's prompt caching also amortizes any repeat-read cost we *would* have incurred. The Kyber environment doesn't have the multi-day-session re-read pattern openwolf optimizes for.

### Why agent B missed the GPU-scheduling concern

Agent A grepped broadly (`nvidia\|GPU\|nodeSelector\|Tolerat`) as part of pattern-discovery. Zero hits told A something non-obvious: GPU scheduling isn't yet wired anywhere in the codebase, so the touch-point list is incomplete without that work. Agent B, primed with a curated table, went directly to known files and didn't perform that exploratory sweep. The anatomy's signal — "here are the relevant files" — implicitly de-incentivized the question "what relevant files are *missing* today?"

This is a class of failure: **anatomy creates a closed-world bias** where the agent stops asking "what isn't here that should be?"

## Costs of productionizing

If we shipped this:

- **Hook + script** to regenerate `ANATOMY.md` on Write/Edit. Generation is ~1s; commit churn would be high (every file change updates the table). The commit-noise alone is meaningful.
- **Staleness risk**: anatomy comments derived from the first doc comment are only as accurate as the comment. A file refactor that doesn't touch the doc comment leaves the anatomy misleadingly authoritative.
- **Heuristic blind spots**: small files (under ~500 tokens) get dropped, tests get dropped — exactly the files agents need to remember to update on cross-cutting changes. Including them inflates the table back into the noise floor.
- **Maintenance**: if hand-curated, the anatomy becomes a documentation-debt vector ("anatomy says X but the file does Y"). If auto-generated, see staleness above.

For a 2K-line ROI of ~40% fewer grep sweeps on cold-start tasks, this is too much surface area.

## Decision

**Do not productionize file-anatomy as a standing pattern in Kyber repos.** The benefits (tool-call efficiency on cold-start file-discovery) are real but small, and they trade against (a) net token *increase*, (b) confidence bias that hides absent-feature gaps, and (c) maintenance + commit-noise costs.

### Triggers that would reopen this decision

- **A new agent runtime is added** (e.g. Hermes, OpenClaw, Codex per kyber#137/#230/#133) and we observe fresh agents wasting tool calls on initial codebase navigation. The cold-start case is the strongest pro-anatomy case.
- **Agents start working in significantly larger repos** (≥1000 source files) where grep sweeps cost more than they do today.
- **Claude Code's prompt cache changes** in a way that re-read amortization no longer holds. Today the cache absorbs repeat-read cost; if that changes, anatomy's cost model shifts.
- **A specific recurring failure** — e.g. agents systematically missing a file on a class of cross-cutting change — that anatomy could plausibly fix.

### Limited adopt: keep the generator script

The generation script (`scripts/gen-anatomy.sh`) is added to the repo as a one-shot tool an operator can invoke ad hoc when planning a large refactor. Not committed output, not auto-maintained, not loaded into agent context by default. Use case: "I'm planning a 30-file refactor; generate me a snapshot of what's there." This captures the optionality without paying the standing cost.

## Out of scope

- Replacing or extending [ADR 0001](./0001-memory-system.md) (agent memory). This ADR is about the *target codebase*, not agent identity.
- Other openwolf features (token-ledger, bug-log) — those are separate ideas; this issue was anatomy only.
- Adopting openwolf wholesale — already addressed in ADR 0001's "do nothing" framing.

## Methodology notes (for the next person reopening this)

- The anatomy script is committed as `scripts/gen-anatomy.sh`. Run from repo root: `./scripts/gen-anatomy.sh > /tmp/anatomy.md`. Token estimate is `chars / 4`; "purpose" is the first non-empty doc comment.
- Single-task A/B isn't statistically significant; if reopening, run on at least 5 task types (bug fix, refactor, feature add, test write, doc update). The token-cost result is likely robust; the tool-call result may vary by task.
- Subjective time-to-confidence isn't reliable across agents. Wall-clock time-to-final-answer is a better metric and was a tie within noise here.
- Agent B's "I didn't grep for missing patterns" failure mode is the most important finding. Any future productionization should explicitly counter the closed-world bias (e.g. with a "what's NOT in the anatomy?" prompt).

## Artifacts

- A/B subagent transcripts: dispatched 2026-05-07; results captured inline above.
- Anatomy generator: `scripts/gen-anatomy.sh` (this PR).
