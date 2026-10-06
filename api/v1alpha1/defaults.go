// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// KnownBackends is every compute backend, in the order they are defaulted.
var KnownBackends = []string{"vulkan", "rocm"}

// KnownProbes is every health probe, in the order they are defaulted.
var KnownProbes = []string{"clock-under-load", "permission-fault", "queue-create"}

// WithDefaults returns a copy of the spec with unset fields resolved.
// Defaults apply at reconcile time and are never persisted, so a default
// change takes effect without rewriting stored objects.
func (s HaloDeviceConfigSpec) WithDefaults() HaloDeviceConfigSpec {
	out := *s.DeepCopy()
	out.MemoryPolicy.Enabled = defaultTrue(out.MemoryPolicy.Enabled)
	out.RuntimeProfile.Enabled = defaultTrue(out.RuntimeProfile.Enabled)
	out.HealthChecks.Enabled = defaultTrue(out.HealthChecks.Enabled)
	if len(out.RuntimeProfile.Backends) == 0 {
		out.RuntimeProfile.Backends = append([]string(nil), KnownBackends...)
	}
	if len(out.HealthChecks.Probes) == 0 {
		out.HealthChecks.Probes = append([]string(nil), KnownProbes...)
	}
	return out
}

func defaultTrue(b *bool) *bool {
	if b == nil {
		t := true
		return &t
	}
	return b
}
