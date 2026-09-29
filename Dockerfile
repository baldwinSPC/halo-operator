# Copyright 2026 the AMD Halo Operator authors
# SPDX-License-Identifier: Apache-2.0

# The operator image: /manager (the controller) and /agent (the node
# observation agent), in one image because the chart runs both from it.
#
# Multi-arch without emulation. The build stage runs on the BUILD machine's own
# platform and cross-compiles Go for the target, so building linux/arm64 on an
# amd64 host (or the reverse) never runs a target-architecture instruction. The
# final stage has no RUN at all, so no stage needs QEMU. Emulated builds are
# slow and produce failures that do not reproduce on real hardware.

# The builder is pinned by digest. It never ships — only the two static
# binaries are copied out of it — but a moving builder makes a release
# unreproducible, which is the same defect as a moving runtime tag.
FROM --platform=$BUILDPLATFORM golang:1.26.8@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
# CGO off: static binaries, so the runtime stage needs no libc at all.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# scratch, not distroless. This repository ships no copyleft in anything it
# distributes, and distroless/static carries the ca-certificates bundle, which
# is MPL-2.0. Neither binary needs a system CA bundle: both reach the API server
# with the service account's own mounted CA. So the shipped image contains our
# two binaries and nothing else to account for.
FROM scratch
COPY --from=build /out/manager /manager
COPY --from=build /out/agent /agent
# NUMERIC, and non-zero. The chart sets runAsNonRoot without runAsUser, and the
# kubelet refuses to start such a container unless the image itself declares a
# numeric non-root user — a name cannot be checked without /etc/passwd, which
# scratch does not have. Leaving this out makes every pod fail to start.
USER 65532:65532
ENTRYPOINT ["/manager"]
