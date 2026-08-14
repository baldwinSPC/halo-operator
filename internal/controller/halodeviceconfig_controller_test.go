// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

func TestComponentStatusesDefaultSpec(t *testing.T) {
	got := componentStatuses(halov1alpha1.HaloDeviceConfigSpec{}.WithDefaults(), metav1.Now())

	if len(got) != 3 {
		t.Fatalf("got %d components, want 3", len(got))
	}
	for _, c := range got {
		if c.State != halov1alpha1.ComponentNotImplemented {
			t.Errorf("%s state = %s, want NotImplemented on a default spec", c.Name, c.State)
		}
		if c.LastObserved.IsZero() {
			t.Errorf("%s has no observation timestamp", c.Name)
		}
	}
}

func TestComponentStatusesDisabledIsDisabledNotOff(t *testing.T) {
	f := false
	spec := halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{Enabled: &f},
	}.WithDefaults()

	for _, c := range componentStatuses(spec, metav1.Now()) {
		if c.Name != "memory-policy" {
			continue
		}
		if c.State != halov1alpha1.ComponentDisabled {
			t.Errorf("memory-policy state = %s, want Disabled", c.State)
		}
		return
	}
	t.Fatal("memory-policy missing from component statuses")
}
