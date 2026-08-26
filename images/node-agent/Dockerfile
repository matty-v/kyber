# images/node-agent/Dockerfile
# Multi-stage build for the Kyber node-agent binary.
# Deployed as a DaemonSet via the Helm chart — mounts /proc readonly and runs
# privileged so it can execute /sbin/shutdown for host actions.

FROM golang:1.27-alpine AS builder
WORKDIR /build

# Download dependencies before copying source to maximise layer cache reuse.
COPY go.mod go.sum ./
RUN go mod download

# Copy all source and build the binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/node-agent ./cmd/node-agent

# Runtime image: Ubuntu 26.04 for the shutdown utility and compatibility with
# host bind mounts (/proc, /sbin). Alpine's busybox shutdown would not match
# the host's systemd-shutdown semantics.
FROM ubuntu:26.04

# CA bundle is copied from the builder rather than installed with apt. The
# binary is CGO_ENABLED=0, so it only ever reads this file — nothing in the
# runtime stage needs a package manager. Installing it with apt made every
# release build depend on ports.ubuntu.com serving an arm64 index consistent
# with its pool; a mirror node that was out of sync failed two v1.1.0 release
# runs on an openssl 404 that had nothing to do with the change being shipped.
# The builder is guaranteed to have the bundle: `go mod download` above fetches
# over HTTPS and would fail without it.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /build/node-agent /usr/local/bin/node-agent

# Default entrypoint: metrics loop mode (no args).
# Action mode is invoked via `node-agent action reboot|stop` from the control plane.
ENTRYPOINT ["/usr/local/bin/node-agent"]
