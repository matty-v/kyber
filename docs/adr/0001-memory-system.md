# ADR 0001 — Memory System for Kyber Agents

**Status:** Accepted — keep current model, no implementation follow-up · 2026-04-22
**Context issue:** [kyber#136](https://github.com/matty-v/kyber/issues/136)
**Decider:** Matt (via Dave)

## Summary

Kyber's current agent memory model is **git-backed markdown files** — each agent has an identity repo with a `memory/` directory, indexed by `MEMORY.md`, auto-committed on write. This ADR evaluates whether to replace or augment that model with a dedicated memory platform (mem0, Zep/Graphiti, Letta, Cognee), keep the current model, or adopt a **hybrid** where git remains the source of truth and a sidecar provides semantic retrieval over the same files.

**Recommendation: do nothing — the current Claude-Code-native, git-backed memory model is the right default and fits Kyber's needs today.**

All four third-party candidates (mem0, Graphiti, Letta, Cognee) were evaluated and each has a material fit or operational concern at our scale. The hybrid option (git + semantic sidecar) is the least-bad upgrade path if we ever need to move, but was rejected for now because **there is no observed retrieval miss or scale problem to solve**. The existing pattern — markdown files in `memory/`, indexed by `MEMORY.md`, auto-committed on write, read-on-demand by Claude Code's native startup walk-up — has proven sufficient in daily operation, including for the self-improvement loop (corrections → `feedback_*` memories → applied next session).

**This ADR is recorded as the decision and closes kyber#136.** Triggers that would reopen the question are listed in the Decision section.

## Context

### What we have today

Kyber agents (claude-code runtime) boot into their identity repo cloned from GitHub. Each repo looks like:

```
<agent>-agent/
├── CLAUDE.md                     # startup instructions, auto-loaded by Claude Code
├── identity/
│   ├── SOUL.md                   # role, values, communication style
│   └── PRINCIPLES.md             # evolving working agreement
├── memory/
│   ├── MEMORY.md                 # index: one line per memory
│   ├── feedback_*.md             # guidance from past conversations
│   ├── project_*.md              # project state snapshots
│   ├── user_*.md                 # user profile facts
│   └── reference_*.md            # pointers to external systems
├── state/                        # session summaries, last-session-tail
└── config/settings.json          # hooks, including auto-commit PostToolUse
```

Mechanics:

- **Boot:** `start-claude.sh` clones or pulls the identity repo to the pod volume (`images/claude-code/start-claude.sh:99-165`). Claude Code starts in that directory; its built-in CLAUDE.md walk-up reads `CLAUDE.md`, which explicitly instructs it to read `SOUL.md`, `PRINCIPLES.md`, `state/last-session-summary.md`, `.runtime/last-session-tail.md`, and `memory/MEMORY.md`.
- **Retrieval:** Only `MEMORY.md` (the index) is auto-loaded per session. Individual memory files are read on demand — the agent scans the index's one-line descriptions and chooses which files to `Read`. Retrieval is Claude-as-retriever: fuzzy semantic match against the English description, not embedding-based.
- **Writes:** The `/w` skill reviews a conversation and writes memory files. A PostToolUse hook on `Write|Edit` against `memory/` or `state/` paths runs `git add && commit && push` automatically (configured in the agent's identity repo `settings.json`).
- **Audit log:** Every memory change is a git commit with timestamp and path. `git log memory/foo.md` gives full revision history. `git blame` gives origin.
- **Backup:** The git remote on GitHub is the backup. Losing the cluster does not lose memories.
- **Scale today:** Dave has **96 memory entries** (~16 KB of index, ~2700 lines across all files). The index is loaded every session; individual files on demand.

### Why the issue was filed

Stated weak points from kyber#136:
1. **Context-window inefficiency** — index grows linearly; at ~1000 entries the index itself is multiple pages of tokens every boot.
2. **No semantic retrieval** — Claude-as-retriever misses memories whose filename/description don't match query vocabulary.
3. **Manual curation discipline** — no automatic fact extraction; `/w` runs on demand, not continuously.
4. **Scale ceiling** — thousands of memories makes the directory unreadable and boot cost painful.

The fleet has 5 agents today. Dave is the largest; others are early. The scale ceiling is a 6-18 month concern, not a this-month concern.

### Non-negotiable requirements (from kyber#136)

Any candidate that fails these is disqualified:

- **Self-hostable** — runs in our k3s cluster, no SaaS dependency.
- **Durable** — survives pod/namespace restart; snapshot-able to object storage.
- **Version-controllable or snapshot-restorable** — git's diff history is the baseline; alternatives need an audit log + snapshot story.
- **Runtime-agnostic** — works for claude-code today AND future codex/other runtimes; no coupling to one LLM SDK.
- **Per-agent isolation** — one agent's memory does not leak into another's.

## Evaluation

Each candidate is scored across 9 axes. Detailed findings follow; summary table at the end.

### Do-nothing baseline (git-backed markdown, current model)

**One-line description:** Markdown files in a per-agent git repo, indexed by MEMORY.md, auto-committed on write, read on demand by Claude.

**Architecture fit:** Zero new infrastructure. The identity repo is already mounted on pod boot. No sidecar, no service, no extra Secrets. Resource footprint: whatever `git clone/pull` costs on boot (~seconds) and the memory files on disk (<1 MB per agent).

**Storage backends:** Filesystem (pod volume) + GitHub (remote backup). No database. Whole-disk persistence (per-agent PVC) covers the pod-local side; the git remote covers the cluster-loss side. Scheduled off-cluster backup is tracked by kyber#171.

**API shape:** `Read` tool on `memory/*.md`. Writes via `Write`/`Edit` triggered by the `/w` skill. No programmatic search API — the index + Claude's reading comprehension is the query layer. Language-agnostic for writes; Claude-specific for retrieval (any runtime that can read markdown works equally well).

**Version control / history:** **Native.** Every write is a commit. `git log`, `git blame`, `git diff` all work. GitHub UI provides browsing. Restore-to-point-in-time is `git checkout <sha>`. This is the strongest axis.

**Retrieval quality:** Fuzzy. Claude scans the 16 KB index and decides what to read. Misses happen when the memory's description doesn't share vocabulary with the query. No embedding-based match, no reranking, no metadata filtering. Works surprisingly well at <100 entries because Claude's comprehension is strong; degrades as entries multiply and the index page count grows.

**Migration story:** N/A — this *is* the current state.

**Integration cost:** Zero.

**Operational complexity:** Zero services to babysit. Backup is git push (handled by hook). Restore is git clone. Runbook for identity-repo loss is already documented (kyber runbook).

**License:** N/A (git is MIT-ish; GitHub is the cloud provider).

**Verdict:** Meets all non-negotiable requirements trivially. Unique strengths: human-readable by design, full audit log via git, zero operational cost, already working. Unique weakness: no semantic retrieval and a real scale ceiling around O(1000) entries where the index becomes expensive to load.

---

### mem0

**One-line description:** Open-source memory layer for AI agents that extracts facts from conversations and stores them in a vector DB plus optional graph DB; ships as both a Python/TS library and a FastAPI REST server.

**Architecture fit:** Python-first with a FastAPI REST server; typical self-host is 3 containers via docker-compose — API + Postgres+pgvector + Neo4j (mem0.ai/blog/self-host-mem0-docker). In Kyber it would most naturally run as a per-agent sidecar or a shared cluster service with per-agent `user_id` scoping. Go control plane talks HTTP. Upstream sizing hint: 2 vCPU / 4 GB RAM, Neo4j dominating.

**Storage backends:** 20 vector stores supported in Python (Qdrant, Chroma, pgvector, Milvus, Pinecone, etc.); TS SDK is narrower. Server compose defaults to pgvector. Fact/history store is SQLite by default; graph store is Neo4j (swappable to Memgraph).

**API shape:** Python SDK + TS SDK + FastAPI REST. Core methods: `add()`, `search()`, `get_all()`, `update()`, `delete()`, `history(id)`, `reset()`. **No native Go SDK** — Kyber would call HTTP/JSON. OpenAPI at `/docs`.

**Version control / history:** `GET /v1/memories/{id}/history/` returns an append-only event log per memory (ADD/UPDATE/DELETE) with before/after values and timestamps. Not git-style branching/diff, no time-travel "as-of" reads. Platform-only features add export; OSS history is functional for audit.

**Retrieval quality:** Semantic + metadata filtering + optional hybrid (BM25 + semantic, requires `rank-bm25`). Default embedder is OpenAI `text-embedding-3-small`; swappable to Ollama `nomic-embed-text` for offline. Default LLM for fact extraction is OpenAI `gpt-5-mini`. Independent benchmark cites weakness at temporal retrieval (49% vs 63-91% competitors).

**Migration story:** No native markdown importer. Import = iterate `memory/*.md`, call `add(content, user_id=agent_name, metadata={source_path})`. Each file becomes one or more extracted "facts." **Export to markdown is Platform-only in OSS** — back-out requires `get_all()` + custom renderer. Embedder swaps mid-flight are destructive: wipe + re-ingest.

**Integration cost:** ~1-2 weeks for a clean integration: Helm sub-chart (3 containers), PVC + backup, secrets for OpenAI/Ollama, Go HTTP client in control plane, boot-time markdown import, retrieval plumbing. Could be 3-5 days for Ollama-only skip-graph-memory variant.

**Operational complexity:** 3 new pods per agent (or shared-cluster with `user_id` scoping). New Secrets: LLM API key, Postgres creds, Neo4j creds. Two stateful DBs to snapshot + SQLite history volume — doesn't align cleanly with kyber#171's git-of-repos GCS backup pattern. **Default deploy has no auth and CORS `allow_origins=["*"]`, binds `0.0.0.0`** — requires reverse proxy + NetworkPolicy before exposure. Upgrade = image bump, but embedder changes force full re-ingest.

**License:** Apache-2.0. OSS is feature-rich but not at parity with managed Platform: memory export, webhooks, custom categories, dashboard, and priority support are Platform-only (docs.mem0.ai/platform/platform-vs-oss).

**Verdict:** Meets all 5 non-negotiables. Standout strengths: broadest backend matrix in the space, real built-in edit-history endpoint, active community, Apache-2.0 core. Biggest concerns: 3-container footprint per agent is heavy vs current zero-infra model; no first-class markdown export means one-way lock-in risk; default deployment is security-wide-open; OSS-vs-Platform export gap is the clearest "crippled" signal.

---

### Zep (and Graphiti)

**One-line description:** The self-hostable `getzep/zep` ("Zep Community Edition") was **deprecated and archived to `legacy/` in April 2025**. The current OSS surface is `getzep/graphiti` — Apache-2.0 Python temporal knowledge-graph framework (core library + FastAPI server + MCP server) backed by Neo4j / FalkorDB / Kuzu / Neptune.

**Architecture fit:** Graphiti-core is a Python library; `server/` directory ships a FastAPI REST service as `zepai/graphiti` on Docker Hub. A Kyber pod would run two containers: graphiti API + a graph DB (Neo4j is heaviest at ~1-2 GB baseline; FalkorDB is a Redis module and much lighter; Kuzu is embedded).

**Storage backends:** Graph DB required — Neo4j (default), FalkorDB, Kuzu 0.11+, Neptune. Neptune additionally requires OpenSearch Serverless. **No pgvector / postgres option.** Default docker-compose uses Neo4j 5.22.

**API shape:** Three surfaces: (1) `graphiti-core` Python SDK; (2) FastAPI REST at `:8000/docs` — episodes ingested async; (3) MCP server for direct LLM tool-use. Python-only SDK — no Go/TS SDK in OSS (those are Zep Cloud only).

**Version control / history:** Graphiti is temporal by design — every fact/edge has validity windows (`valid_at`, `invalid_at`) and provenance back to source episodes. "What did we know when" is a first-class query. **Not the same as git diff-over-text** — no per-file log, no blame, no PR workflow. Backup = graph DB dump.

**Retrieval quality:** Hybrid — semantic + BM25 + graph traversal + cross-encoder reranking + graph-distance reranking. Embeddings pluggable (OpenAI, Gemini, Voyage, Ollama). **Every ingest triggers LLM calls for entity+relation extraction** — not free, requires LLM endpoint in pod's network path.

**Migration story:** Ingest is episode-shaped (`text`, `json`, `message`). Each markdown file → one text episode with `group_id = agent_name`. **Round-tripping back to `memory/*.md` is lossy** — source markdown isn't the unit of storage. No documented markdown export.

**Integration cost:** Weeks, not days. Helm chart for graphiti + graph-DB StatefulSet, per-agent `group_id` convention, ingestion job that walks `memory/` on boot, retrieval shim, LLM API key. Plus Cypher/graph debugging learning curve.

**Operational complexity:** Graph DB StatefulSet + graphiti API Deployment + Secrets + LLM spend per ingest. Multi-tenancy via `group_id` is clean in a single DB (matches one-group-per-agent). Upgrade = image bump; graph DB upgrades are their own story.

**License:** Graphiti Apache-2.0. Deprecated Zep CE was also Apache-2.0. Graphiti is explicitly feature-reduced vs Zep Cloud — no dashboard, no visualization UI, Python-only SDK in OSS.

**Verdict:** `getzep/zep` is **disqualified** — deprecated April 2025, no updates. Graphiti meets all five Kyber non-negotiables, but: (1) heavy — adds graph DB dependency and LLM-extraction spend per write; (2) lossy migration — markdown becomes extracted entities, not reversible; (3) OSS is intentionally reduced vs Cloud. Strongest temporal story of any candidate (validity windows); weakest alignment with Kyber's existing markdown-is-truth posture.

---

### Letta (née MemGPT)

**One-line description:** Python-based stateful-agent platform with a Postgres/pgvector-backed hierarchical memory subsystem. **Memory is exposed only as agent-scoped endpoints and cannot be cleanly detached from Letta's agent abstraction.**

**Architecture fit:** Letta is delivered as a server, run via docker-compose with three services: `letta_db` (pgvector), `letta_server`, `letta_nginx`. **No library mode** — all memory APIs live under `client.agents.*` and require an `agent_id` to address. Even the official "ai-memory-sdk" is a thin wrapper that spins up a backing Letta agent per subject.

**Storage backends:** Default is bundled Postgres with pgvector for vector search; `LETTA_PG_URI` override for external Postgres. SQLite exists in the codebase but Postgres+pgvector is the documented self-hosted path.

**API shape:** HTTP REST on port 8283 plus Python + TS SDKs. Memory ops usable standalone: `passages.insert/search/list/update/delete` for archival, block CRUD for core memory. **But every call is `agent_id`-scoped** — no subject/namespace primitive independent of an agent.

**Version control / history:** `BlockHistory` table keeps an append-only audit trail of memory-block edits. `.af` (Agent File) format supports full agent snapshot export/import — usable for checkpoint/restore but not git-diffable.

**Retrieval quality:** Three tiers: **core** (pinned system prompt, agent-editable), **recall** (conversation history, searchable), **archival** (pgvector semantic passages, on-demand via tool calls). Default embedding model not documented for self-hosted. Tag filter + semantic query; full hybrid search not documented.

**Migration story:** No documented markdown-import path. Ingestion = script `passages.insert(agent_id=..., content=..., tags=[...])` per file. Export = `.af` snapshot or raw passage list. ADE (web UI) is inspection, not bulk migration.

**Integration cost:** Memory-only adoption (one dummy Letta agent per Kyber agent): **~1-2 weeks**. Full Letta-as-runtime adoption: **weeks-to-months + rewriting the Claude Code driver** — disqualifying path.

**Operational complexity:** Minimum 3 containers (Postgres+pgvector, letta_server, nginx). Backup = pg_dump + `.af` exports. Running Python + Postgres ops surface on top of today's Go/k3s footprint.

**License:** Apache-2.0, fully open-core (not crippled).

**Verdict:** **Headline disqualifier — memory is not decoupled from Letta's agent abstraction.** Every memory call is `agent_id`-scoped; shoehorning it in means running the full letta-server + Postgres + pgvector + nginx stack to use ~10% of the product, with Letta's agent-loop code path as dead weight. Also flag: default embedding model undocumented for self-hosted, and no native markdown-import path. Real strengths (hierarchical memory design, BlockHistory audit, `.af` portability) don't overcome the architectural mismatch with Kyber's existing runtime.

---

### Cognee

**One-line description:** Open-source Python knowledge engine that ingests unstructured data, runs an LLM-driven ECL pipeline (Extract, Cognify, Load) to build an ontology-grounded knowledge graph, and serves it via graph + vector + relational stores behind a `remember`/`recall`/`forget`/`improve` API.

**Architecture fit:** Python library with embedded FastAPI/uvicorn and a separate MCP server. Default graph backend is Kuzu (embedded, no extra service); also supports Neo4j/Memgraph/FalkorDB. **Upstream docker-compose.yml sets `cpus: "4.0", memory: 8GB` as the limit** — that's the upstream's own sizing hint. In a pod with Kuzu-only defaults this is one process holding Kuzu + LanceDB files + SQLite in-proc.

**Storage backends:** Three stores are **mandatory**. Defaults are file-based, zero extra services: SQLite (relational) + LanceDB (vector) + Kuzu (graph). Heavyweight swap-ins: Postgres+pgvector, Neo4j 5.26 (with APOC + GDS plugins), Qdrant, Weaviate, Milvus, Redis. S3 supported for file-backed defaults.

**API shape:** Embeddable Python async SDK (`await cognee.remember(...)`, `recall`, `forget`, `improve`) plus a REST API server and an MCP server. SDK is Python-only; other runtimes hit HTTP/MCP. A Claude Code plugin exists (`cognee-integrations/integrations/claude-code`) that hooks SessionStart/PostToolUse/SessionEnd.

**Version control / history:** **No git-like history surfaced.** "Export Dataset Markdown" endpoint exists; `improve`/`forget` lifecycle documented. No commit/branch model, no time-travel query. Treat as snapshot-restore only.

**Retrieval quality:** Auto-routing `recall()` picks between vector similarity, graph traversal, and session cache. Graph is ontology-grounded (optional OWL via `ONTOLOGY_RESOLVER=rdflib`). Differentiator vs mem0: **multi-hop relationship queries and entity-linked retrieval across documents** — "which past cases share a root cause with this one" rather than just "top-k nearest notes." Depends on LLM call per ingest for entity extraction.

**Migration story:** Ingest is format-agnostic; markdown trivially covered (`pypdf`, `beautifulsoup4` in deps for richer formats). Export: "Export Dataset Markdown" produces a memory report. **No documented round-trip re-import of that markdown** — one-way lossy.

**Integration cost:** 1-2 weeks realistic. Day-scale to wire SDK into a bootloader and replace file-glob load with `recall()`. Week-scale with per-agent dataset isolation (`ENABLE_BACKEND_ACCESS_CONTROL`), Helm chart authoring, backup plumbing for three stores, LLM bill for cognify runs. Kuzu-only default lowers this vs Neo4j.

**Operational complexity:** Minimum footprint is one pod with Kuzu + LanceDB + SQLite on a PVC — snapshot to object storage and you're done. **That's the tractable path.** Adding Neo4j for scale inherits Neo4j's backup story + Postgres/Redis + APOC plugin pinning. Upstream's 4 CPU / 8 GB sizing hint is heavy for an 85-file markdown corpus.

**License:** Apache-2.0 (core + all backends). Paid "Cognee Cloud" tier exists but the repo is not open-core-gated.

**Verdict:** Cognee's **unique offering is ontology-grounded knowledge-graph retrieval** with multi-hop reasoning — nothing in mem0/Zep/Letta OSS matches it in a single Apache-2.0 package. For Kyber's stated needs (85 markdown files, boot-time load, pod restart durability) it is operationally oversized: three mandatory stores even in the lightweight default, LLM call per ingest, 4 CPU / 8 GB upstream sizing, no native version-control semantics, no markdown round-trip. Right answer for a future problem Kyber doesn't have yet.

---

### Hybrid (git source of truth + semantic-search sidecar)

**One-line description:** Keep markdown files in git as the authoritative memory store; add a sidecar (or cluster service) that indexes the same files into a vector DB and exposes `memory.search(query)` via MCP. Agent queries the index instead of loading `MEMORY.md` eagerly.

**Architecture fit:** One additional component — either a per-agent sidecar container in the pod, or a shared cluster service in `kyber-system`. The files remain in the identity repo; the index is regenerable from files. Sidecar reads memory/, embeds each file, serves search over HTTP/MCP. Stateless relative to the source files — crash-safe.

**Storage backends:** Files: identity repo (unchanged). Index: vector DB of choice — the choice doesn't much matter for hybrid because the files are the source of truth. Qdrant / pgvector / Chroma / even SQLite-FTS as a starter. The index is disposable; if it corrupts, regenerate from files.

**API shape:** Agent calls `memory.search(query, top_k=5)` via MCP. Gets back file paths + snippets. Then `Read`s the matched files. Still uses the existing `/w` skill to write — the sidecar watches `memory/` (inotify / fsnotify) and re-indexes on change. No API change to write path.

**Version control / history:** Unchanged from baseline — git still tracks every edit. Index is ephemeral, no version history needed for it.

**Retrieval quality:** Semantic search via embeddings, metadata filter by memory type (feedback/project/user/reference). Optional reranking. A reasonable implementation beats index-scan at >200 entries comfortably.

**Migration story:** **None required.** Files already exist; sidecar starts indexing them. Back-out: stop running the sidecar, MEMORY.md index still works, zero data loss.

**Integration cost:** ~1-2 weeks for a minimal sidecar (embed-on-change + in-process vector index + MCP tool) plus chart template. Longer if we want Qdrant/pgvector as a dedicated service.

**Operational complexity:** One more sidecar container per agent pod (or one cluster service with per-agent namespacing). Backup model inherits from git; the index itself does not need backup. Upgrade path: change embedding model → rebuild index (minutes) from files.

**License:** Depends on the vector DB chosen. pgvector is Postgres-licensed (permissive); Qdrant is Apache 2.0; Chroma is Apache 2.0. None of these are blockers.

**Verdict:** Meets all non-negotiable requirements. Unique strengths: no migration (files are already the right shape), preserves git audit log, back-out is trivial, semantic retrieval without surrendering human-readable source of truth. Unique weakness: two sources of derived truth (files + index) must stay in sync; index regeneration lag on write is a minor UX concern. This is the lowest-risk upgrade path.

---

## Summary table

Scoring scale: ✅ strong · ◐ acceptable with work · ❌ disqualified or fails requirement. Lower is worse.

| Axis | Baseline (git) | Hybrid (git + sidecar) | mem0 | Zep / Graphiti | Letta | Cognee |
|---|---|---|---|---|---|---|
| Self-hostable | ✅ | ✅ | ✅ | ✅ (graphiti) / ❌ (`getzep/zep` archived) | ✅ | ✅ |
| Durable | ✅ git remote | ✅ git remote | ◐ DB snapshots | ◐ graph DB dumps | ◐ pg_dump | ◐ PVC + S3 |
| Version history | ✅ git native | ✅ git native | ◐ event log, no time-travel | ◐ temporal edges | ◐ BlockHistory | ❌ snapshot only |
| Runtime-agnostic | ✅ | ✅ | ✅ REST | ✅ REST/MCP | ◐ agent-scoped only | ✅ REST/MCP |
| Per-agent isolation | ✅ one repo per agent | ✅ one repo per agent | ◐ `user_id` scoping | ✅ `group_id` | ✅ one Letta agent per | ◐ `ENABLE_BACKEND_ACCESS_CONTROL` |
| Retrieval quality | ❌ fuzzy / index-scan | ✅ semantic + metadata | ✅ semantic + hybrid | ✅ graph + hybrid + rerank | ✅ hierarchical + pgvector | ✅ graph + vector + ontology |
| Migration in | N/A | N/A (already in) | ◐ script per file → facts | ◐ lossy → entities | ◐ script per file → passages | ◐ format-agnostic |
| Migration out | N/A | N/A (files unchanged) | ❌ export is Platform-only | ❌ no markdown export | ◐ `.af` snapshot | ◐ "Export Markdown" one-way |
| Integration cost | zero | 1-2 weeks | 1-2 weeks (3-5 days minimal) | weeks (graph DB + Cypher) | 1-2 weeks memory-only / weeks-months full | 1-2 weeks |
| Ops complexity | zero | +1 sidecar | +3 pods per agent | +2 pods per agent (API + graph DB) | +3 pods (pg, server, nginx) | +1 pod Kuzu / +many Neo4j |
| License + open-core posture | MIT / N/A | MIT / N/A | Apache-2.0, export Platform-only | Apache-2.0, SDK coverage Cloud-only | Apache-2.0, fully open | Apache-2.0, fully open |

## Decision

**Keep the current Claude-Code-native, git-backed memory model. Do not adopt mem0, Zep/Graphiti, Letta, or Cognee at this time. Do not build the hybrid sidecar at this time.**

### Reasoning

1. **The current model is not a gap — it is Claude Code's native long-term memory.** The `memory/` directory + `MEMORY.md` index + CLAUDE.md walk-up + auto-commit hook is a working implementation of Claude Code's built-in memory primitive. Swapping it for mem0 would be replacing a working native mechanism with a third-party one, not filling a missing capability.
2. **Short-term / session continuity — Matt's stated pain point — is not what these tools solve.** Short-term recovery is handled by the Stop hook (`.runtime/last-session-tail.md`, verbatim tail) + the `/restart` skill (`state/last-session-summary.md`, narrative) + the CLAUDE.md resume handshake. mem0/Graphiti/Cognee are long-term memory systems, poorly shaped for turn-by-turn transcript state. Investing there does not improve session continuity.
3. **The self-improvement loop already works.** Corrections produce `feedback_*.md` memories via the `/w` skill; those memories are auto-loaded on every session boot via `MEMORY.md` and applied by the agent. The format encodes nuance (rule + why + how-to-apply) that LLM-based fact extraction in mem0/Cognee would flatten. The current file shape is load-bearing.
4. **Scale is not an active concern.** Dave — the largest memory profile in the fleet — has 96 entries / 16 KB index. Daily Claude-as-retriever performance is acceptable. No observed retrieval miss justifies a rebuild.
5. **Every third-party candidate has a concrete tradeoff that does not pay off today.**
   - Zep OSS (`getzep/zep`) is archived — **disqualified.**
   - Letta cannot cleanly detach memory from its agent runtime — **effectively disqualified** for a platform that already has runtimes.
   - mem0 requires 3 stateful containers per agent and has no OSS markdown export (one-way lock-in risk).
   - Cognee is operationally oversized (4 CPU / 8 GB upstream guidance, LLM call per ingest); its unique multi-hop graph retrieval is value we don't currently have demand for.
6. **If we did eventually need to move, the hybrid option (git + semantic-search sidecar) is the lowest-risk path** and the evaluation of mem0/Cognee as full-replacements is already captured above. This document is the cache for that future decision.

### Triggers that would reopen this question

- `MEMORY.md` index crosses **~500-1000 entries** per agent and the boot-time index-load cost is visibly expensive.
- Observed pattern of retrieval misses — Dave (or another agent) reports specific moments where a relevant memory existed but was not found, across a window large enough to be a pattern rather than noise.
- New demand for **multi-hop / relational queries** (e.g. "which past incidents share a root cause with this one") that index-scan + file read cannot serve — Cognee becomes the leading candidate at that point.
- Multi-agent **shared-memory** requirement emerges (currently explicitly out of scope; per-agent isolation is the V1 model).
- A future agent runtime (Codex, etc.) proves unable to work with the current file-based memory pattern.

### Rejected alternatives, briefly

- **Hybrid (git + semantic-search sidecar):** lowest-risk upgrade path; rejected for now because there is no observed problem to solve. Remains the recommended upgrade if any trigger above fires.
- **Full mem0 replacement:** best-in-class backend matrix and a real history endpoint, but the 3-container tax and no-export-in-OSS lock-in don't pay off at our scale.
- **Full Graphiti replacement:** strongest temporal story of any candidate, but lossy markdown migration and graph-DB dependency don't fit a platform where human-readable files are the intentional posture.
- **Full Letta replacement:** best hierarchical memory design on paper, but agent-scoped API + needing to run the full letta-server stack for a memory primitive is the wrong shape.
- **Full Cognee replacement:** only candidate with multi-hop knowledge-graph retrieval, but solving a problem we don't currently have.

## Migration plan

**None for this ADR cycle.** No changes to the current memory model. No follow-up implementation issue filed.

**If a trigger above fires, the recommended next step is:**

1. Re-open this ADR and update with the observed problem (specific retrieval misses, scale measurements, new use case).
2. Start with the **hybrid** option (git + semantic-search sidecar) unless the use case clearly demands graph/multi-hop (Cognee) or temporal reasoning (Graphiti).
3. Sidecar V1 sketch (cached here for that future session): in-process per-agent sidecar, SQLite-vec or in-memory vector store, Ollama embeddings (`nomic-embed-text`), file watcher on `memory/` with re-embed on change, single MCP tool `memory.search(query, top_k=5)` returning `[{path, snippet, score}]`, behind a per-agent CRD feature flag defaulted off, enable on Dave first, measure for 2-4 weeks.
4. **Do not** eager-adopt mem0/Graphiti/Letta/Cognee as first-move — their tradeoffs are captured in this ADR and none are worth the cost today.

## Out of scope

## Out of scope

- Implementation of the chosen system (goes to a follow-up issue).
- Replacement of identity files (`SOUL.md`, `PRINCIPLES.md`, `CLAUDE.md`) — those remain in git, human-edited.
- UX for browsing memories in the PWA.
- Multi-agent shared memory (each agent stays isolated in V1).
- Embedding model selection beyond "whatever the sidecar picks by default."

## Open questions

None for this decision cycle — the "do nothing" recommendation is unambiguous. The set of design questions that would need answers if a trigger reopens this evaluation (embedding model, sidecar topology, MEMORY.md retention, active vs passive writes) is captured in the Migration plan sketch above.
