# images/node-agent/Dockerfile
# Multi-stage build for the Kyber node-agent binary.
# Deployed as a DaemonSet via the Helm chart — mounts /proc readonly and runs
# privileged so it can execute /sbin/shutdown for host actions.

FROM golang:1.26-alpine AS builder
WORKDIR /build

# Download dependencies before copying source to maximise layer cache reuse.
COPY go.mod go.sum ./
RUN go mod download

# Copy all source and build the binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/node-agent ./cmd/node-agent

# Runtime image: Ubuntu 24.04 for the shutdown utility and compatibility with
# host bind mounts (/proc, /sbin). Alpine's busybox shutdown would not match
# the host's systemd-shutdown semantics.
FROM ubuntu:26.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/node-agent /usr/local/bin/node-agent

# Default entrypoint: metrics loop mode (no args).
# Action mode is invoked via `node-agent action reboot|stop` from the control plane.
ENTRYPOINT ["/usr/local/bin/node-agent"]
