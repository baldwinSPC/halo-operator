// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HaloNodeObservationSpec is a report, not a desire. The node agent writes what
// it read off one node; the controller reads it and nothing reconciles it
// toward anything. It lives in spec rather than status so the whole object is
// one writer's claim, applied in a single request (the Lease pattern), and so
// the agent needs no status-subresource choreography.
//
// Every reading is a pointer, and that is the contract of this type: a reading
// that could not be taken is an absent field, never zero and never an empty
// string. On a fault counter zero means "measured, nothing wrong", so
// collapsing "not measured" into zero would silently turn an unread node into a
// healthy one. Serialization preserves the distinction because absent fields
// are omitted, and the agent publishes with server-side apply, so a reading
// that was present last cycle and unreadable this cycle disappears from the
// object rather than going stale.
type HaloNodeObservationSpec struct {
	// ObservedAt is when the agent took these readings. The controller treats
	// an observation older than its freshness window as no observation at all.
	ObservedAt metav1.Time `json:"observedAt"`

	// KernelCmdline is the raw contents of /proc/cmdline, unparsed. Raw on
	// purpose: parsing is interpretation and lives in one place, the
	// controller (internal/memorypolicy.ParseKernelCmdline). Carrying the raw
	// line also keeps "read fine, nothing set" (present, empty) structurally
	// distinct from "could not read" (absent) — a parsed map cannot make that
	// distinction survive JSON, because an empty map serializes away.
	// +optional
	KernelCmdline *string `json:"kernelCmdline,omitempty"`

	// KernelRelease is the release of the running kernel, from
	// /proc/sys/kernel/osrelease (what `uname -r` prints).
	//
	// Every other reading here is only interpretable against it. On the AMD
	// Ryzen AI Developer Platform the installed kernels do not boot to the same
	// memory configuration — one kernel's EFI-partition initramfs carries
	// /etc/modprobe.d/ttm.conf and another's does not (halo-operator#20) — so a
	// GTT reading without the kernel it was taken on cannot say whether the
	// node is configured or merely happened to boot the configured kernel.
	// +optional
	KernelRelease *string `json:"kernelRelease,omitempty"`

	// ModuleParams is the running value of each readable kernel module
	// parameter that governs unified memory, keyed exactly as the kernel
	// command line would name it — "ttm.pages_limit", "amdgpu.gttsize".
	//
	// It exists because the command line is not the only way such a parameter
	// gets set, and on the AMD Ryzen AI Developer Platform it is not the way
	// that is used: the platform's ttm-config.service writes
	// /etc/modprobe.d/ttm.conf, so a correctly sized unit has ttm.pages_limit
	// in effect and nowhere on /proc/cmdline. Judging that node by its command
	// line alone reports permanent drift on a node that is exactly right.
	//
	// A key is absent when that one parameter could not be read, which is
	// routine rather than exceptional: /sys/module/amdgpu/parameters/* is mode
	// 0400 and the agent is unprivileged. The controller treats an absent key
	// as not observed, never as unset — accusing a node of drift on the
	// strength of a permission error is the failure this type exists to avoid.
	// +optional
	ModuleParams map[string]string `json:"moduleParams,omitempty"`

	// GTTTotalBytes is the amdgpu GTT segment size in bytes, read from the
	// device's mem_info_gtt_total. This is the node's effective GTT-visible
	// GPU memory ceiling, the thing amdgpu.gttsize and ttm.pages_limit exist
	// to raise.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GTTTotalBytes *int64 `json:"gttTotalBytes,omitempty"`

	// GPUBusyPercent is the device's gpu_busy_percent, 0 to 100.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GPUBusyPercent *int32 `json:"gpuBusyPercent,omitempty"`

	// SCLKMHz is the shader clock currently selected in the pp_dpm_sclk
	// ladder, in MHz.
	// +optional
	// +kubebuilder:validation:Minimum=0
	SCLKMHz *int32 `json:"sclkMHz,omitempty"`

	// RatedSCLKMHz is the top of the pp_dpm_sclk ladder, the clock the part
	// is rated to reach, in MHz.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RatedSCLKMHz *int32 `json:"ratedSCLKMHz,omitempty"`

	// PermissionFaults counts PERMISSION_FAULT kernel-log entries in the
	// agent's sampling window.
	//
	// No reader sets this yet. The kernel-log line formats are hardware facts
	// to be read off a physical gfx1151 unit, and an unvalidated pattern that
	// matches nothing would report "0 faults" — a clean bill of health this
	// operator has no grounds to issue. Until the patterns are validated the
	// field stays absent and the probe correctly reports NotObserved.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PermissionFaults *int32 `json:"permissionFaults,omitempty"`

	// QueueCreateFailures counts KFD queue-creation failures in the agent's
	// sampling window. Absent for the same reason as PermissionFaults.
	// +optional
	// +kubebuilder:validation:Minimum=0
	QueueCreateFailures *int32 `json:"queueCreateFailures,omitempty"`

	// Messages says, for a human, why readings above are absent — one entry
	// per reading that could not be taken. The controller never acts on
	// these; absence itself carries the semantics.
	// +optional
	Messages []string `json:"messages,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=hno
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=`.spec.observedAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HaloNodeObservation is what the node agent actually read off one node. It is
// named after the node it describes and owned by that Node object, so deleting
// the node garbage-collects the observation.
//
// The agent is the only writer. It holds create/patch on this resource and
// nothing else in the cluster — in particular it can never write Nodes, which
// is what keeps a read-only observer from needing cluster-mutating power.
type HaloNodeObservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HaloNodeObservationSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HaloNodeObservationList contains a list of HaloNodeObservation.
type HaloNodeObservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HaloNodeObservation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HaloNodeObservation{}, &HaloNodeObservationList{})
}
