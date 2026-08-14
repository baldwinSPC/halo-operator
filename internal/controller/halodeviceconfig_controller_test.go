// Copyright 2026 the glimmer-halo-operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/glimmer-halo-operator/api/v1alpha1"
)

func TestComponentStatusesDefaultSpec(t *testing.T) {
	got := componentStatuses(halov1alpha1.HaloDeviceConfigSpec{}.WithDefaults(), metav1.Now())

	if len(got) != 6 {
		t.Fatalf("got %d components, want 6", len(got))
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
		MetricsExporter: halov1alpha1.MetricsExporterSpec{Enabled: &f},
	}.WithDefaults()

	for _, c := range componentStatuses(spec, metav1.Now()) {
		if c.Name != "metrics-exporter" {
			continue
		}
		if c.State != halov1alpha1.ComponentDisabled {
			t.Errorf("metrics-exporter state = %s, want Disabled", c.State)
		}
		return
	}
	t.Fatal("metrics-exporter missing from component statuses")
}
