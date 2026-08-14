# glimmer-halo-operator

A Kubernetes operator for AMD Strix Halo / "AI Halo" class devices
(Ryzen AI Max, gfx1151, unified memory): it pulls the pieces a working
GPU node needs — device plugin, APU-aware labelling, sysfs/hwmon metrics,
unified-memory configuration verification, and health probes — into one
reconciled lifecycle behind a single CRD, `HaloDeviceConfig`.

Design and rationale live in
[Glimmer GEP-0753](https://github.com/baldwinSPC/glimmer/blob/main/docs/geps/0753-amd-ai-halo.md).
Implementation is tracked on this repository's issues.

## Why an operator

AMD's GPU Operator does not support APUs; the community runs Strix Halo
Kubernetes from copy-paste guides (hand-written NFD rules, kernel-arg
folklore, a bare device-plugin DaemonSet, no metrics because the amdsmi
stack is blind on APUs). Five components share one lifecycle, one version
matrix, and one status surface — that is an operator, not a pile of
DaemonSets.

## API

`HaloDeviceConfig` (cluster-scoped, `halo.glimmer.ai/v1alpha1`) with
component stanzas:

| Stanza | What it manages |
|---|---|
| `devicePlugin` | The ROCm device plugin (`amd.com/gpu`), APU-validated pinning |
| `labeller` | APU-aware detection: NFD rule + KFD-derived capability labels |
| `metricsExporter` | sysfs/hwmon metrics (temperature, power, clocks, utilization, VRAM **and GTT**) |
| `memoryPolicy` | Verification only — observes kernel args and effective GTT, reports drift; never mutates the host |
| `runtimeProfile` | Vulkan/ROCm backend selection surfaced as labels + validated runtime images |
| `healthChecks` | Probes for the documented APU failure modes (clock-under-load, permission-fault, queue-create) |

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
