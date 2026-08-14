// Copyright 2026 the glimmer-halo-operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// DefaultBackend is the runtime backend when none is set. Vulkan is the
// reliability and single-stream performance choice on gfx1151.
const DefaultBackend = "vulkan"

// DefaultMetricsPort is the exporter port when none is set.
const DefaultMetricsPort int32 = 9110

// KnownProbes is every health probe, in the order they are defaulted.
var KnownProbes = []string{"clock-under-load", "permission-fault", "queue-create"}

// WithDefaults returns a copy of the spec with unset fields resolved.
// Defaults apply at reconcile time and are never persisted, so a default
// change takes effect without rewriting stored objects.
func (s HaloDeviceConfigSpec) WithDefaults() HaloDeviceConfigSpec {
	out := *s.DeepCopy()
	out.DevicePlugin.Enabled = defaultTrue(out.DevicePlugin.Enabled)
	out.Labeller.Enabled = defaultTrue(out.Labeller.Enabled)
	out.MetricsExporter.Enabled = defaultTrue(out.MetricsExporter.Enabled)
	out.MemoryPolicy.Enabled = defaultTrue(out.MemoryPolicy.Enabled)
	out.HealthChecks.Enabled = defaultTrue(out.HealthChecks.Enabled)
	if out.MetricsExporter.Port == 0 {
		out.MetricsExporter.Port = DefaultMetricsPort
	}
	if out.RuntimeProfile.Backend == "" {
		out.RuntimeProfile.Backend = DefaultBackend
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
