// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "testing"

func TestWithDefaultsEmptySpec(t *testing.T) {
	got := HaloDeviceConfigSpec{}.WithDefaults()

	for name, enabled := range map[string]*bool{
		"memoryPolicy":   got.MemoryPolicy.Enabled,
		"runtimeProfile": got.RuntimeProfile.Enabled,
		"healthChecks":   got.HealthChecks.Enabled,
	} {
		if enabled == nil || !*enabled {
			t.Errorf("%s should default to enabled", name)
		}
	}
	if len(got.RuntimeProfile.Backends) != len(KnownBackends) {
		t.Errorf("backends = %v, want all of %v", got.RuntimeProfile.Backends, KnownBackends)
	}
	if len(got.HealthChecks.Probes) != len(KnownProbes) {
		t.Errorf("probes = %v, want all of %v", got.HealthChecks.Probes, KnownProbes)
	}
}

func TestWithDefaultsPreservesSetFields(t *testing.T) {
	f := false
	in := HaloDeviceConfigSpec{
		MemoryPolicy:   MemoryPolicySpec{Enabled: &f},
		RuntimeProfile: RuntimeProfileSpec{Backends: []string{"vulkan"}},
		HealthChecks:   HealthChecksSpec{Probes: []string{"clock-under-load"}},
	}
	got := in.WithDefaults()

	if got.MemoryPolicy.Enabled == nil || *got.MemoryPolicy.Enabled {
		t.Error("explicit memoryPolicy.enabled=false was overridden")
	}
	if len(got.RuntimeProfile.Backends) != 1 || got.RuntimeProfile.Backends[0] != "vulkan" {
		t.Errorf("backends = %v, want the explicit single backend", got.RuntimeProfile.Backends)
	}
	if len(got.HealthChecks.Probes) != 1 || got.HealthChecks.Probes[0] != "clock-under-load" {
		t.Errorf("probes = %v, want the explicit single probe", got.HealthChecks.Probes)
	}
}

func TestWithDefaultsDoesNotMutateReceiver(t *testing.T) {
	in := HaloDeviceConfigSpec{}
	_ = in.WithDefaults()
	if in.MemoryPolicy.Enabled != nil || in.RuntimeProfile.Enabled != nil || in.RuntimeProfile.Backends != nil {
		t.Error("WithDefaults mutated its receiver")
	}
}

// TestWithDefaultsDoesNotAliasKnownBackends guards the package-level default:
// a caller appending to its resolved spec must not grow every later default.
func TestWithDefaultsDoesNotAliasKnownBackends(t *testing.T) {
	got := HaloDeviceConfigSpec{}.WithDefaults()
	got.RuntimeProfile.Backends[0] = "mutated"
	if KnownBackends[0] == "mutated" {
		t.Fatal("WithDefaults handed out KnownBackends itself rather than a copy")
	}
}
