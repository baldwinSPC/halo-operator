// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

// FieldManager identifies the agent's server-side-apply ownership. One fixed
// manager for every apply is what lets a reading that disappears this cycle
// also disappear from the object: the apply declares the full set of fields
// the agent currently claims, and the server prunes the rest.
const FieldManager = "halo-node-agent"

// Publisher writes one node's readings to the cluster as a HaloNodeObservation
// named after the node.
type Publisher struct {
	Client client.Client
	// NodeName is this node, from the downward API.
	NodeName string
}

// Publish applies the reading. The observation is owned by the Node object,
// so deleting the node garbage-collects it; publishing fails rather than
// producing an unowned observation, because an orphan would outlive its node
// and go on being read as that node's state.
func (p Publisher) Publish(ctx context.Context, r Reading, at metav1.Time) error {
	var node corev1.Node
	if err := p.Client.Get(ctx, types.NamespacedName{Name: p.NodeName}, &node); err != nil {
		return fmt.Errorf("reading Node %s for ownership: %w", p.NodeName, err)
	}

	obs := buildObservation(p.NodeName, node.UID, r, at)
	if err := p.Client.Patch(ctx, obs, client.Apply,
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying HaloNodeObservation %s: %w", p.NodeName, err)
	}
	return nil
}

// buildObservation lifts a Reading into the API type, one field per reading,
// nil for nil. It adds nothing and infers nothing: the observation is the
// reading, plus the identity and ownership that make it addressable.
func buildObservation(nodeName string, nodeUID types.UID, r Reading, at metav1.Time) *halov1alpha1.HaloNodeObservation {
	return &halov1alpha1.HaloNodeObservation{
		// TypeMeta is set explicitly because an apply patch serializes the
		// object as-is, and without it the request carries no kind.
		TypeMeta: metav1.TypeMeta{
			APIVersion: halov1alpha1.GroupVersion.String(),
			Kind:       "HaloNodeObservation",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       nodeName,
				UID:        nodeUID,
			}},
		},
		Spec: halov1alpha1.HaloNodeObservationSpec{
			ObservedAt:          at,
			KernelCmdline:       r.KernelCmdline,
			ModuleParams:        r.ModuleParams,
			GTTTotalBytes:       r.GTTTotalBytes,
			GPUBusyPercent:      r.GPUBusyPercent,
			SCLKMHz:             r.SCLKMHz,
			RatedSCLKMHz:        r.RatedSCLKMHz,
			Messages:            r.Messages,
			PermissionFaults:    nil, // no validated kernel-log reader yet; see the spec comments
			QueueCreateFailures: nil,
		},
	}
}
