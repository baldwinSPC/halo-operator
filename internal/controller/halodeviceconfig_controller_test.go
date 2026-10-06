// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
)

// stubObserver returns a fixed set of observations, so every decision below the
// observation boundary is exercised without a node or a GPU.
type stubObserver struct {
	observations []memorypolicy.Observation
	err          error
}

func (s stubObserver) Observe(context.Context, map[string]string) ([]memorypolicy.Observation, error) {
	return s.observations, s.err
}

func gib(v int32) *int32 { return &v }

func i32(v int32) *int32 { return &v }

func haloSpec() halov1alpha1.HaloDeviceConfigSpec {
	return halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{
				"amd_iommu":      "off",
				"amdgpu.gttsize": "126976",
			},
			MinEffectiveGPUMemoryGiB: 100,
		},
	}.WithDefaults()
}

func TestEvaluateMemoryPolicy(t *testing.T) {
	compliant := memorypolicy.Observation{
		Node:                  "halo-1",
		KernelParams:          map[string]string{"amd_iommu": "off", "amdgpu.gttsize": "126976"},
		EffectiveGPUMemoryGiB: gib(124),
	}
	drifted := memorypolicy.Observation{
		Node:                  "halo-2",
		KernelParams:          map[string]string{"amd_iommu": "on", "amdgpu.gttsize": "126976"},
		EffectiveGPUMemoryGiB: gib(124),
	}
	unread := memorypolicy.Observation{Node: "halo-3"}

	testCases := []struct {
		name          string
		observer      NodeObserver
		disable       bool
		wantState     halov1alpha1.ComponentState
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantInMessage string
	}{
		{
			name:       "no observation source is Unknown, never compliant",
			observer:   nil,
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoObservationSource",
		},
		{
			name:       "a failed observation is Unknown, not drift",
			observer:   stubObserver{err: errors.New("node unreachable")},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "ObservationFailed",
		},
		{
			name:       "observing zero nodes is Unknown, never compliant",
			observer:   stubObserver{observations: nil},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NotObserved",
		},
		{
			name:       "every node compliant is Ready and True",
			observer:   stubObserver{observations: []memorypolicy.Observation{compliant}},
			wantState:  halov1alpha1.ComponentReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: "Compliant",
		},
		{
			name:          "a drifted node is Degraded and False, and names the parameter",
			observer:      stubObserver{observations: []memorypolicy.Observation{compliant, drifted}},
			wantState:     halov1alpha1.ComponentDegraded,
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "Drifted",
			wantInMessage: "amd_iommu",
		},
		{
			name:       "an unread node blocks a claim of compliance",
			observer:   stubObserver{observations: []memorypolicy.Observation{compliant, unread}},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NotObserved",
		},
		{
			name:       "a disabled component is Disabled and Unknown",
			observer:   stubObserver{observations: []memorypolicy.Observation{compliant}},
			disable:    true,
			wantState:  halov1alpha1.ComponentDisabled,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "Disabled",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			spec := haloSpec()
			if tc.disable {
				f := false
				spec.MemoryPolicy.Enabled = &f
			}
			cfg := &halov1alpha1.HaloDeviceConfig{}
			cfg.Generation = 7

			status, condition := evaluateMemoryPolicy(
				context.Background(), tc.observer, cfg, spec, metav1.Now())

			if status.State != tc.wantState {
				t.Errorf("component state = %s, want %s", status.State, tc.wantState)
			}
			if condition.Status != tc.wantStatus {
				t.Errorf("condition status = %s, want %s", condition.Status, tc.wantStatus)
			}
			if condition.Reason != tc.wantReason {
				t.Errorf("condition reason = %s, want %s", condition.Reason, tc.wantReason)
			}
			if condition.Type != halov1alpha1.ConditionMemoryPolicyCompliant {
				t.Errorf("condition type = %s, want %s",
					condition.Type, halov1alpha1.ConditionMemoryPolicyCompliant)
			}
			if condition.ObservedGeneration != 7 {
				t.Errorf("condition observedGeneration = %d, want 7", condition.ObservedGeneration)
			}
			if condition.Message == "" {
				t.Errorf("condition carries no message")
			}
			if status.LastObserved.IsZero() {
				t.Errorf("component status carries no observation timestamp")
			}
			if tc.wantInMessage != "" && !strings.Contains(condition.Message, tc.wantInMessage) {
				t.Errorf("condition message %q does not name %q", condition.Message, tc.wantInMessage)
			}
		})
	}
}

