# Copyright 2026 the AMD Halo Operator authors
# SPDX-License-Identifier: Apache-2.0

CONTROLLER_TOOLS_VERSION ?= v0.19.0
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
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
