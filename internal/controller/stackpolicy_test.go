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
	"github.com/baldwinSPC/halo-operator/internal/stackpolicy"
)

type stubStackObserver struct {
	observations []stackpolicy.Observation
	err          error
}

func (s stubStackObserver) ObserveStack(context.Context, map[string]string) ([]stackpolicy.Observation, error) {
	return s.observations, s.err
}

// inboxStack is amd-halo as read on 2026-10-06: the inbox amdgpu, the kernel's
// own TTM, and its firmware.
func inboxStack(node string) stackpolicy.Observation {
	return stackpolicy.Observation{
		Node:          node,
		KernelRelease: str("6.18.44+rex+5-amd64"),
		AMDGPUTaint:   str(""),
		TTMModule:     str("ttm"),
		Firmware:      map[string]string{"mes": "0x00000088", "smc": "0x0a640900", "vbios": "113-STRXLGEN-001"},
	}
}

func TestEvaluateStackPolicy(t *testing.T) {
	dkms := inboxStack("halo-2")
	dkms.AMDGPUTaint = str("O")
	dkms.TTMModule = str("amdttm")

	badMES := []halov1alpha1.KnownBadFirmware{{Component: "mes", Version: "0x00000088", Reason: "breaks KFD queues (example)"}}

	cases := []struct {
		name         string
		observer     StackObserver
		policy       halov1alpha1.StackPolicySpec
		wantState    halov1alpha1.ComponentState
		wantStack    metav1.ConditionStatus
		wantFirmware metav1.ConditionStatus
		wantInStack  []string
	}{
		{name: "no observation source is Unknown on both, never compliant",
			wantState: halov1alpha1.ComponentNotObserved, wantStack: metav1.ConditionUnknown, wantFirmware: metav1.ConditionUnknown},
		{name: "a failed observation is Unknown on both",
			observer:  stubStackObserver{err: errors.New("unreachable")},
			wantState: halov1alpha1.ComponentNotObserved, wantStack: metav1.ConditionUnknown, wantFirmware: metav1.ConditionUnknown},
		{name: "zero nodes is Unknown, never compliant",
			observer:  stubStackObserver{},
			wantState: halov1alpha1.ComponentNotObserved, wantStack: metav1.ConditionUnknown, wantFirmware: metav1.ConditionUnknown},
		{name: "the physical unit is compliant with the defaults",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{inboxStack("amd-halo")}},
			wantState: halov1alpha1.ComponentReady, wantStack: metav1.ConditionTrue, wantFirmware: metav1.ConditionTrue},
		{name: "amdgpu-dkms is drift, naming the node and AMD's guidance",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{inboxStack("amd-halo"), dkms}},
			wantState: halov1alpha1.ComponentDegraded, wantStack: metav1.ConditionFalse, wantFirmware: metav1.ConditionTrue,
			wantInStack: []string{"halo-2", "out-of-tree", "ROCm#6186"}},
		{name: "a kernel below the floor is drift",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{inboxStack("amd-halo")}},
			policy:    halov1alpha1.StackPolicySpec{MinKernelVersion: "6.19"},
			wantState: halov1alpha1.ComponentDegraded, wantStack: metav1.ConditionFalse, wantFirmware: metav1.ConditionTrue,
			wantInStack: []string{"below the floor 6.19"}},
		{name: "known-bad firmware is its own condition and leaves the stack alone",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{inboxStack("amd-halo")}},
			policy:    halov1alpha1.StackPolicySpec{KnownBadFirmware: badMES},
			wantState: halov1alpha1.ComponentDegraded, wantStack: metav1.ConditionTrue, wantFirmware: metav1.ConditionFalse},
		{name: "allowing dkms turns the driver check off",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{dkms}},
			policy:    halov1alpha1.StackPolicySpec{RequireInboxDriver: func() *bool { f := false; return &f }()},
			wantState: halov1alpha1.ComponentReady, wantStack: metav1.ConditionTrue, wantFirmware: metav1.ConditionTrue},
		{name: "disabled is Disabled and Unknown",
			observer:  stubStackObserver{observations: []stackpolicy.Observation{dkms}},
			policy:    halov1alpha1.StackPolicySpec{Enabled: func() *bool { f := false; return &f }()},
			wantState: halov1alpha1.ComponentDisabled, wantStack: metav1.ConditionUnknown, wantFirmware: metav1.ConditionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := halov1alpha1.HaloDeviceConfigSpec{StackPolicy: tc.policy}.WithDefaults()
			cfg := &halov1alpha1.HaloDeviceConfig{}
			cfg.Generation = 3
			status, stack, firmware := evaluateStackPolicy(context.Background(), tc.observer, cfg, spec, metav1.Now())
			if status.State != tc.wantState {
				t.Errorf("component = %s (%s), want %s", status.State, status.Message, tc.wantState)
			}
			if stack.Type != halov1alpha1.ConditionStackCompliant || stack.Status != tc.wantStack {
				t.Errorf("%s = %s (%s), want %s", stack.Type, stack.Status, stack.Message, tc.wantStack)
			}
			if firmware.Type != halov1alpha1.ConditionGPUFirmwareCompliant || firmware.Status != tc.wantFirmware {
				t.Errorf("%s = %s (%s), want %s", firmware.Type, firmware.Status, firmware.Message, tc.wantFirmware)
			}
			if stack.ObservedGeneration != 3 || firmware.ObservedGeneration != 3 {
				t.Errorf("observedGeneration = %d/%d, want 3", stack.ObservedGeneration, firmware.ObservedGeneration)
			}
			for _, want := range tc.wantInStack {
				if !strings.Contains(stack.Message, want) {
					t.Errorf("StackCompliant message %q does not mention %q", stack.Message, want)
				}
			}
		})
	}
}

// TestStackPolicyNeverReportsCompliantWithoutObservation mirrors the other
// components' invariant.
func TestStackPolicyNeverReportsCompliantWithoutObservation(t *testing.T) {
	half := inboxStack("halo-2")
	half.AMDGPUTaint = nil
	for name, observer := range map[string]StackObserver{
		"nil observer":             nil,
		"observation failed":       stubStackObserver{err: errors.New("boom")},
		"no nodes":                 stubStackObserver{},
		"node not read":            stubStackObserver{observations: []stackpolicy.Observation{{Node: "halo-1"}}},
		"one node's driver unread": stubStackObserver{observations: []stackpolicy.Observation{inboxStack("halo-1"), half}},
	} {
		t.Run(name, func(t *testing.T) {
			status, stack, firmware := evaluateStackPolicy(context.Background(), observer,
				&halov1alpha1.HaloDeviceConfig{}, halov1alpha1.HaloDeviceConfigSpec{}.WithDefaults(), metav1.Now())
			if status.State == halov1alpha1.ComponentReady {
				t.Error("component reported Ready without an observation")
			}
			if stack.Status == metav1.ConditionTrue {
				t.Error("StackCompliant reported True without every node's driver read")
			}
			if name != "one node's driver unread" && firmware.Status == metav1.ConditionTrue {
				t.Error("GPUFirmwareCompliant reported True without an observation")
			}
		})
	}
}