// TestMemoryPolicyNeverReportsReadyWithoutObservation is the invariant that
// matters: no observer state may produce a Ready component or a True condition
// unless a node was actually read and actually matched.
func TestMemoryPolicyNeverReportsReadyWithoutObservation(t *testing.T) {
	observers := map[string]NodeObserver{
		"nil observer":       nil,
		"observation failed": stubObserver{err: errors.New("boom")},
		"no nodes returned":  stubObserver{observations: nil},
		"node not read":      stubObserver{observations: []memorypolicy.Observation{{Node: "halo-1"}}},
	}
	for name, observer := range observers {
		t.Run(name, func(t *testing.T) {
			status, condition := evaluateMemoryPolicy(
				context.Background(), observer, &halov1alpha1.HaloDeviceConfig{}, haloSpec(), metav1.Now())
			if status.State == halov1alpha1.ComponentReady {
				t.Errorf("component reported Ready without an observation")
			}
			if condition.Status == metav1.ConditionTrue {
				t.Errorf("condition reported True without an observation")
			}
		})
	}
}

func TestReadyCondition(t *testing.T) {
	testCases := []struct {
		name       string
		components []halov1alpha1.ComponentStatus
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "no components is not ready",
			components: nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: "NothingEnabled",
		},
		{
			name: "every component ready is ready",
			components: []halov1alpha1.ComponentStatus{
				{State: halov1alpha1.ComponentReady},
				{State: halov1alpha1.ComponentReady},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllComponentsReady",
		},
		{
			name: "an unobserved component blocks readiness",
			components: []halov1alpha1.ComponentStatus{
				{State: halov1alpha1.ComponentReady},
				{State: halov1alpha1.ComponentNotObserved},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "ComponentsNotReady",
		},
		{
			name: "a degraded component blocks readiness",
			components: []halov1alpha1.ComponentStatus{
				{State: halov1alpha1.ComponentDegraded},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "ComponentsNotReady",
		},
		{
			name: "disabled components alone do not establish readiness",
			components: []halov1alpha1.ComponentStatus{
				{State: halov1alpha1.ComponentDisabled},
				{State: halov1alpha1.ComponentDisabled},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "NothingEnabled",
		},
		{
			name: "a disabled component does not block a ready one",
			components: []halov1alpha1.ComponentStatus{
				{State: halov1alpha1.ComponentReady},
				{State: halov1alpha1.ComponentDisabled},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllComponentsReady",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := readyCondition(tc.components, 3)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", got.Reason, tc.wantReason)
			}
			if got.ObservedGeneration != 3 {
				t.Errorf("observedGeneration = %d, want 3", got.ObservedGeneration)
			}
		})
	}
}

// stubHealthObserver returns fixed health readings, so the controller's mapping
// from probe results to conditions is exercised without a node.
type stubHealthObserver struct {
	observations []healthchecks.Observation
	err          error
}

func (s stubHealthObserver) ObserveHealth(context.Context, map[string]string) ([]healthchecks.Observation, error) {
	return s.observations, s.err
}

func healthySpec() halov1alpha1.HaloDeviceConfigSpec {
	return halov1alpha1.HaloDeviceConfigSpec{}.WithDefaults()
}

func TestEvaluateHealthChecks(t *testing.T) {
	healthy := healthchecks.Observation{
		Node:                "halo-1",
		GPUBusyPercent:      i32(99),
		SCLKMHz:             i32(2900),
		RatedSCLKMHz:        i32(2900),
		PermissionFaults:    i32(0),
		QueueCreateFailures: i32(0),
	}
	clockLocked := healthy
	clockLocked.Node = "halo-2"
	clockLocked.SCLKMHz = i32(200)

	testCases := []struct {
		name          string
		observer      HealthObserver
		disable       bool
		wantState     halov1alpha1.ComponentState
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantInMessage string
	}{
		{
			name:       "no observation source is Unknown, never healthy",
			observer:   nil,
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoObservationSource",
		},
		{
			name:       "a failed observation is Unknown, not a fault",
			observer:   stubHealthObserver{err: errors.New("unreachable")},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "ObservationFailed",
		},
		{
			name:       "observing zero nodes is Unknown, never healthy",
			observer:   stubHealthObserver{},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NotObserved",
		},
		{
			name:       "every probe healthy is Ready and True",
			observer:   stubHealthObserver{observations: []healthchecks.Observation{healthy}},
			wantState:  halov1alpha1.ComponentReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllProbesHealthy",
		},
		{
			name:          "a locked clock is Degraded and False, and cites the defect",
			observer:      stubHealthObserver{observations: []healthchecks.Observation{healthy, clockLocked}},
			wantState:     halov1alpha1.ComponentDegraded,
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "ProbeFailed",
			wantInMessage: "5750",
		},
		{
			name:       "a disabled component is Disabled and Unknown",
			observer:   stubHealthObserver{observations: []healthchecks.Observation{healthy}},
			disable:    true,
			wantState:  halov1alpha1.ComponentDisabled,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "Disabled",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			spec := healthySpec()
			if tc.disable {
				f := false
				spec.HealthChecks.Enabled = &f
			}
			cfg := &halov1alpha1.HaloDeviceConfig{}
			cfg.Generation = 5

			status, condition := evaluateHealthChecks(
				context.Background(), tc.observer, cfg, spec, metav1.Now())

			if status.State != tc.wantState {
				t.Errorf("component state = %s, want %s", status.State, tc.wantState)
			}
			if condition.Status != tc.wantStatus {
				t.Errorf("condition status = %s, want %s", condition.Status, tc.wantStatus)
			}
			if condition.Reason != tc.wantReason {
				t.Errorf("condition reason = %s, want %s", condition.Reason, tc.wantReason)
			}
			if condition.Type != halov1alpha1.ConditionNodesHealthy {
				t.Errorf("condition type = %s, want %s", condition.Type, halov1alpha1.ConditionNodesHealthy)
			}
			if condition.ObservedGeneration != 5 {
				t.Errorf("condition observedGeneration = %d, want 5", condition.ObservedGeneration)
			}
			if tc.wantInMessage != "" && !strings.Contains(condition.Message, tc.wantInMessage) {
				t.Errorf("condition message %q does not cite %q", condition.Message, tc.wantInMessage)
			}
		})
	}
}

// TestHealthChecksNeverReportHealthyWithoutObservation mirrors the memory
// policy invariant: nothing unproven may read as a clean bill of health.
func TestHealthChecksNeverReportHealthyWithoutObservation(t *testing.T) {
	observers := map[string]HealthObserver{
		"nil observer":       nil,
		"observation failed": stubHealthObserver{err: errors.New("boom")},
		"no nodes returned":  stubHealthObserver{},
		"node not read": stubHealthObserver{
			observations: []healthchecks.Observation{{Node: "halo-1"}},
		},
	}
	for name, observer := range observers {
		t.Run(name, func(t *testing.T) {
			status, condition := evaluateHealthChecks(
				context.Background(), observer, &halov1alpha1.HaloDeviceConfig{}, healthySpec(), metav1.Now())
			if status.State == halov1alpha1.ComponentReady {
				t.Errorf("component reported Ready without an observation")
			}
			if condition.Status == metav1.ConditionTrue {
				t.Errorf("condition reported True without an observation")
			}
		})
	}
}
