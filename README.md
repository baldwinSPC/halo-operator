# AMD Halo Operator

Node configuration for AMD Ryzen AI Max ("Strix Halo") Kubernetes nodes.

A Kubernetes operator that keeps AMD Strix Halo / "AI Halo" nodes (Ryzen AI
Max, gfx1151, unified memory) **configured the way a fleet declared they should
be** — kernel arguments, unified-memory sizing, firmware versions and the
compute backends a node can run — behind a single CRD, `HaloDeviceConfig`.

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
| `memoryPolicy` | Verification only — observes kernel args and effective GTT, reports drift; **never mutates the host** (the standalone binary's `apply -execute` is the one opt-in exception, see below) |
| `runtimeProfile` | Verification only — observes whether every node exposes what each declared backend needs from the kernel (a DRM render node for Vulkan; that and a KFD GPU agent for ROCm), plus a catalogue of validated runtime images per backend. It does **not** choose a backend: which one is faster flips per model and per phase, so that is the workload's choice |
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
command line, the running values of the amdgpu and ttm module parameters, the
documented amdgpu sysfs counters, and the GPU's DRM render node and KFD
topology — read-only, unprivileged, two narrow host mounts — and publishes what
it read as a
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

**Each observation is pinned to the node that writes it.** RBAC can't scope
create or patch to a resource name, so on its own it would let a compromised
node's agent write a healthy-looking observation for a peer. NFD's workers
carry exactly that trust model. The chart closes the gap with a
`ValidatingAdmissionPolicy`, on by default through `agent.pinObservationNames`.
It admits a write from the agent's service account only when the
observation's name equals the node in the caller's own bound token
(`authentication.kubernetes.io/node-name`), and it fails closed when the
token carries no node claim.

The policy needs Kubernetes 1.30 or newer. An older API server refuses the
install by naming the missing `ValidatingAdmissionPolicy` kind; set
`agent.pinObservationNames=false` there. A test renders the chart's policy
into a real API server and impersonates the agent with and without its node
claim, so the shipped object is what's verified.

The kernel command line is published raw and parsed controller-side, so
"read fine, nothing set" (empty) and "could not read" (absent) stay distinct
through serialization, and parse-rule fixes ship without redeploying agents.

**The command line is not the only place a parameter is set**, which is why
the running module parameters are read too. On the AMD Ryzen AI Developer
Platform the shipped `ttm-config.service` writes `/etc/modprobe.d/ttm.conf`, so
a correctly sized unit has `ttm.pages_limit` in effect and carries it nowhere
on `/proc/cmdline`; judged by the command line alone that node reports drift
forever. An expected parameter therefore resolves from the command line first —
that is what the fleet asked for at boot — and falls back to the module's
running value under `/sys/module`. The asymmetry between the two missing cases
is deliberate: a core-kernel parameter missing from a command line that *was*
read is observed absence and so is drift, while a module parameter missing from
the command line proves nothing until the module's own value has been checked.
That value is often unreadable — `/sys/module/amdgpu/parameters/*` is mode 0400
and this agent is unprivileged — and an unreadable parameter reports
`NotObserved`, never drift. Accusing a node on the strength of a permission
error is precisely the failure this design refuses.

## Hardware validation

**The collector's reads are confirmed against a physical gfx1151** (AMD Ryzen
AI Developer Platform, kernel `6.18.44+rex+5-amd64`, 2026-09-20). Everything it
reads is **documented kernel ABI** (`mem_info_gtt_total`, `gpu_busy_percent`,
`pp_dpm_sclk`, PCI `vendor`/`class`/`driver`), and how a real unit presents
that ABI is no longer a guess:

- **Device enumeration holds.** Exactly one amdgpu function, class `0x038000`.
  Its sibling at the same PCI slot is *also* vendor `0x1002` but class
  `0x040300` (`snd_hda_intel`) — the class filter is the only thing keeping
  discovery at one candidate, so it must not be loosened.
- **The counters populate on an APU.** `mem_info_gtt_total` reads
  101139496960 (94 GiB after the floor to whole GiB); `gpu_busy_percent` reads
  a real 0 at idle.
- **The `pp_dpm_sclk` ladder parses.** Three levels, `600`/`1100`/`2900` MHz
  with the current level starred. Note `pp_dpm_fclk` on the same part carries
  **no** star on any level — a starless ladder is a real format, and the parser
  reports the current clock as unread rather than inventing one.

The runtime profile's two reads are not equally settled. **KFD's topology was
read on the unit** (2026-09-14, unprivileged): node 0 is the CPU at
`gfx_target_version` 0 and node 1 the GPU at 110501, gfx1151. **The render
node's location is inferred, not yet read**: the agent lists the PCI device's
own `drm` directory, which is where the kernel registers a device's DRM minors
and where `/sys/class/drm/card0` resolves, and `/dev/dri/renderD128` is
present on the unit, but nobody has listed that directory on it. If the layout
differs, the reading is *not observed* and the backends `Unknown`, never
unavailable.

