# images/status-sidecar/Dockerfile
# Multi-stage build for the kyber-status-sidecar (kyber#248). Runs as a
# sidecar container in every agent pod. Tiny — push-only HTTP, no
# kubectl/k8s-client deps. Final image is distroless to keep the
# attack surface minimal.

FROM golang:1.27-alpine AS builder
WORKDIR /build

# Download deps first to maximise layer cache reuse.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build the binary statically.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/kyber-status-sidecar ./cmd/status-sidecar

# Runtime image: distroless/static keeps the container <10 MB and gives
# us nothing for an attacker to pivot through if the binary is ever
# compromised. ca-certificates included so HTTPS to the control plane
# works.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/kyber-status-sidecar /usr/local/bin/kyber-status-sidecar

# distroless/static-debian12:nonroot runs as uid 65532 by default. The
# sidecar reads the pod-token from a Secret volume (read-only) and writes
# nothing to disk; nonroot is a clean fit.

ENTRYPOINT ["/usr/local/bin/kyber-status-sidecar"]
