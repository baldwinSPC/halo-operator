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
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
)

type stubRuntimeObserver struct {
	observations []runtimeprofile.Observation
	err          error
}

func (s stubRuntimeObserver) ObserveRuntime(context.Context, map[string]string) ([]runtimeprofile.Observation, error) {
	return s.observations, s.err
}

func str(s string) *string { return &s }

// bothBackends is the physical unit's reading: render node renderD128 and a
// KFD GPU agent at gfx_target_version 110501.
func bothBackends(node string) runtimeprofile.Observation {
	return runtimeprofile.Observation{Node: node, RenderNode: str("renderD128"), GFXTargetVersion: i32(110501)}
}

func TestEvaluateRuntimeProfile(t *testing.T) {
	noKFD := runtimeprofile.Observation{Node: "halo-2", RenderNode: str("renderD128"), GFXTargetVersion: i32(0)}

	testCases := []struct {
		name          string
		observer      RuntimeObserver
		backends      []string
		disable       bool
		wantState     halov1alpha1.ComponentState
		wantStatus    metav1.ConditionStatus
		wantReason    string
		wantInMessage []string
	}{
		{
			name:       "no observation source is Unknown, never available",
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NoObservationSource",
		},
		{
			name:       "a failed observation is Unknown, not unavailable",
			observer:   stubRuntimeObserver{err: errors.New("unreachable")},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "ObservationFailed",
		},
		{
			name:       "observing zero nodes is Unknown, never available",
			observer:   stubRuntimeObserver{},
			wantState:  halov1alpha1.ComponentNotObserved,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "NotObserved",
		},
		{
			name:       "both backends available is Ready and True",
			observer:   stubRuntimeObserver{observations: []runtimeprofile.Observation{bothBackends("halo-1")}},
			wantState:  halov1alpha1.ComponentReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllBackendsAvailable",
		},
		{
			name: "a node without a KFD GPU agent is Degraded and False, naming node and backend",
			observer: stubRuntimeObserver{observations: []runtimeprofile.Observation{
				bothBackends("halo-1"), noKFD}},
			wantState:     halov1alpha1.ComponentDegraded,
			wantStatus:    metav1.ConditionFalse,
			wantReason:    "BackendUnavailable",
			wantInMessage: []string{"rocm", "halo-2"},
		},
		{
			name:       "the same node is compliant when only vulkan is declared",
			observer:   stubRuntimeObserver{observations: []runtimeprofile.Observation{noKFD}},
			backends:   []string{"vulkan"},
			wantState:  halov1alpha1.ComponentReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: "AllBackendsAvailable",
		},
		{
			name:       "a disabled component is Disabled and Unknown",
			observer:   stubRuntimeObserver{observations: []runtimeprofile.Observation{bothBackends("halo-1")}},
			disable:    true,
			wantState:  halov1alpha1.ComponentDisabled,
			wantStatus: metav1.ConditionUnknown,
			wantReason: "Disabled",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			raw := halov1alpha1.HaloDeviceConfigSpec{}
			raw.RuntimeProfile.Backends = tc.backends
			if tc.disable {
				f := false
				raw.RuntimeProfile.Enabled = &f
			}
			cfg := &halov1alpha1.HaloDeviceConfig{}
			cfg.Generation = 5

			status, condition := evaluateRuntimeProfile(
				context.Background(), tc.observer, cfg, raw.WithDefaults(), metav1.Now())

			if status.State != tc.wantState {
				t.Errorf("component state = %s, want %s", status.State, tc.wantState)
			}
			if status.Name != componentRuntimeProfile {
				t.Errorf("component name = %s, want %s", status.Name, componentRuntimeProfile)
			}
			if condition.Status != tc.wantStatus {
				t.Errorf("condition status = %s, want %s", condition.Status, tc.wantStatus)
			}
			if condition.Reason != tc.wantReason {
				t.Errorf("condition reason = %s, want %s", condition.Reason, tc.wantReason)
			}
			if condition.Type != halov1alpha1.ConditionBackendsAvailable {
				t.Errorf("condition type = %s, want %s", condition.Type, halov1alpha1.ConditionBackendsAvailable)
			}
			if condition.ObservedGeneration != 5 {
				t.Errorf("condition observedGeneration = %d, want 5", condition.ObservedGeneration)
			}
			for _, want := range tc.wantInMessage {
				if !strings.Contains(condition.Message, want) {
					t.Errorf("condition message %q does not mention %q", condition.Message, want)
				}
			}
		})
	}
}

// TestRuntimeProfileNeverReportsAvailableWithoutObservation mirrors the memory
// policy and health invariants: nothing unproven may read as available.
func TestRuntimeProfileNeverReportsAvailableWithoutObservation(t *testing.T) {
	observers := map[string]RuntimeObserver{
		"nil observer":       nil,
		"observation failed": stubRuntimeObserver{err: errors.New("boom")},
		"no nodes returned":  stubRuntimeObserver{},
		"node not read": stubRuntimeObserver{
			observations: []runtimeprofile.Observation{{Node: "halo-1"}},
		},
		"one node of two not read": stubRuntimeObserver{
			observations: []runtimeprofile.Observation{bothBackends("halo-1"), {Node: "halo-2"}},
		},
	}
	for name, observer := range observers {
		t.Run(name, func(t *testing.T) {
			status, condition := evaluateRuntimeProfile(context.Background(), observer,
				&halov1alpha1.HaloDeviceConfig{}, halov1alpha1.HaloDeviceConfigSpec{}.WithDefaults(), metav1.Now())
			if status.State == halov1alpha1.ComponentReady {
				t.Errorf("component reported Ready without an observation")
			}
			if condition.Status == metav1.ConditionTrue {
				t.Errorf("condition reported True without an observation")
			}
		})
	}
}
