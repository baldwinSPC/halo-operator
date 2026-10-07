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

	// DRMRenderNode is the DRM render node the kernel registered for the
	// amdgpu device ("renderD128"), read from the device's drm directory in
	// sysfs. It is the kernel interface Vulkan needs from the node, and ROCm
	// needs it too.
	//
	// Present and empty is a reading: the device's drm directory was read and
	// lists no render node, so the node cannot run either backend. Absent
	// means it could not be read.
	// +optional
	DRMRenderNode *string `json:"drmRenderNode,omitempty"`

	// GFXTargetVersion is the gfx_target_version of the GPU compute agent in
	// KFD's topology (110501 is gfx1151). KFD, the kernel's /dev/kfd
	// interface, is what ROCm needs from the node beyond the render node.
	//
	// Zero is a reading, not an absence. It is what KFD itself reports for an
	// agent that is not a GPU, and it is published when the topology was read
	// and lists no GPU agent, or when the kernel has no KFD at all. Absent
	// means the topology could not be read, or it lists more than one GPU
	// agent and the agent will not choose between them.
	// +optional
	// +kubebuilder:validation:Minimum=0
	GFXTargetVersion *int32 `json:"gfxTargetVersion,omitempty"`

	// AMDGPUTaint is /sys/module/amdgpu/taint: the kernel's taint flags for
	// the loaded amdgpu module, verbatim. Empty is a reading, the inbox
	// driver with nothing to declare; "O" marks an out-of-tree module, which
	// is how amdgpu-dkms shows up. Absent means it could not be read.
	// +optional
	AMDGPUTaint *string `json:"amdgpuTaint,omitempty"`

	// TTMModule names the TTM memory manager module that is loaded: "ttm"
	// for the kernel's own, or "amdttm" / "amd_ttm" for the copies
	// amdgpu-dkms ships. GTT sizing lives in that module's pages_limit, so
	// on a DKMS host ttm.pages_limit is read from amdttm. Empty is a reading:
	// none is loaded. Absent means it could not be established.
	// +optional
	TTMModule *string `json:"ttmModule,omitempty"`

	// FirmwareVersions is the GPU's per-block firmware versions, one entry
	// per readable file of its fw_version directory in sysfs ("mes",
	// "smc", ...), verbatim. On amd-halo every entry is mode 0444.
	// +optional
	FirmwareVersions map[string]string `json:"firmwareVersions,omitempty"`

	// VBIOSVersion is the GPU's vbios_version.
	// +optional
	VBIOSVersion *string `json:"vbiosVersion,omitempty"`

	// ModprobeOptions is every `options` line in /etc/modprobe.d/*.conf for
	// the modules whose parameters are read above, verbatim, in the order
	// modprobe reads the files. It is the declared intent the running values
	// are compared against, so a value staged on disk but not yet in effect
	// reads as RebootPending rather than as unconfigured (#31). It is evidence
	// for the reason, never the verdict: modprobe.d is not what the running
	// kernel has. Absent when nothing was read or nothing is declared.
	// +optional
	ModprobeOptions []string `json:"modprobeOptions,omitempty"`

	// PermissionFaults counts GPU page faults whose status carries
	// PERMISSION_FAULTS in the agent's kernel-log window (ROCm#6186's storm).
	//
	// Read only when the agent is given CAP_SYSLOG (agent.readKernelLog in the
	// chart): amd-halo runs with kernel.dmesg_restrict=1. Without it the field
	// is absent and the probe reports NotObserved, never a counted zero. The
	// pattern is amdgpu's own format string for gfx11.5, checked against the
	// dmesg real gfx1151 units logged in ROCm#6186 and #6118. The kernel
	// rate-limits these prints, so a count is a lower bound.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PermissionFaults *int32 `json:"permissionFaults,omitempty"`

	// QueueCreateFailures counts GPU page faults from the CPF client, the
	// command processor fetching a queue, in the same window: the fault
	// hsa_queue_create triggers in ROCm#6118. Absent for the same reason as
	// PermissionFaults.
	// +optional
	// +kubebuilder:validation:Minimum=0
	QueueCreateFailures *int32 `json:"queueCreateFailures,omitempty"`

	// Messages says, for a human, why readings above are absent — one entry
	// per reading that could not be taken. The controller never acts on
	// these; absence itself carries the semantics.
	// +optional
	Messages []string `json:"messages,omitempty"`

	// BootHold is what the agent read about the boot hold — the `splash`
	// token that makes a headless unit wait for a display it does not have.
	//
	// The running kernel's own command line is KernelCmdline above; this
	// carries only the PERSISTED sources, which decide the NEXT boot.
	// +optional
	BootHold *BootHoldReading `json:"bootHold,omitempty"`
}

// BootHoldReading is the persisted boot configuration, as the agent found it.
//
// It is a struct rather than loose fields because the readings are only
// meaningful together: whether a file carries the token means nothing without
// knowing whether every file was seen.
type BootHoldReading struct {
	// EtcKernelCmdline is the verbatim contents of /etc/kernel/cmdline, absent
	// when it could not be read. Measured on RAH-001 it carries a leading and
	// a trailing space, so it is stored unmodified and parsed by the reader.
	// +optional
	EtcKernelCmdline *string `json:"etcKernelCmdline,omitempty"`

	// LoaderEntriesListed says the agent successfully listed the loader
	// entries directory. Absent when the agent did not attempt it; false when
	// the listing itself failed.
	//
	// A FAILED LISTING IS NOT AN EMPTY ONE, and this field is the only thing
	// that distinguishes them. `filepath.Glob` cannot: it returns the same
	// empty slice and nil error for a missing directory, an unreadable one and
	// an empty one.
	// +optional
	LoaderEntriesListed *bool `json:"loaderEntriesListed,omitempty"`

	// LoaderEntriesFound is how many entry files the listing returned.
	//
	// It may exceed len(LoaderEntries): the listing can succeed while
	// individual entries cannot be opened, and those land in Unreadable. Zero
	// is never an answer about the boot hold — a booted Boot Loader
	// Specification unit has at least one entry, so a zero here is an
	// untriggered autofs mount or a directory the agent cannot enter.
	// +optional
	// +kubebuilder:validation:Minimum=0
	LoaderEntriesFound *int32 `json:"loaderEntriesFound,omitempty"`

	// LoaderEntries is one record per entry file the agent opened.
	// +optional
	LoaderEntries []LoaderEntryReading `json:"loaderEntries,omitempty"`

	// Unreadable names every path the agent could not read, with the reason.
	// Reading /efi/loader/entries needs root; a permission denial is NOT
	// evidence that the entries are clean.
	// +optional
	Unreadable []string `json:"unreadable,omitempty"`
}

// LoaderEntryReading is one Boot Loader Specification type#1 entry.
type LoaderEntryReading struct {
	// Path is the file the agent opened, reported verbatim so a human is sent
	// to the entry rather than to a category.
	Path string `json:"path"`

	// Options is the entry's `options` line, stored unmodified.
	// +optional
	Options string `json:"options,omitempty"`
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
