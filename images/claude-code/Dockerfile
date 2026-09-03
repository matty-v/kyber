# BASE_IMAGE defaults to the local tag produced by `make build-images`.
# CI (build.yml push-images job) overrides it with the SHA-pinned GHCR image,
# which is guaranteed to exist at this step because runtime-base publishes first.
# Declared before the first FROM so it is a global ARG, usable in both stages.
ARG BASE_IMAGE=kyber/runtime-base:local

# Build the token-reporter binary from source. Multi-stage keeps the runtime
# image free of the Go toolchain.
FROM golang:1.27-alpine AS tokenreporter-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/kyber-token-reporter ./cmd/token-reporter
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/kyber-skills ./cmd/kyber-skills

FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive

# Node.js 22 (Claude Code requirement) via NodeSource
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && \
    apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

# Claude Code CLI — pinned for reproducible builds (kyber#175).
# Source of truth is the chart's runtime.claudeCode.version (kyber#175
# Phase 2): CI reads it via yq and passes --build-arg CLAUDE_CODE_VERSION
# to every fleet build. This ARG default is the fallback for ad-hoc local
# builds (`docker build` without --build-arg); a drift-detector test in
# pkg/runtimes/claudecode/ enforces that this default and the chart value
# stay in sync. The ENV is carried into the running container so
# start-claude.sh can log and compare against the actually-installed version.
ARG CLAUDE_CODE_VERSION=2.1.119
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}
ENV KYBER_RUNTIME_DEFAULT_VERSION=${CLAUDE_CODE_VERSION}

# Bun runtime (required by the Telegram channel plugin)
# Install to /root/.bun then copy (not symlink) to /usr/local/bin so the
# non-root kyber user can access it (/root is mode 700).
RUN apt-get update && apt-get install -y --no-install-recommends unzip \
    && curl -fsSL https://bun.sh/install | bash \
    && cp /root/.bun/bin/bun /usr/local/bin/bun \
    && rm -rf /var/lib/apt/lists/* /root/.bun

# GitHub CLI (for repo operations)
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | \
    dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | \
    tee /etc/apt/sources.list.d/github-cli.list > /dev/null && \
    apt-get update && apt-get install -y --no-install-recommends gh && \
    rm -rf /var/lib/apt/lists/*

# kubectl (deploy/release agents need it for /deploy-to-dev and cluster ops; a
# per-pod runtime bootstrap can't survive a fresh boot — kyber#572). Pinned via
# ARG for reproducibility; default tracks the fleet's k8s (kyber-falcon v1.35.x).
# kubectl is ±1 minor version-skew tolerant against the API server.
ARG KUBECTL_VERSION=v1.35.4
RUN curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/$(dpkg --print-architecture)/kubectl" \
        -o /usr/local/bin/kubectl && \
    chmod 0755 /usr/local/bin/kubectl && \
    kubectl version --client

# Claude Code start script
COPY --chmod=0755 images/claude-code/start-claude.sh /usr/local/bin/start-claude.sh
COPY --chmod=0755 images/agent-base/scripts/kyber-task-receipt /usr/local/bin/kyber-task-receipt
COPY --chmod=0755 images/shared/kyber-harness-install /usr/local/bin/kyber-harness-install
COPY --chmod=0755 images/shared/kyber-runtime-repair /usr/local/bin/kyber-runtime-repair
# Shared identity-repo setup (kyber#676) — installed BESIDE the start script so
# the runtime resolves it as a sibling. Same file in every runtime image: this
# is the code that manages agent identity and git credentials, and two copies
# drifting apart is the failure this consolidation exists to prevent.
COPY --chmod=0755 images/shared/kyber-identity-repo.sh /usr/local/bin/kyber-identity-repo.sh

# Agent manual — the platform's own docs, written to the agent. start-claude.sh
# renders this into <launch dir>/.runtime/KYBER.md at boot so every agent reads a
# manual matching the platform version it is actually running on, rather than a
# frozen copy scaffolded into its identity repo months earlier. Baked here (not in
# agent-base) because agent-base's build context is images/agent-base/ and can't
# reach docs/; a future runtime image repeats this one line.
# NOTE: docs/agent-manual.md is in the claudeCode path filter in build.yml —
# editing the manual must rebuild this image or the fleet keeps the old text.
#
# Create /opt/kyber explicitly, BEFORE the COPY. `COPY --chmod=0644` applies that
# mode to the parent directories it implicitly creates as well, so the bare COPY
# left /opt/kyber at 0644 — no execute bit, therefore not traversable — and the
# unprivileged `kyber` user that start-claude.sh runs as could not see the file
# at all. The manual silently failed to render on every pod (kyber#653). Any
# future COPY into a new directory here needs the same treatment.
RUN install -d -m 0755 /opt/kyber
COPY --chmod=0644 docs/agent-manual.md /opt/kyber/KYBER.md
COPY images/shared/skills/telegram-messaging /opt/kyber/skills/telegram-messaging
COPY images/shared/skills/discord-messaging /opt/kyber/skills/discord-messaging
COPY images/shared/skills/a2a-client /opt/kyber/skills/a2a-client

# Kyber token-usage reporter — runs as a background process alongside
# Claude Code. See cmd/token-reporter/ for details.
COPY --from=tokenreporter-builder /build/kyber-token-reporter /usr/local/bin/kyber-token-reporter
# The agent's own tool for its skills: install/link/commit/push, and the
# scan+report the identity-repo sync fires at boot. See cmd/kyber-skills/.
COPY --from=tokenreporter-builder /build/kyber-skills /usr/local/bin/kyber-skills

ENV KYBER_START_CMD="/usr/local/bin/start-claude.sh"
