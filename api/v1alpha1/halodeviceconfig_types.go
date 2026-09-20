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
	// (e.g. "amdgpu.gttsize", "ttm.pages_limit", "amd_iommu").
	// +optional
	ExpectedKernelArgs map[string]string `json:"expectedKernelArgs,omitempty"`
	// MinEffectiveGPUMemoryGiB is the floor for GTT-visible GPU memory; a
	// node below it is reported as drifted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MinEffectiveGPUMemoryGiB int32 `json:"minEffectiveGPUMemoryGiB,omitempty"`
}

// RuntimeProfileSpec selects the compute backend surfaced to workloads.
type RuntimeProfileSpec struct {
	// Backend defaults to vulkan.
	// +optional
	// +kubebuilder:validation:Enum=vulkan;rocm
	Backend string `json:"backend,omitempty"`
	// RuntimeImages maps a backend to its validated runtime image.
	// +optional
	RuntimeImages map[string]string `json:"runtimeImages,omitempty"`
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
// and compute backend are what the fleet declared they should be.
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
