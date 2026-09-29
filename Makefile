# Copyright 2026 the AMD Halo Operator authors
# SPDX-License-Identifier: Apache-2.0

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_TOOLS_VERSION ?= v0.19.0
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: build
build:
	go build ./...

ENVTEST_K8S_VERSION ?= 1.36.2
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.24.1

.PHONY: envtest-assets
envtest-assets: ## Download the control-plane binaries the envtest suite needs.
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)

.PHONY: test
test: ## Run every test, including the envtest suite.
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./...

.PHONY: test-unit
test-unit: ## Run only the tests that need no control plane.
	go test ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
# Scope generation to the real source trees. "./..." descends into any nested
# checkout under the working tree, such as a git worktree parked in .claude/,
# and silently merges its stale API markers into the generated CRDs and RBAC.
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=halo-operator-role \
		paths="./api/..." paths="./internal/..." paths="./cmd/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: verify-no-glimmer-imports
verify-no-glimmer-imports:
	@if grep -rEn 'github\.com/baldwinSPC/glimmer(/|")' --include='*.go' .; then \
		echo "the AMD Halo Operator must never import glimmer" >&2; \
		exit 1; \
	fi

CHART_DIR ?= deploy/helm/halo-operator

.PHONY: helm-sync
helm-sync: manifests ## Copy the generated CRD into the Helm chart.
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	helm lint $(CHART_DIR)

.PHONY: helm-package
helm-package: helm-lint ## Package the chart into dist/.
	mkdir -p dist
	helm package $(CHART_DIR) --destination dist

##@ CI runner image

# The image CI jobs run inside. Built and published by
# .github/workflows/runner-image.yml; this target is for reproducing a build
# locally when changing the Dockerfile. It builds for arm64 because that is
# what the ARC scale set runs on — on an x86 machine this needs QEMU
# (docker buildx create --use) and will be slow.
RUNNER_IMAGE ?= ghcr.io/baldwinspc/halo-operator-runner
RUNNER_TAG ?= dev

.PHONY: runner-image
runner-image: ## Build the CI runner image locally for linux/arm64.
	docker buildx build --platform linux/arm64 \
		-t $(RUNNER_IMAGE):$(RUNNER_TAG) \
		--load images/runner

.PHONY: verify-runner-pin
verify-runner-pin: ## Assert every workflow job container is pinned to an immutable reference.
	./scripts/ci-runner-pin-check.sh

##@ Standalone

# halo: the operator's judgement for a single node, no Kubernetes. Statically
# linked (CGO off) so the one file runs on any Linux Halo, including one with
# a different libc from the build machine. GOOS/GOARCH pass through, so
# `GOOS=linux GOARCH=amd64 make build-halo` cross-builds for a Halo.
.PHONY: build-halo
build-halo: ## Build the standalone halo binary into bin/halo.
	CGO_ENABLED=0 go build -trimpath -o bin/halo ./cmd/halo

##@ Operator image

# The image the chart runs (/manager and /agent). Releases build and push it
# from .github/workflows/release.yml, for linux/amd64 and linux/arm64, pinned by
# digest in the released chart. These targets are for local builds.
IMG ?= ghcr.io/baldwinspc/halo-operator
IMG_TAG ?= dev

.PHONY: image
image: ## Build the operator image for this machine's platform and load it into Docker.
	docker buildx build --load -t $(IMG):$(IMG_TAG) .

.PHONY: image-multiarch
image-multiarch: ## Build the operator image for linux/amd64 and linux/arm64 (not loaded; needs a docker-container builder).
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMG):$(IMG_TAG) .