The collector is strict on purpose: any deviation from the documented formats
is reported as *not observed*, never guessed at. `/agent --once` exists for
bring-up.

Two things stay unvalidated, and hardware arriving did not settle either:

- **The kernel-log probes have no reader.** `permission-fault` and
  `queue-create` count dmesg lines whose exact formats are hardware facts;
  an unvalidated pattern that matches nothing would report "0 faults" — a
  clean bill of health nobody measured. The unit on hand has never emitted
  one of these faults, so there is nothing to validate a pattern *against*:
  this is blocked by the absence of a failure, not by the absence of
  hardware. Until the patterns are validated those counters are never
  published, the probes report `NotObserved`, and a config using the default
  probe set holds `NodesHealthy` at `Unknown`. Scope `healthChecks.probes` to
  `[clock-under-load]` if you want that condition decidable today.
- **Multi-GPU nodes are not chosen between.** With more than one
  amdgpu-driven device the agent publishes no counters rather than silently
  picking one. The available unit is single-GPU, so this is still unsettled.

The claim made here is narrow and deliberate. The agent's *reads* are
confirmed on a physical gfx1151, and so is the *judgement*: run through the
standalone binary (below), which shares every decision function with the
controller, both `memory-policy` and `health-checks` reach `Ready` against the
physical unit for a policy matching what the platform configures
(`runtime-profile` is newer than that run and has not yet been judged against
the unit). What has not
happened yet is a condition reaching `True` on a node **under Kubernetes
management** — which is what issues #4/#5 ask for, so they remain open until
the node joins a cluster.

## Running without Kubernetes

Most Halo boxes will never be in a cluster. The same judgement is available
as a single static binary, `halo`, with no API server, no CRDs and no agent:

```sh
halo inspect                        # what this node reports
halo check -policy policy.yaml      # judge it against a policy
halo apply -policy policy.yaml      # plan what would bring it into line
```

The policy file is the `HaloDeviceConfig` manifest you would `kubectl apply` —
one format for both modes, parsed strictly, so a misspelled key is an error
rather than a silently-ignored expectation that then passes by checking
nothing.

**It is not a second implementation.** `halo` reads the node with the agent's
collector and judges it through the controller's own translation and decision
functions; the only thing removed is the round trip through an API server. A
test feeds the same readings down both paths and fails the build if any
component state differs, and that test is itself checked by breaking one side
and confirming it goes red.

Exit codes carry the three-valued answer all the way out, so a script or a
cron job can never read "could not tell" as "fine":

| code | meaning |
|---|---|
| `0` | compliant — every enabled check was observed and matched |
| `1` | drifted — something was observed to disagree (`apply`: action needed) |
| `2` | not observed — nothing disagreed, but compliance could not be established |
| `64` / `70` | usage / internal error — never a verdict about the node |

`2` is deliberately not reused for usage errors, which is what Go's flag
package would do by default: a bad flag must never read as a verdict.

Run it unprivileged and parameters behind mode-0400 files report as not
observed; run it as root and they become real verdicts. It never guesses in
between.

### `check` as root: every boot image, not just the running kernel

