// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
)

// RuntimeObserver supplies, for the nodes a config targets, the readings that
// say whether each compute backend can run there.
type RuntimeObserver interface {
	ObserveRuntime(ctx context.Context, selector map[string]string) ([]runtimeprofile.Observation, error)
}

// evaluateRuntimeProfile checks the declared backends across the targeted
// nodes and reduces the results to a component status and a condition.
//
// The three outcomes map onto the three condition statuses: every backend
// available is True, a backend observed unable to run is False, and anything
// unproven is Unknown.
func evaluateRuntimeProfile(
	ctx context.Context,
	observer RuntimeObserver,
	cfg *halov1alpha1.HaloDeviceConfig,
	spec halov1alpha1.HaloDeviceConfigSpec,
	now metav1.Time,
) (halov1alpha1.ComponentStatus, metav1.Condition) {
	status := halov1alpha1.ComponentStatus{Name: componentRuntimeProfile, LastObserved: now}
	condition := metav1.Condition{
		Type:               halov1alpha1.ConditionBackendsAvailable,
		ObservedGeneration: cfg.Generation,
	}

	if !*spec.RuntimeProfile.Enabled {
		status.State = halov1alpha1.ComponentDisabled
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "Disabled"
		condition.Message = "the runtime profile component is disabled on this config"
		return status, condition
	}

	if observer == nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "no node observation source is configured, so backend availability cannot be established"
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NoObservationSource"
		condition.Message = status.Message
		return status, condition
	}

	observations, err := observer.ObserveRuntime(ctx, spec.NodeSelector)
	if err != nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "observing the targeted nodes failed: " + err.Error()
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ObservationFailed"
		condition.Message = status.Message
		return status, condition
	}

	var results []runtimeprofile.BackendResult
	for _, obs := range observations {
		results = append(results, runtimeprofile.Evaluate(spec.RuntimeProfile.Backends, obs)...)
	}
	summary := runtimeprofile.Summarize(results)

	condition.Message = summary.Message
	status.Message = summary.Message

	switch summary.Result {
	case runtimeprofile.ResultAvailable:
		status.State = halov1alpha1.ComponentReady
		condition.Status = metav1.ConditionTrue
		condition.Reason = "AllBackendsAvailable"
	case runtimeprofile.ResultUnavailable:
		status.State = halov1alpha1.ComponentDegraded
		condition.Status = metav1.ConditionFalse
		condition.Reason = "BackendUnavailable"
		if summary.Detail != "" {
			condition.Message = summary.Message + ": " + summary.Detail
			status.Message = condition.Message
		}
	default:
		status.State = halov1alpha1.ComponentNotObserved
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NotObserved"
	}
	return status, condition
}
