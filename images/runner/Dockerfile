# Copyright 2026 the AMD Halo Operator authors
# SPDX-License-Identifier: Apache-2.0

# The CI job container for this repository.
#
# The self-hosted runners this repository uses are ARC scale sets running in
# Kubernetes container-hook mode, with ACTIONS_RUNNER_REQUIRE_JOB_CONTAINER=true
# — so every job MUST name a container, and that container supplies the
# toolchain. The stock ghcr.io/actions/actions-runner image has git, curl, tar,
# jq and the docker CLI, but no make, no gcc, no helm and no Go, all four of
# which this repository's CI needs. Hence this image.
#
# It is deliberately NOT derived from Glimmer's runner image. This repository
# has a CI-enforced rule that it never depends on Glimmer, and while a shared
# container image is not a Go import, borrowing one would put this repository's
# CI on Glimmer's release cadence — which is the coupling that rule exists to
# prevent.
#
# Ubuntu rather than a smaller base because the ARC container hook mounts the
# runner's own glibc-linked node binary into this container to execute
# JavaScript actions; a musl base (alpine) cannot run it.
FROM ubuntu:24.04

# Pinned, and bumped deliberately. GO_VERSION may be newer than the `go`
# directive in go.mod — Go's own toolchain rule downloads an exact match when
# one is required, so this only needs to be >= that directive, and tracking the
# current patch release is what keeps the security fixes.
ARG GO_VERSION=1.26.8
# Helm 3, not 4. Helm 4 is a major version with breaking template and lint
# behaviour, and this chart is developed against 3.x.
ARG HELM_VERSION=3.22.0
# Supplied automatically by BuildKit. These runners are aarch64.
ARG TARGETARCH=arm64

ENV DEBIAN_FRONTEND=noninteractive

# ca-certificates before anything is downloaded over TLS. make and gcc are what
# the stock runner image lacks; git is used by the generated-files drift gate.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gcc \
        git \
        jq \
        libc6-dev \
        make \
        tar \
        unzip \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Go's checksum is pinned here rather than fetched: go.dev publishes checksums
# in its JSON index (https://go.dev/dl/?mode=json&include=all), not as .sha256
# sidecar files, and a request for one returns an HTML redirect page that a
# naive verifier would happily compare against and fail — or worse, skip. It is
# arch-specific, so bumping GO_VERSION or building for another architecture
# means updating it from that index.
ARG GO_SHA256_ARM64=211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0

RUN set -eux; \
    test "${TARGETARCH}" = "arm64" || { \
        echo "GO_SHA256_ARM64 only describes the arm64 archive; building for ${TARGETARCH} needs its own pinned checksum" >&2; \
        exit 1; \
    }; \
    url="https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL -o /tmp/go.tgz "$url"; \
    echo "${GO_SHA256_ARM64}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz

# Helm, unlike Go, does publish a .sha256sum sidecar, in the standard
# "<digest>  <filename>" form — so it is fetched rather than pinned. That
# guards a corrupted transfer; it is not a supply-chain claim, since the
# checksum comes from the same host as the artifact.
RUN set -eux; \
    url="https://get.helm.sh/helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL -o /tmp/helm.tgz "$url"; \
    curl -fsSL -o /tmp/helm.sha256 "${url}.sha256sum"; \
    test -s /tmp/helm.sha256; \
    awk '{print $1"  /tmp/helm.tgz"}' /tmp/helm.sha256 | sha256sum -c -; \
    tar -C /tmp -xzf /tmp/helm.tgz; \
    install -m 0755 "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm; \
    rm -rf /tmp/helm.tgz /tmp/helm.sha256 "/tmp/linux-${TARGETARCH}"

# GOTOOLCHAIN=auto is the default, and is what lets a go.mod requiring a newer
# patch release than the baked one fetch it rather than fail. GOPATH and
# GOCACHE are under /root because the container hook runs job containers as
# root; leaving them at their defaults would put them in a home directory that
# may not exist.
ENV PATH=/usr/local/go/bin:/root/go/bin:$PATH \
    GOPATH=/root/go \
    GOCACHE=/root/.cache/go-build \
    GOTOOLCHAIN=auto

# Fail the build rather than ship an image whose toolchain is not what the
# Dockerfile claims. A CI image that silently lacks a tool produces a red build
# nobody can explain.
RUN set -eux; \
    go version; \
    helm version --short; \
    make --version | head -1; \
    gcc --version | head -1; \
    git --version
