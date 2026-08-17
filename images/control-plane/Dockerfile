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
FROM node:26-alpine AS pwa-builder
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
FROM golang:1.26-alpine AS go-builder
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
# kyber-upgrade: the entrypoint of the self-upgrade Job. Same image, different
# binary, same reasoning as transcript-compact — the Job that replaces the
# control plane must not need an image of its own to build, publish and pin.
RUN CGO_ENABLED=0 GOOS=linux go build -o /build/kyber-upgrade ./cmd/kyber-upgrade

# Stage 3: minimal runtime image.
FROM alpine:3.24
RUN apk add --no-cache ca-certificates

# helm, for the self-upgrade Job (kyber-upgrade shells out to it).
#
# The CLI rather than the Helm Go SDK, deliberately: the SDK would pull ~100
# transitive modules (oras, containerd, docker) into a control plane whose
# dependency set is currently tight, quadrupling the supply-chain surface and
# the Dependabot load — to reimplement, less well, what the operator already
# runs by hand. Shelling out also makes the Job log literally the `helm upgrade`
# output, which is the upgrade record the design asks for.
#
# Version-pinned and checksum-verified: this binary rewrites the cluster, so a
# silently-swapped download is not an acceptable failure mode.
#
# Per-architecture, because this is the one download in the image that is not
# arch-neutral. TARGETARCH is supplied by buildx (`amd64`, `arm64`) and matches
# helm's own asset naming, so it selects both the tarball and its checksum. An
# unknown value fails the build rather than silently skipping helm — the
# self-upgrade Job would otherwise be broken only at upgrade time.
ARG HELM_VERSION=3.16.3
ARG HELM_SHA256_AMD64=f5355c79190951eed23c5432a3b920e071f4c00a64f75e077de0dd4cb7b294ea
ARG HELM_SHA256_ARM64=5bd34ed774df6914b323ff84a0a156ea6ff2ba1eaf0113962fa773f3f9def798
ARG TARGETARCH
RUN apk add --no-cache --virtual .helm-deps curl \
  && case "${TARGETARCH}" in \
       amd64) HELM_SHA256="${HELM_SHA256_AMD64}" ;; \
       arm64) HELM_SHA256="${HELM_SHA256_ARM64}" ;; \
       *) echo "unsupported TARGETARCH='${TARGETARCH}' for the pinned helm download" >&2; exit 1 ;; \
     esac \
  && curl -fsSL -o /tmp/helm.tgz "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" \
  && echo "${HELM_SHA256}  /tmp/helm.tgz" | sha256sum -c - \
  && tar -xzf /tmp/helm.tgz -C /tmp "linux-${TARGETARCH}/helm" \
  && mv "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm \
  && rm -rf /tmp/helm.tgz "/tmp/linux-${TARGETARCH}" \
  && apk del .helm-deps \
  && helm version --short

COPY --from=go-builder /build/control-plane /usr/local/bin/control-plane
COPY --from=go-builder /build/transcript-compact /usr/local/bin/transcript-compact
COPY --from=go-builder /build/kyber-upgrade /usr/local/bin/kyber-upgrade
EXPOSE 8080 8081 8082 9090
ENTRYPOINT ["/usr/local/bin/control-plane"]
