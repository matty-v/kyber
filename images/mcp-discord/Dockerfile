# images/mcp-discord/Dockerfile
# Multi-stage build for kyber-mcp-discord (kyber#646) — the Discord channel
# sidecar. Runs as a sidecar container in a Discord-enabled agent pod: holds
# the Discord Gateway connection and bridges inbound messages onto the agent's
# inbound binding. Distroless final image to keep the attack surface minimal
# (it carries a bot token).

FROM golang:1.25-alpine AS builder
WORKDIR /build

# Download deps first to maximise layer cache reuse.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build the binary statically.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/kyber-mcp-discord ./cmd/kyber-mcp-discord

# ca-certificates (via distroless base) so HTTPS to Discord's Gateway/REST and
# to the in-cluster control plane both work.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/kyber-mcp-discord /usr/local/bin/kyber-mcp-discord

# Reads the bot token + HMAC secret from Secret env refs; writes nothing to
# disk. nonroot (uid 65532) is a clean fit.
ENTRYPOINT ["/usr/local/bin/kyber-mcp-discord"]