A node can be right on disk and right in its running kernel, and still come
up at the kernel-default GTT the next time a different kernel boots. The
physical unit was in exactly that state (#20). Its older kernel's initramfs on
the EFI system partition, which is what the boot loader actually reads,
carried no `ttm.conf`, while the `/boot` copy did and every other check
reported the node as fine.

So `halo check`, run as root, also opens every initramfs image a boot could
load. That is the `/boot` copy and the ESP copy for every kernel in
`/lib/modules`, the same positive list `apply -execute` verifies. It reports
each image as `boot-images`. The component compares the `/etc/modprobe.d`
files that set the policy's module parameters, in both directions:

- a governed file missing from an image is drift;
- a copy that differs from the one on disk is drift;
- an image carrying a governed file the disk no longer has is drift.

Files that set nothing the policy governs are not compared. An image that
can't be unpacked is not observed, never fine.

Run unprivileged, the check is skipped and reported as skipped. It doesn't
change the exit code, so an unprivileged `check` answers exactly as it did
before. `-boot-images=false` skips it as root too.

The remedy for a drifted image is `update-initramfs -u -k all` as root, then
`check` again. Re-checking matters because the hook that copies images to the
ESP exits 0 even when it copies nothing. `apply -execute` doesn't repair this
case: it rebuilds only when it has a modprobe.d edit to make, and a node whose
disk is already right has none.

### `apply`: plan by default, execute on request

`halo apply` computes the exact `/etc/modprobe.d` edits that would bring
module parameters to the policy and shows them as a diff. **By default it
writes nothing.** `halo apply -execute`, run as root, carries the plan out.

**This is a deliberate, narrow exception to the boundary that this project
never mutates a host, and the boundary is otherwise unchanged.** The operator —
the controller and the DaemonSet agent, everything that runs in a cluster —
still never writes to a node; host mutation from a fleet remains Glimmer's
signed-task path. The exception covers one thing only: a person, on the node,
as root, passing an explicit flag. Even then it writes only `/etc/modprobe.d`,
never touches the kernel command line, and never reboots.

What planning gets right:

- **Kernel command line parameters are out of scope, always.** Changing the
  command line is Glimmer's signed-task path, decided by the owner for exactly
  this hardware (#13); a second tool editing loader entries would be two
  writers on one boot configuration. A module parameter that is *on* the
  command line is also out of scope, since it overrides modprobe.d at boot and
  an edit there would be silently ignored.
- **It edits the platform's own file.** `ttm.pages_limit` is planned as an edit
  to `/etc/modprobe.d/ttm.conf` — the file the platform's `ttm-config.service`
  writes — never as a second file whose effect would depend on modprobe's
  merge order.

What `-execute` does, and why it is all-or-nothing:

- **It writes modprobe.d and rebuilds the initramfs together, or neither.**
  `ttm` and `amdgpu` load from the initramfs, and modprobe.d options are baked
  in when it is built, so a write alone changes nothing at the next boot. Worse,
  a write without a rebuild does not leave the node unchanged: the stale
  initramfs keeps the old value until something else rebuilds it — a kernel
  upgrade does so automatically — and the change lands silently at an
  unpredictable later boot. So there is no write-only mode.
- **"Together" is verified, not assumed.** The boot loader here reads the
  initramfs from the EFI system partition, not `/boot`; a post-update hook
  copies it there, and that hook exits 0 *without copying* if `bootctl` is
  missing. So after `update-initramfs -u -k all`, every image a boot could
  load — the `/boot` copy and the ESP copy, for every installed kernel — is
  unpacked and checked to carry the new options. Images are found from the
  kernels in `/lib/modules`, matched exactly: the physical unit carries a
  stale `initrd.img-*.dpkg-bak` that a glob would pick up and fail on.
- **Any failure rolls back, and resyncs.** If the rebuild fails or any image
  does not carry the change, every file is restored exactly (a file the run
  created is removed), and the initramfs is rebuilt again from the restored
  files — so a failed apply leaves the boot image agreeing with the config,
  not carrying a change nobody kept. Writes are atomic renames, so a crash
  leaves the old file or the new one, never a truncated one.
- **It re-checks before writing.** If a file changed between planning and
  executing, the diff that was shown is no longer the diff that would be
  written, and it refuses.
- **It never reboots.** Nothing takes effect on the running system — GTT is
  sized when amdgpu initialises. Reboot, then `halo check` verifies the running
  values rather than trusting that the write took.

One side effect belongs to the platform rather than this tool, and is worth
knowing: the post-update hook runs `kernel-install add` for each kernel, which
also regenerates the loader entries from `/etc/kernel/cmdline`.

`-execute` exits `0` when the change was written, rebuilt and verified (reboot
pending), `70` when it failed and was rolled back, and `77` when not run as
root.

### Building it

```sh
make build-halo                                     # for this machine
GOOS=linux GOARCH=amd64 make build-halo             # for a Halo, from anywhere
```

The result is one statically linked binary at `bin/halo`; copy it to the node.

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

**From a release** (recommended). Each release on GitHub carries the chart
already pinned to its image **by digest**, so the version it names is the
bytes it runs, plus the standalone `halo` binaries and `SHA256SUMS`:

```sh
helm install halo-operator halo-operator-<version>.tgz --namespace halo-system --create-namespace
```

The image, `ghcr.io/baldwinspc/halo-operator`, is built for linux/amd64 and
linux/arm64, so the same chart installs on a Halo (x86-64) and on an arm64
control plane such as a DGX Spark. It is `FROM scratch`: the two static
binaries and nothing else, running as a numeric non-root user. This repository
is private, so its package is too — a cluster needs an image pull secret with
`read:packages`, set through `imagePullSecrets`.

**From source**, for development:

```sh
helm install halo-operator deploy/helm/halo-operator --namespace halo-system --create-namespace
```

The chart installs the CRD from its `crds/` directory. Helm installs those on
first install only and never upgrades or removes them, so a CRD change needs
`kubectl apply -f config/crd/bases/` at upgrade time. Set `installCRDs: false`
when something else owns the CRD.

Values worth knowing: `image.tag` defaults to the chart appVersion,
`image.digest` pins the pull (a released chart sets it; a malformed one fails
the render rather than the pull),
`leaderElection.enabled` must stay on if `replicaCount` is above 1, and
`rbac.create` / `serviceAccount.create` let you bring your own. The node
agent is on by default (`agent.enabled`), runs on nodes matching
`agent.nodeSelector` (the NFD AMD GPU label), and publishes every
`agent.interval` (1m); disabling it does not fake anything — conditions fall
back to `Unknown`. The image carries both `/manager` and `/agent`; `make
image` builds it locally.

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
