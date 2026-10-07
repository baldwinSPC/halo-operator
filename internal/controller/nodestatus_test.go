// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
)

// TestNodeStatusesNameEveryNode is #30's acceptance shape: with two nodes,
// one drifted and one unread, a reader can name both from the status. The
// condition alone says "1 node drifted" and names only the first.
func TestNodeStatusesNameEveryNode(t *testing.T) {
	r := &HaloDeviceConfigReconciler{
		MemoryObserver: stubObserver{observations: []memorypolicy.Observation{
			{Node: "halo-ok", KernelParams: map[string]string{"amd_iommu": "off"}},
			{Node: "halo-drifted", KernelParams: map[string]string{"amd_iommu": "on"}},
			{Node: "halo-unread"},
		}},
	}
	spec := halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: halov1alpha1.MemoryPolicySpec{
		ExpectedKernelArgs: map[string]string{"amd_iommu": "off"}}}.WithDefaults()

	nodes, omitted := r.nodeStatuses(context.Background(), spec, metav1.Now())
	if omitted != 0 {
		t.Errorf("omitted = %d, want 0", omitted)
	}
	want := []struct {
		name  string
		state halov1alpha1.ComponentState
	}{
		// Degraded first, then not observed, then Ready: a bound on the list
		// must never cut the nodes a reader is looking for.
		{"halo-drifted", halov1alpha1.ComponentDegraded},
		{"halo-unread", halov1alpha1.ComponentNotObserved},
		{"halo-ok", halov1alpha1.ComponentReady},
	}
	if len(nodes) != len(want) {
		t.Fatalf("got %d nodes, want %d: %+v", len(nodes), len(want), nodes)
	}
	for i, w := range want {
		if nodes[i].Name != w.name {
			t.Errorf("nodes[%d] = %s, want %s", i, nodes[i].Name, w.name)
			continue
		}
		if len(nodes[i].Components) != 1 || nodes[i].Components[0].State != w.state {
			t.Errorf("%s components = %+v, want one memory-policy component %s", w.name, nodes[i].Components, w.state)
		}
		if nodes[i].Components[0].Message == "" {
			t.Errorf("%s has an empty message; every per-node state must say why", w.name)
		}
	}
}

func TestNodeStatusesAreBounded(t *testing.T) {
	var obs []memorypolicy.Observation
	for i := 0; i < halov1alpha1.MaxNodeStatuses+10; i++ {
		obs = append(obs, memorypolicy.Observation{Node: fmt.Sprintf("n%04d", i), KernelParams: map[string]string{"amd_iommu": "off"}})
	}
	obs = append(obs, memorypolicy.Observation{Node: "zz-drifted", KernelParams: map[string]string{"amd_iommu": "on"}})
	r := &HaloDeviceConfigReconciler{MemoryObserver: stubObserver{observations: obs}}
	spec := halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: halov1alpha1.MemoryPolicySpec{
		ExpectedKernelArgs: map[string]string{"amd_iommu": "off"}}}.WithDefaults()

	nodes, omitted := r.nodeStatuses(context.Background(), spec, metav1.Now())
	if len(nodes) != halov1alpha1.MaxNodeStatuses || omitted != 11 {
		t.Errorf("listed %d, omitted %d; want %d and 11", len(nodes), omitted, halov1alpha1.MaxNodeStatuses)
	}
	if nodes[0].Name != "zz-drifted" {
		t.Errorf("the drifted node sorted last by name was cut or not first: nodes[0] = %s", nodes[0].Name)
	}
}
