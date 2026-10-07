// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryPolicySpec declares the expected unified-memory node
// configuration. The operator verifies and reports drift; it never
// mutates the host.
type MemoryPolicySpec struct {
	// Enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// ExpectedKernelArgs maps kernel parameters to their expected values
	// (e.g. "ttm.pages_limit", "amd_iommu"). Module parameters such as
	// ttm.pages_limit are checked against the running value too, so one set
	// through /etc/modprobe.d is found. Size GTT with ttm.pages_limit:
	// amdgpu.gttsize is deprecated by the kernel ("Configuring gttsize via
	// module parameter is deprecated, please use ttm.pages_limit"), and a
	// policy that declares it is still checked but reported with a note.
	// +optional
	ExpectedKernelArgs map[string]string `json:"expectedKernelArgs,omitempty"`
	// MinEffectiveGPUMemoryGiB is the floor for GTT-visible GPU memory; a
	// node below it is reported as drifted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinEffectiveGPUMemoryGiB int32 `json:"minEffectiveGPUMemoryGiB,omitempty"`
}

// RuntimeProfileSpec declares the compute backends the targeted nodes must be
// able to run, and the validated runtime image for each.
//
// It deliberately does not choose a backend for the node. Which backend is
// faster flips per model and per phase (GEP-0753 pain point #4: ROCm leads
// prefill, Vulkan leads decode), so that choice belongs to the workload. What
// a node can truthfully report is whether the kernel exposes the interfaces
// each backend needs; a backend's userspace ships in the workload's image,
// not on the node.
type RuntimeProfileSpec struct {
	// Enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Backends lists the compute backends every targeted node must be able to
	// run. A node can run vulkan when the kernel exposes a DRM render node for
	// its GPU, and rocm when KFD also lists that GPU as a compute agent.
	// Defaults to every known backend.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Enum=vulkan;rocm
	Backends []string `json:"backends,omitempty"`
	// RuntimeImages names the validated runtime image for each backend, as a
	// catalogue for workloads. Nodes are not judged against it.
	// +optional
	RuntimeImages RuntimeImages `json:"runtimeImages,omitempty"`
}

// RuntimeImages is the validated runtime image for each backend. It has one
// field per backend rather than being a map, so a misspelt backend is an
// unknown field instead of a catalogue entry no workload will ever look up.
type RuntimeImages struct {
	// +optional
	Vulkan string `json:"vulkan,omitempty"`
	// +optional
	ROCm string `json:"rocm,omitempty"`
}

// StackPolicySpec declares the software stack under ROCm and Vulkan that the
// targeted nodes must run: which amdgpu driver, how old a kernel, and which
// GPU firmware versions are known to be bad. The operator verifies and
// reports; installing or upgrading any of it is the Glimmer host path.
type StackPolicySpec struct {
	// Enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// RequireInboxDriver requires the kernel's own amdgpu, not the out-of-tree
	// amdgpu-dkms. Defaults to true. AMD's guidance for gfx1151 APUs is the
	// inbox driver; it closed ROCm#6186 (PERMISSION_FAULT storms) and
	// ROCm#6118 (queue-create faults) as caused by amdgpu-dkms.
	// +optional
	RequireInboxDriver *bool `json:"requireInboxDriver,omitempty"`
	// MinKernelVersion is the oldest kernel accepted, as a dotted version
	// compared against the leading version of the running kernel's release
	// ("6.18.4" accepts "6.18.44+rex+5-amd64"). Empty means no floor. There
	// is no default because AMD's own floors disagree by distribution: its
	// RDNA3.5 page names Ubuntu 24.04 HWE 6.17.0-19, OEM 6.14.0-1018, and
	// 6.18.4 otherwise.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[0-9]+\.[0-9]+(\.[0-9]+)?$`
	MinKernelVersion string `json:"minKernelVersion,omitempty"`
	// KnownBadFirmware flags firmware versions known to break this hardware.
	// The operator never installs firmware; it reports a node running one of
	// these.
	// +optional
	// +listType=atomic
	KnownBadFirmware []KnownBadFirmware `json:"knownBadFirmware,omitempty"`
}

// KnownBadFirmware is one firmware version to flag.
type KnownBadFirmware struct {
	// Component names the GPU firmware: an entry of the GPU's fw_version
	// directory in sysfs ("mes", "smc", "rlc", ...) or "vbios". On an APU
	// these arrive with the distribution's firmware package, not a flash.
	// The system BIOS is not here: its pin and compliance belong to the
	// firmware stanza (halo-operator#36) and its flash to Glimmer's rollout.
	// +kubebuilder:validation:MinLength=1
	Component string `json:"component"`
	// Version is the version exactly as sysfs prints it ("0x00000088").
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// Reason says why it is bad, ideally with a link; it is quoted in the
	// condition that reports it.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// HealthChecksSpec enables probes for the documented APU failure modes.
type HealthChecksSpec struct {
	// Enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// Probes defaults to every known probe.
	// +optional
	// +kubebuilder:validation:items:Enum=clock-under-load;permission-fault;queue-create
	Probes []string `json:"probes,omitempty"`
}

// HaloDeviceConfigSpec defines the desired NODE CONFIGURATION for Halo nodes.
//
// It is deliberately NOT a GPU-stack config. The device plugin, NFD detection
// and metrics exporter are AMD's product and are contributed upstream to
// ROCm/gpu-operator rather than reimplemented here; this operator consumes that
// one. What it owns is the layer no GPU operator models in any vendor's design:
// whether a node's kernel arguments, unified-memory sizing, firmware versions
// and compute backends are what the fleet declared they should be.
//
// The NVIDIA analogue is not the GPU Operator — it is DGX Spark Enterprise
// Manageability, which is a different product category.
type HaloDeviceConfigSpec struct {
	// NodeSelector targets the nodes this config applies to. Empty means
	// every node carrying the AMD GPU node feature label, which AMD's GPU
	// Operator applies through Node Feature Discovery; this operator
	// consumes that labelling rather than performing its own.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// +optional
	MemoryPolicy MemoryPolicySpec `json:"memoryPolicy,omitempty"`
	// +optional
	RuntimeProfile RuntimeProfileSpec `json:"runtimeProfile,omitempty"`
	// +optional
	HealthChecks HealthChecksSpec `json:"healthChecks,omitempty"`
	// +optional
	StackPolicy StackPolicySpec `json:"stackPolicy,omitempty"`
}

// ComponentState is the observed state of one managed component.
// Absence of information is NotObserved, never a confident "off".
type ComponentState string

const (
	ComponentDisabled       ComponentState = "Disabled"
	ComponentNotImplemented ComponentState = "NotImplemented"
	ComponentProgressing    ComponentState = "Progressing"
	ComponentReady          ComponentState = "Ready"
	ComponentDegraded       ComponentState = "Degraded"
	ComponentNotObserved    ComponentState = "NotObserved"
)

// ComponentStatus reports one component's observed state.
type ComponentStatus struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Disabled;NotImplemented;Progressing;Ready;Degraded;NotObserved
	State ComponentState `json:"state"`
	// +optional
	Message string `json:"message,omitempty"`
	// LastObserved is when this state was taken.
	// +optional
	LastObserved metav1.Time `json:"lastObserved,omitempty"`
}

