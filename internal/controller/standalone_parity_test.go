// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
	"github.com/baldwinSPC/halo-operator/internal/standalone"
)

// TestStandaloneAgreesWithTheController is the test the standalone binary's
// design rests on. `halo check` claims to judge a node exactly as this
// controller would; that claim is only worth anything if a disagreement fails
// a build. So the same reading goes down both paths — the controller's own
// evaluate functions, fed through the real per-node translation, and
// standalone.Evaluate — and every component state must match.
func TestStandaloneAgreesWithTheController(t *testing.T) {
	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }

	// The physical amd-halo reading, 2026-09-27, unprivileged. The render
	// node and KFD target are from #8's reading of the same unit, 2026-09-14.
	real := halov1alpha1.HaloNodeObservationSpec{
		KernelCmdline:    ptr("root=UUID=643b044c splash quiet loglevel=3"),
		ModuleParams:     map[string]string{"ttm.pages_limit": "24692260"},
		GTTTotalBytes:    i64(101139496960),
		GPUBusyPercent:   i32(0),
		SCLKMHz:          i32(600),
		RatedSCLKMHz:     i32(2900),
		DRMRenderNode:    ptr("renderD128"),
		GFXTargetVersion: i32(110501),
	}
	loaded := real
	loaded.GPUBusyPercent = i32(97)
	loaded.SCLKMHz = i32(600) // stuck at idle under load: ROCm#5750
	noKFD := real
	noKFD.GFXTargetVersion = i32(0)

	readings := map[string]halov1alpha1.HaloNodeObservationSpec{
		"real node":              real,
		"stuck clock under load": loaded,
		"a kernel without KFD":   noKFD,
		"nothing readable":       {},
	}
	policies := map[string]halov1alpha1.HaloDeviceConfigSpec{
		"what the platform configures": {
			MemoryPolicy: halov1alpha1.MemoryPolicySpec{
				ExpectedKernelArgs:       map[string]string{"ttm.pages_limit": "24692260"},
				MinEffectiveGPUMemoryGiB: 90,
			},
			HealthChecks: halov1alpha1.HealthChecksSpec{Probes: []string{"clock-under-load"}},
		},
		"the repository sample": {
			MemoryPolicy: halov1alpha1.MemoryPolicySpec{
				ExpectedKernelArgs: map[string]string{
					"amd_iommu": "off", "amdgpu.gttsize": "126976", "ttm.pages_limit": "32505856"},
				MinEffectiveGPUMemoryGiB: 100,
			},
		},
		"an unreadable module parameter": {
			MemoryPolicy: halov1alpha1.MemoryPolicySpec{
				ExpectedKernelArgs: map[string]string{"amdgpu.gttsize": "126976"}},
		},
		"vulkan only": {
			RuntimeProfile: halov1alpha1.RuntimeProfileSpec{Backends: []string{"vulkan"}},
		},
		"everything disabled": {
			MemoryPolicy:   halov1alpha1.MemoryPolicySpec{Enabled: func() *bool { f := false; return &f }()},
			RuntimeProfile: halov1alpha1.RuntimeProfileSpec{Enabled: func() *bool { f := false; return &f }()},
			HealthChecks:   halov1alpha1.HealthChecksSpec{Enabled: func() *bool { f := false; return &f }()},
		},
	}

	for rName, spec := range readings {
		for pName, policy := range policies {
			t.Run(rName+" / "+pName, func(t *testing.T) {
				cfg := &halov1alpha1.HaloDeviceConfig{Spec: policy}
				resolved := policy.WithDefaults()
				now := metav1.Now()
				s := spec

				// The controller path, with the real per-node translation.
				memObs := stubObserver{observations: []memorypolicy.Observation{observe.MemoryObservation("n", &s)}}
				healthObs := stubHealthObserver{observations: []healthchecks.Observation{observe.HealthObservation("n", &s)}}
				ctrlMem, _ := evaluateMemoryPolicy(context.Background(), memObs, cfg, resolved, now)
				ctrlHealth, _ := evaluateHealthChecks(context.Background(), healthObs, cfg, resolved, now)
				runtimeObs := stubRuntimeObserver{observations: []runtimeprofile.Observation{observe.RuntimeObservation("n", &s)}}
				ctrlRuntime, _ := evaluateRuntimeProfile(context.Background(), runtimeObs, cfg, resolved, now)

				// The standalone path.
				report := standalone.Evaluate("n", spec, *cfg)
				byName := map[string]halov1alpha1.ComponentState{}
				for _, c := range report.Components {
					byName[c.Name] = c.State
				}

				if got := byName[componentMemoryPolicy]; got != ctrlMem.State {
					t.Errorf("memory-policy: standalone %s, controller %s", got, ctrlMem.State)
				}
				if got := byName[componentHealthChecks]; got != ctrlHealth.State {
					t.Errorf("health-checks: standalone %s, controller %s", got, ctrlHealth.State)
				}
				if got := byName[componentRuntimeProfile]; got != ctrlRuntime.State {
					t.Errorf("runtime-profile: standalone %s, controller %s", got, ctrlRuntime.State)
				}
			})
		}
	}
}
