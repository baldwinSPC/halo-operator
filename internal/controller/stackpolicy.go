// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/observe"
	"github.com/baldwinSPC/halo-operator/internal/stackpolicy"
)

// StackObserver supplies, for the nodes a config targets, the driver, kernel
// and GPU firmware readings the stack policy judges.
type StackObserver interface {
	ObserveStack(ctx context.Context, selector map[string]string) ([]stackpolicy.Observation, error)
}

// evaluateStackPolicy produces the stack-policy component and its two
// conditions: StackCompliant for the driver and kernel, GPUFirmwareCompliant
// for the known-bad GPU firmware list. They are separate conditions because
// they are fixed by different actions and consumers observe them separately.
// The component is the worse of the two.
func evaluateStackPolicy(
	ctx context.Context,
	observer StackObserver,
	cfg *halov1alpha1.HaloDeviceConfig,
	spec halov1alpha1.HaloDeviceConfigSpec,
	now metav1.Time,
) (halov1alpha1.ComponentStatus, metav1.Condition, metav1.Condition) {
	status := halov1alpha1.ComponentStatus{Name: componentStackPolicy, LastObserved: now}
	stack := metav1.Condition{Type: halov1alpha1.ConditionStackCompliant, ObservedGeneration: cfg.Generation}
	firmware := metav1.Condition{Type: halov1alpha1.ConditionGPUFirmwareCompliant, ObservedGeneration: cfg.Generation}

	unknown := func(reason, message string) (halov1alpha1.ComponentStatus, metav1.Condition, metav1.Condition) {
		for _, c := range []*metav1.Condition{&stack, &firmware} {
			c.Status = metav1.ConditionUnknown
			c.Reason = reason
			c.Message = message
		}
		status.Message = message
		return status, stack, firmware
	}

	if !*spec.StackPolicy.Enabled {
		status.State = halov1alpha1.ComponentDisabled
		return unknown("Disabled", "the stack policy component is disabled on this config")
	}
	status.State = halov1alpha1.ComponentNotObserved
	if observer == nil {
		return unknown("NoObservationSource",
			"no node observation source is configured, so the node stack cannot be established")
	}
	observations, err := observer.ObserveStack(ctx, spec.NodeSelector)
	if err != nil {
		return unknown("ObservationFailed", "observing the targeted nodes failed: "+err.Error())
	}

	policy := observe.StackPolicy(spec.StackPolicy)
	var stackFindings, firmwareFindings []stackpolicy.Finding
	for _, obs := range observations {
		stackFindings = append(stackFindings, stackpolicy.EvaluateStack(policy, obs))
		firmwareFindings = append(firmwareFindings, stackpolicy.EvaluateFirmware(policy, obs))
	}
	stackSummary := stackpolicy.Summarize("stack", stackFindings)
	firmwareSummary := stackpolicy.Summarize("GPU firmware", firmwareFindings)
	setFromSummary(&stack, stackSummary, "Compliant", "Drifted")
	setFromSummary(&firmware, firmwareSummary, "Compliant", "KnownBadFirmware")

	switch {
	case stackSummary.Verdict == stackpolicy.Drifted || firmwareSummary.Verdict == stackpolicy.Drifted:
		status.State = halov1alpha1.ComponentDegraded
	case stackSummary.Verdict == stackpolicy.Compliant && firmwareSummary.Verdict == stackpolicy.Compliant:
		status.State = halov1alpha1.ComponentReady
	default:
		status.State = halov1alpha1.ComponentNotObserved
	}
	status.Message = stackSummary.Message + "; " + firmwareSummary.Message
	return status, stack, firmware
}

func setFromSummary(c *metav1.Condition, s stackpolicy.Summary, compliantReason, driftedReason string) {
	c.Message = s.Message
	switch s.Verdict {
	case stackpolicy.Compliant:
		c.Status = metav1.ConditionTrue
		c.Reason = compliantReason
	case stackpolicy.Drifted:
		c.Status = metav1.ConditionFalse
		c.Reason = driftedReason
	default:
		c.Status = metav1.ConditionUnknown
		c.Reason = "NotObserved"
	}
}