// ConditionReady is True when every enabled component is Ready.
const ConditionReady = "Ready"

// ConditionMemoryPolicyCompliant is True when every targeted node was observed
// and matched the declared memory policy, False when a node was observed to
// disagree with it, and Unknown when compliance could not be established —
// including when nothing has been observed at all. Unknown is a real answer
// here, not a placeholder: an unread node is never reported as configured
// correctly.
const ConditionMemoryPolicyCompliant = "MemoryPolicyCompliant"

// ConditionNodesHealthy is True when every configured probe ran on every
// targeted node and found no fault, False when a probe detected one of the
// documented failure modes, and Unknown when health could not be established.
// A probe that could not read its inputs leaves this Unknown; it never counts
// as a clean bill of health.
const ConditionNodesHealthy = "NodesHealthy"

// ConditionBackendsAvailable is True when every targeted node was observed to
// expose the kernel interfaces each declared backend needs, False when a node
// was observed to lack one, and Unknown when that could not be established.
const ConditionBackendsAvailable = "BackendsAvailable"

// ConditionStackCompliant is True when every targeted node was observed to run
// the declared driver, a kernel at or above the floor, and no known-bad
// firmware; False when a node was observed not to; and Unknown when that could
// not be established.
const ConditionStackCompliant = "StackCompliant"

// ConditionGPUFirmwareCompliant is True when every targeted node's GPU
// firmware was read and none of it is on the declared known-bad list, False
// when a node was observed running a known-bad version, and Unknown when that
// could not be established. The system BIOS is not judged here; see
// halo-operator#36.
const ConditionGPUFirmwareCompliant = "GPUFirmwareCompliant"

// ConditionBootHoldCleared is True when no persisted boot configuration file
// carries `splash` and the running kernel was booted without it, False when the
// unit still holds boots, and Unknown when it could not be established.
//
// False has two reasons and they call for different actions: BootHoldPresent
// means a file still carries the token and needs editing, RebootPending means
// every file is clear and only a restart is outstanding — which is what
// Glimmer's scan calls "staged".
//
// Unknown is reachable and required. The loader entries live on an autofs ESP
// that a container can see as empty, and on a directory an unprivileged agent
// cannot enter; both return nothing without erroring. Reporting True off that
// is the wrong value rather than a missing one, so an unseen file leaves this
// Unknown.
//
// The operator never edits the host. Removing the token is Glimmer's
// signed-task path; this condition only reports.
const ConditionBootHoldCleared = "BootHoldCleared"

// HaloDeviceConfigStatus is the observed state of the config.
type HaloDeviceConfigStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Components []ComponentStatus `json:"components,omitempty"`
	// Nodes is the verdict per targeted node, so a reader can name which
	// nodes drifted and which went unread without reading agent logs; the
	// conditions above only count them. Nodes with any component not Ready
	// come first, then the rest, each group by name. At most MaxNodeStatuses
	// are listed.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=256
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// NodesOmitted counts targeted nodes left out of Nodes by its bound. Zero
	// when every targeted node is listed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NodesOmitted int32 `json:"nodesOmitted,omitempty"`
}

// MaxNodeStatuses bounds status.nodes, keeping a large fleet's status well
// under the API server's object size limit.
const MaxNodeStatuses = 256

// NodeStatus is one targeted node's state per enabled component.
type NodeStatus struct {
	// Name is the node's name.
	Name string `json:"name"`
	// ObservedAt is when the agent took the reading these states rest on.
	// Absent when the node has no fresh observation, which is also why every
	// component then reads NotObserved.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// Components is this node's state for each enabled component, in the
	// same vocabulary as the config-wide list.
	// +listType=map
	// +listMapKey=name
	Components []ComponentStatus `json:"components"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=hdc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HaloDeviceConfig is the GPU-stack configuration for Halo-class nodes.
type HaloDeviceConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HaloDeviceConfigSpec   `json:"spec,omitempty"`
	Status HaloDeviceConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// HaloDeviceConfigList contains a list of HaloDeviceConfig.
type HaloDeviceConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HaloDeviceConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HaloDeviceConfig{}, &HaloDeviceConfigList{})
}
