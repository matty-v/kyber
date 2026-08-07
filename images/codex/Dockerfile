ARG BASE_IMAGE=kyber/runtime-base:local
FROM golang:1.25-alpine AS reporter-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/kyber-codex-reporter ./cmd/codex-reporter

FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Pinned to the version verified by the Codex V1 spike.
ARG CODEX_VERSION=0.146.0
RUN npm install -g @openai/codex@${CODEX_VERSION}
ENV KYBER_RUNTIME_DEFAULT_VERSION=${CODEX_VERSION}

COPY --chmod=0755 images/codex/start-codex.sh /usr/local/bin/start-codex.sh
# Shared identity-repo setup (kyber#676) — installed BESIDE the start script so
# the runtime resolves it as a sibling. Same file in every runtime image: this
# is the code that manages agent identity and git credentials, and two copies
# drifting apart is the failure this consolidation exists to prevent.
COPY --chmod=0755 images/shared/kyber-identity-repo.sh /usr/local/bin/kyber-identity-repo.sh
COPY --from=reporter-builder /build/kyber-codex-reporter /usr/local/bin/kyber-codex-reporter
RUN install -d -m 0755 /opt/kyber
COPY --chmod=0644 docs/agent-manual.md /opt/kyber/KYBER.md
COPY images/shared/skills/telegram-messaging /opt/kyber/skills/telegram-messaging
COPY images/shared/skills/discord-messaging /opt/kyber/skills/discord-messaging

ENV KYBER_START_CMD="/usr/local/bin/start-codex.sh"
