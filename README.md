# AMD Halo Operator

Node configuration for AMD Ryzen AI Max ("Strix Halo") Kubernetes nodes.

A Kubernetes operator that keeps AMD Strix Halo / "AI Halo" nodes (Ryzen AI
Max, gfx1151, unified memory) **configured the way a fleet declared they should
be** — kernel arguments, unified-memory sizing, firmware versions and compute
backend — behind a single CRD, `HaloDeviceConfig`.

**It is not a GPU-stack operator, and deliberately so.** The device plugin, NFD
detection and metrics exporter belong to AMD; gaps there are fixed in
[our fork of ROCm/gpu-operator](https://github.com/baldwinSPC/gpu-operator) and
offered upstream rather than reimplemented here. This operator *consumes* AMD's.

Design and rationale live in
[Glimmer GEP-0753](https://github.com/baldwinSPC/glimmer/blob/main/docs/geps/0753-amd-ai-halo.md).
Implementation is tracked on this repository's issues.

## Why this layer exists at all

The hardest-won knowledge in the Strix Halo community is not the GPU stack —
that works today with AMD's operator (`driver.enable: false` plus an NFD rule).
It is **node configuration**: kernel args (`amd_iommu=off`,
`amdgpu.gttsize`, `ttm.pages_limit`), a kernel-version floor, linux-firmware
pins where one release breaks ROCm and the next fixes it, and whether Vulkan or
ROCm is the right backend for a given workload. Today that lives in wiki pages
and is applied by hand, drifts silently, and is discovered when a node is slow
rather than when it is wrong.

No GPU operator models any of it, in any vendor's design — the NVIDIA analogue
is DGX Spark Enterprise Manageability, a different product category. That gap is
what this reconciles, and it is why the components below are all
*verification and policy* rather than workload plumbing.

## API

`HaloDeviceConfig` (cluster-scoped, `amd.halo.glimmer.ai/v1alpha1`) with
component stanzas:

| Stanza | What it manages |
|---|---|
| `memoryPolicy` | Verification only — observes kernel args and effective GTT, reports drift; **never mutates the host** |
| `runtimeProfile` | Vulkan/ROCm backend selection surfaced as labels + validated runtime images |
| `healthChecks` | Probes for the documented APU failure modes (clock-under-load, permission-fault, queue-create) |

Upstream owns the rest. `devicePlugin`, `labeller` and `metricsExporter` were
specified here in the first draft and were removed once it was clear they are
fixes to AMD's product rather than a missing one.

Status reports per-component state with three-valued semantics — a
component is `Ready`, `Degraded`, or `NotObserved`; absence of information
is never rendered as "off" — and every observation carries a timestamp.

See [config/samples/](config/samples/) for an example resource.

## Project boundaries

- **This repository never imports Glimmer** (CI-enforced). Glimmer may
  depend on this operator; the integration seam is this operator's CRD
  and its status, nothing else.
- Apache-2.0, permissive dependencies only (no copyleft anywhere in the
  shipped tree).
- Useful standalone: any Kubernetes user with Halo-class nodes can run it
  without Glimmer.

## Developing

```sh
make build     # go build ./...
make test      # go test ./...
make generate  # regenerate deepcopy (controller-gen)
make manifests # regenerate CRD + RBAC into config/
```

CI runs the same, plus a generated-files drift check and the
no-glimmer-imports gate.

## Trademarks

AMD, Ryzen and Radeon are trademarks of Advanced Micro Devices, Inc. ROCm is a
trademark of Advanced Micro Devices, Inc. Kubernetes is a registered trademark
of The Linux Foundation.

This project is an independent work. It is **not affiliated with, endorsed by,
or sponsored by Advanced Micro Devices, Inc.** The AMD name is used only to
identify the hardware this operator configures.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
