# images/control-plane/Dockerfile
# Multi-stage build for the Kyber control-plane binary.
# Built+pushed per-PR as cp:<pr.head.sha> for the deploy-to-dev substrate
# (kyber#544); main pushes additionally move :latest.
#
# The Dockerfile is self-contained: it builds the PWA in a Node stage and the
# Go binary in a Go stage, so CI does not need a separate `make pwa-build`
# step. Without this, the embedded pwa_dist/ directory would only contain the
# git placeholder and the PWA would render a "not built" message.

# Stage 1: build the PWA (React + Vite + Tailwind) inside the npm workspace.
# packages/pwa-views is the published library (@matty-v/kyber-pwa-views) and
# apps/embedded-pwa is the thin shell that gets embedded into the Go binary.
FROM node:22-alpine AS pwa-builder
WORKDIR /build

# Copy workspace metadata first so npm ci can install before the rest of the
# source changes invalidate the layer cache.
COPY package.json package-lock.json ./
COPY packages/pwa-views/package.json ./packages/pwa-views/package.json
COPY apps/embedded-pwa/package.json ./apps/embedded-pwa/package.json

RUN npm ci

# Copy the rest of the workspace source.
COPY packages/pwa-views ./packages/pwa-views
COPY apps/embedded-pwa ./apps/embedded-pwa

# Build the library first, then the embedded app (which consumes it via the
# workspace symlink — same version means npm prefers the workspace over the
# registry, so no auth needed at build time).
RUN npm run build --workspace=packages/pwa-views
RUN npm run build --workspace=apps/embedded-pwa

# Stage 2: build the Go binary. Pull the PWA dist into pkg/api/pwa_dist/
# before `go build` so //go:embed picks up the real assets.
FROM golang:1.25-alpine AS go-builder
ARG BUILD_SHA=""
ARG BUILD_DATE=""
# BUILD_VERSION is the user-facing release version (kyber#482). Set from the
# release tag (v stripped) on release.yml, or `git describe --tags` on the
# build.yml :latest/canary path. Empty on local/preview builds → the binary
# falls back to the chart-rendered version file.
ARG BUILD_VERSION=""
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Replace the gitkeep placeholder with the built PWA.
RUN rm -rf pkg/api/pwa_dist
COPY --from=pwa-builder /build/apps/embedded-pwa/dist pkg/api/pwa_dist
RUN CGO_ENABLED=0 GOOS=linux go build \
  -ldflags "-X main.BuildSHA=${BUILD_SHA} -X main.BuildDate=${BUILD_DATE} -X main.Version=${BUILD_VERSION}" \
  -o /build/control-plane ./cmd/control-plane
# transcript-compact (kyber#458): the on-demand transcript reclaim CLI, shipped
# in the same image so the helm-gated compaction Job can run it via this image
# with a different entrypoint. Not started by the default ENTRYPOINT.
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/transcript-compact ./cmd/transcript-compact

# Stage 3: minimal runtime image.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=go-builder /build/control-plane /usr/local/bin/control-plane
COPY --from=go-builder /build/transcript-compact /usr/local/bin/transcript-compact
EXPOSE 8080 8081 8082 9090
ENTRYPOINT ["/usr/local/bin/control-plane"]
