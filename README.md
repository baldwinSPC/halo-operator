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

A second kind, `HaloNodeObservation` (cluster-scoped, one per node, shortname
`hno`), carries what the node agent actually read. It is written by the agent
and read by the controller — not user-authored — and exists so that every
verdict on a `HaloDeviceConfig` is traceable to an inspectable, timestamped
reading. See the next section.

Status reports per-component state with three-valued semantics — a
component is `Ready`, `Degraded`, or `NotObserved`; absence of information
is never rendered as "off" — and every observation carries a timestamp.

See [config/samples/](config/samples/) for an example resource.

## How a condition gets its value

Nothing in the controller touches a node. A separate **node observation
agent** runs as a DaemonSet on every AMD GPU node (placed by the
`feature.node.kubernetes.io/amd-gpu` label NFD applies), reads the kernel
command line and the documented amdgpu sysfs counters — read-only,
unprivileged, two narrow host mounts — and publishes what it read as a
**`HaloNodeObservation`** (cluster-scoped, named after the node, owned by the
Node object so node deletion garbage-collects it). The controller lists the
targeted Nodes, joins observations by name, and judges the result:

- a node that was **read and matched** the policy counts toward `True`;
- a node **observed to disagree** drives the condition `False`, with the
  parameter named;
- a node whose observation is **missing, partial, or older than
  `--max-observation-age`** (default 5m) counts as *unread* and holds the
  condition at `Unknown`. It stays in the calculus — a fleet is not more
  compliant because part of it stopped reporting.

Readings travel as a CR rather than as Node annotations or a scrape endpoint
for four load-bearing reasons. Absence is structural: every reading is an
optional field, so "could not read" arrives as a *missing field*, never as a
zero — on a fault counter zero means healthy, and that collapse is the one
failure this design refuses by construction. Privilege is minimal: the agent
can create and patch observations and read its own Node, nothing else — no
write access to Nodes, which annotation-publishing would require. The flow is
level-triggered: the controller watches observations, so readings persist
across controller restarts and `kubectl get halonodeobservations` shows
exactly what each node claimed. And it is the shape this ecosystem already
reads — NFD's `NodeFeature` works the same way.

The kernel command line is published raw and parsed controller-side, so
"read fine, nothing set" (empty) and "could not read" (absent) stay distinct
through serialization, and parse-rule fixes ship without redeploying agents.

## Hardware validation pending

The physical Halo units have not arrived. Everything the agent reads is
**documented kernel ABI** (`mem_info_gtt_total`, `gpu_busy_percent`,
`pp_dpm_sclk`, PCI `vendor`/`class`/`driver`), but how the first real gfx1151
unit presents that ABI — device enumeration, ladder format, whether the
counters populate at all on an APU — is deliberately unverified until it can
be read off hardware. The collector is strict on purpose: any deviation from
the documented formats is reported as *not observed*, never guessed at.
`/agent --once` exists for exactly that bring-up day.

Two consequences to know about:

- **The kernel-log probes have no reader yet.** `permission-fault` and
  `queue-create` count dmesg lines whose exact formats are hardware facts;
  an unvalidated pattern that matches nothing would report "0 faults" — a
  clean bill of health nobody measured. Until patterns are validated on a
  real unit those counters are simply never published, the probes report
  `NotObserved`, and a config using the default probe set holds
  `NodesHealthy` at `Unknown`. Scope `healthChecks.probes` to
  `[clock-under-load]` if you want that condition decidable today.
- **Multi-GPU nodes are not chosen between.** With more than one
  amdgpu-driven device the agent publishes no counters rather than silently
  picking one; selection policy waits for hardware.

No claim of hardware validation is made anywhere in this repository, and
issues #4/#5 stay open until a condition reaches `True` against a physical
node.

## Project boundaries

- **This repository never imports Glimmer** (CI-enforced). Glimmer may
  depend on this operator; the integration seam is this operator's CRD
  and its status, nothing else.
- Apache-2.0, permissive dependencies only (no copyleft anywhere in the
  shipped tree).
- Useful standalone: any Kubernetes user with Halo-class nodes can run it
  without Glimmer.

## Installing

Requires Kubernetes 1.29 or newer.

```sh
helm install halo-operator deploy/helm/halo-operator --namespace halo-system --create-namespace
```

The chart installs the CRD from its `crds/` directory. Helm installs those on
first install only and never upgrades or removes them, so a CRD change needs
`kubectl apply -f config/crd/bases/` at upgrade time. Set `installCRDs: false`
when something else owns the CRD.

Values worth knowing: `image.tag` defaults to the chart appVersion,
`leaderElection.enabled` must stay on if `replicaCount` is above 1, and
`rbac.create` / `serviceAccount.create` let you bring your own. The node
agent is on by default (`agent.enabled`), runs on nodes matching
`agent.nodeSelector` (the NFD AMD GPU label), and publishes every
`agent.interval` (1m); disabling it does not fake anything — conditions fall
back to `Unknown`. The image is expected to carry both `/manager` and
`/agent` binaries.

## Developing

```sh
make build     # go build ./...
make test      # go test ./...
make generate  # regenerate deepcopy (controller-gen)
make manifests # regenerate CRD + RBAC into config/
make helm-sync # copy the generated CRD into the chart
make helm-lint # lint the chart
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
