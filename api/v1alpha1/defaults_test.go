// Copyright 2026 the glimmer-halo-operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestWithDefaultsEmptySpec(t *testing.T) {
	got := HaloDeviceConfigSpec{}.WithDefaults()

	for name, enabled := range map[string]*bool{
		"memoryPolicy": got.MemoryPolicy.Enabled,
		"healthChecks": got.HealthChecks.Enabled,
	} {
		if enabled == nil || !*enabled {
			t.Errorf("%s should default to enabled", name)
		}
	}
	if got.RuntimeProfile.Backend != DefaultBackend {
		t.Errorf("backend = %q, want %q", got.RuntimeProfile.Backend, DefaultBackend)
	}
	if len(got.HealthChecks.Probes) != len(KnownProbes) {
		t.Errorf("probes = %v, want all of %v", got.HealthChecks.Probes, KnownProbes)
	}
}

func TestWithDefaultsPreservesSetFields(t *testing.T) {
	f := false
	in := HaloDeviceConfigSpec{
		MemoryPolicy:   MemoryPolicySpec{Enabled: &f},
		RuntimeProfile: RuntimeProfileSpec{Backend: "rocm"},
		HealthChecks:   HealthChecksSpec{Probes: []string{"clock-under-load"}},
	}
	got := in.WithDefaults()

	if got.MemoryPolicy.Enabled == nil || *got.MemoryPolicy.Enabled {
		t.Error("explicit memoryPolicy.enabled=false was overridden")
	}
	if got.RuntimeProfile.Backend != "rocm" {
		t.Errorf("backend = %q, want rocm", got.RuntimeProfile.Backend)
	}
	if len(got.HealthChecks.Probes) != 1 || got.HealthChecks.Probes[0] != "clock-under-load" {
		t.Errorf("probes = %v, want the explicit single probe", got.HealthChecks.Probes)
	}
}

func TestWithDefaultsDoesNotMutateReceiver(t *testing.T) {
	in := HaloDeviceConfigSpec{}
	_ = in.WithDefaults()
	if in.MemoryPolicy.Enabled != nil || in.RuntimeProfile.Backend != "" {
		t.Error("WithDefaults mutated its receiver")
	}
}
