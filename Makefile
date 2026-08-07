# Kyber — Agent Fleet Platform
# Makefile for build, test, generate, and image targets.

CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null || echo $(GOBIN)/controller-gen)
GOBIN ?= $(shell go env GOPATH)/bin

CONTROLLER_GEN_VERSION ?= v0.16.5
CRD_OUTPUT_DIR ?= deploy/helm/kyber/crds

IMAGE_PREFIX ?= kyber
CONTROL_PLANE_IMAGE  ?= $(IMAGE_PREFIX)/control-plane
NODE_AGENT_IMAGE     ?= $(IMAGE_PREFIX)/node-agent
AGENT_BASE_IMAGE     ?= $(IMAGE_PREFIX)/runtime-base
CLAUDE_CODE_IMAGE    ?= $(IMAGE_PREFIX)/claude-code
STATUS_SIDECAR_IMAGE ?= $(IMAGE_PREFIX)/status-sidecar

HELM_CHART_DIR ?= deploy/helm/kyber
HELM_RELEASE   ?= kyber

.PHONY: build test lint generate tools-install build-images build-control-plane-image \
        build-node-agent-image build-status-sidecar-image image-list pwa-build pwa-dev \
        helm-lint helm-template helm-install-k3d

## pwa-build: install npm deps, build the embedded PWA, and copy dist to pkg/api/pwa_dist/
pwa-build:
	npm ci
	npm run build --workspace=apps/embedded-pwa
	rm -rf pkg/api/pwa_dist
	cp -r apps/embedded-pwa/dist pkg/api/pwa_dist

## pwa-dev: run the embedded PWA dev server with hot reload
pwa-dev:
	npm ci
	npm run dev --workspace=apps/embedded-pwa

## build: compile all Go packages (does NOT run pwa-build; use pwa-build separately)
build:
	go build ./...

## test: run all tests
test:
	go test ./...

## lint: run go vet (placeholder — full linting configured in A2)
lint:
	go vet ./...

## tools-install: install required code-generation tools
tools-install:
	GOBIN=$(GOBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

## generate: generate CRD manifests from kubebuilder markers
generate: tools-install
	$(GOBIN)/controller-gen crd paths=./pkg/api/v1/... output:crd:dir=$(CRD_OUTPUT_DIR)
	$(GOBIN)/controller-gen object paths=./pkg/api/v1/...

## build-control-plane-image: build the control-plane container image locally (used by e2e harness)
build-control-plane-image:
	docker build -t kyber/control-plane:local -f images/control-plane/Dockerfile .

## build-node-agent-image: build the node-agent container image locally
build-node-agent-image:
	docker build -t kyber/node-agent:local -f images/node-agent/Dockerfile .

## build-status-sidecar-image: build the status-sidecar container image locally (kyber#248)
build-status-sidecar-image:
	docker build -t kyber/status-sidecar:local -f images/status-sidecar/Dockerfile .

## build-images: build all container images locally (control-plane, node-agent, status-sidecar, runtime-base, claude-code)
build-images: build-control-plane-image build-node-agent-image build-status-sidecar-image
	docker build -t kyber/runtime-base:local images/agent-base
	docker build --build-arg BASE_IMAGE=kyber/runtime-base:local -t kyber/claude-code:local -f images/claude-code/Dockerfile .

## image-list: print all managed image names
image-list:
	@echo $(CONTROL_PLANE_IMAGE) $(NODE_AGENT_IMAGE) $(STATUS_SIDECAR_IMAGE) $(AGENT_BASE_IMAGE) $(CLAUDE_CODE_IMAGE)

## helm-lint: lint the Helm chart (requires helm CLI)
helm-lint:
	helm lint $(HELM_CHART_DIR) \
		--set api.apiKey=test123 \
		--set api.webhookSecret=webhook123 \
		--set k3s.joinToken=K10abc \
		--set k3s.serverUrl=https://10.0.0.1:6443 \
		--set image.controlPlane.tag=local \
		--set image.nodeAgent.tag=local \
		--set image.statusSidecar.tag=local \
		--set image.claudeCode.tag=local \
		--set image.codex.tag=local

## helm-template: render chart templates to stdout (useful for inspection/dry-run)
## Image tags are required at render time (no Chart.AppVersion fallback; kyber#358),
## so pin placeholders for local rendering.
helm-template:
	helm template $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--set api.apiKey=test123 \
		--set api.webhookSecret=webhook123 \
		--set k3s.joinToken=K10abc \
		--set k3s.serverUrl=https://10.0.0.1:6443 \
		--set image.controlPlane.tag=local \
		--set image.nodeAgent.tag=local \
		--set image.statusSidecar.tag=local \
		--set image.claudeCode.tag=local \
		--set image.codex.tag=local

## helm-install-k3d: install the chart on a local k3d cluster (requires k3d + kubectl).
## The control-plane image must exist or be skipped via imagePullPolicy=Never + local load.
## Real install is not tested in D1 (image not yet published); use helm-template + kubectl dry-run.
helm-install-k3d:
	k3d cluster create kyber-d1 --no-rollback || true
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--set api.apiKey=test123 \
		--set api.webhookSecret=webhook123 \
		--set k3s.joinToken=K10abc \
		--set k3s.serverUrl=https://10.0.0.1:6443 \
		--set image.controlPlane.tag=local \
		--set image.nodeAgent.tag=local \
		--set image.statusSidecar.tag=local \
		--set image.claudeCode.tag=local \
		--set postgresql.enabled=false \
		--set redis.enabled=false \
		--set nodeAgent.enabled=false \
		--wait --timeout=120s
