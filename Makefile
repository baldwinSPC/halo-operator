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
