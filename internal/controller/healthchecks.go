// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
)

// HealthObserver supplies health readings for the nodes a config targets.
//
// Like NodeObserver, it exists so that every decision above it is exercised
// against a fixture in CI. The implementation has to run on the node to read
// sysfs and the kernel log, and does not exist yet.
type HealthObserver interface {
	ObserveHealth(ctx context.Context, selector map[string]string) ([]healthchecks.Observation, error)
}

// evaluateHealthChecks runs the configured probes across the targeted nodes and
// reduces the results to a component status and a condition.
//
// The three outcomes map onto the three condition statuses: healthy is True,
// a detected fault is False, and anything unproven is Unknown.
func evaluateHealthChecks(
	ctx context.Context,
	observer HealthObserver,
	cfg *halov1alpha1.HaloDeviceConfig,
	spec halov1alpha1.HaloDeviceConfigSpec,
	now metav1.Time,
) (halov1alpha1.ComponentStatus, metav1.Condition) {
	status := halov1alpha1.ComponentStatus{Name: componentHealthChecks, LastObserved: now}
	condition := metav1.Condition{
		Type:               halov1alpha1.ConditionNodesHealthy,
		ObservedGeneration: cfg.Generation,
	}

	if !*spec.HealthChecks.Enabled {
		status.State = halov1alpha1.ComponentDisabled
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "Disabled"
		condition.Message = "the health checks component is disabled on this config"
		return status, condition
	}

	if observer == nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "no node observation source is configured, so node health cannot be established"
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NoObservationSource"
		condition.Message = status.Message
		return status, condition
	}

	observations, err := observer.ObserveHealth(ctx, spec.NodeSelector)
	if err != nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "observing the targeted nodes failed: " + err.Error()
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ObservationFailed"
		condition.Message = status.Message
		return status, condition
	}

	var results []healthchecks.ProbeResult
	for _, obs := range observations {
		results = append(results,
			healthchecks.Evaluate(spec.HealthChecks.Probes, healthchecks.DefaultThresholds, obs)...)
	}
	summary := healthchecks.Summarize(results)

	condition.Message = summary.Message
	status.Message = summary.Message

	switch summary.Result {
	case healthchecks.ResultHealthy:
		status.State = halov1alpha1.ComponentReady
		condition.Status = metav1.ConditionTrue
		condition.Reason = "AllProbesHealthy"
	case healthchecks.ResultUnhealthy:
		status.State = halov1alpha1.ComponentDegraded
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ProbeFailed"
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
