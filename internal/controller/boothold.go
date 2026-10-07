// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
)

// BootHoldObserver supplies boot-hold readings for the nodes a config targets.
//
// An interface for the same reason NodeObserver is one: every decision above it
// runs against a fixture in CI, on hardware that has never seen an APU.
type BootHoldObserver interface {
	ObserveBootHold(ctx context.Context, selector map[string]string) ([]boothold.NodeResult, error)
}

// evaluateBootHold reduces what the nodes reported to a component status and a
// condition.
//
// There is no spec stanza to enable or disable this. The boot hold is an
// OBSERVATION of the host, not a policy the fleet declares, so there is nothing
// for an operator to configure and nothing this could be switched off against.
// Removing the hold is Glimmer's signed-task path; the only question here is
// what the node currently looks like.
func evaluateBootHold(
	ctx context.Context,
	observer BootHoldObserver,
	cfg *halov1alpha1.HaloDeviceConfig,
	spec halov1alpha1.HaloDeviceConfigSpec,
	now metav1.Time,
) (halov1alpha1.ComponentStatus, metav1.Condition) {
	status := halov1alpha1.ComponentStatus{Name: componentBootHold, LastObserved: now}
	condition := metav1.Condition{
		Type:               halov1alpha1.ConditionBootHoldCleared,
		ObservedGeneration: cfg.Generation,
	}

	if observer == nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "no boot-hold observation source is configured, so whether the boot hold is cleared cannot be established"
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NoObservationSource"
		condition.Message = status.Message
		return status, condition
	}

	results, err := observer.ObserveBootHold(ctx, spec.NodeSelector)
	if err != nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "observing the targeted nodes failed: " + err.Error()
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ObservationFailed"
		condition.Message = status.Message
		return status, condition
	}

	summary := boothold.Summarize(results)
	status.Message = summary.Message
	condition.Message = summary.Message
	condition.Reason = summary.Reason

	switch summary.Verdict {
	case boothold.Cleared:
		status.State = halov1alpha1.ComponentReady
		condition.Status = metav1.ConditionTrue
	case boothold.Held, boothold.RebootPending:
		// Both are False: the unit holds boots either way. The reason carries
		// which, because they call for different actions — one needs a file
		// edited, the other only a restart.
		status.State = halov1alpha1.ComponentDegraded
		condition.Status = metav1.ConditionFalse
	default:
		status.State = halov1alpha1.ComponentNotObserved
		condition.Status = metav1.ConditionUnknown
	}
	return status, condition
}
