// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

// NodeObserver supplies memory observations for the nodes a config targets.
//
// It is an interface so that every decision above it runs against a fixture in
// CI. The implementation that actually reads /proc/cmdline and the GTT counters
// has to run on the node itself and does not exist yet; until it does, the
// reconciler is configured without one and reports NotObserved rather than
// guessing.
type NodeObserver interface {
	Observe(ctx context.Context, selector map[string]string) ([]memorypolicy.Observation, error)
}

// evaluateMemoryPolicy observes the targeted nodes and reduces what was found
// to a component status and a condition.
//
// The three outcomes map onto the three condition statuses Kubernetes already
// has, which is why no fourth state is invented here: compliant is True,
// drifted is False, and not-observed is Unknown. Unknown is a real answer and
// the only honest one when no node has been read.
func evaluateMemoryPolicy(
	ctx context.Context,
	observer NodeObserver,
	cfg *halov1alpha1.HaloDeviceConfig,
	spec halov1alpha1.HaloDeviceConfigSpec,
	now metav1.Time,
) (halov1alpha1.ComponentStatus, metav1.Condition) {
	status := halov1alpha1.ComponentStatus{Name: componentMemoryPolicy, LastObserved: now}
	condition := metav1.Condition{
		Type:               halov1alpha1.ConditionMemoryPolicyCompliant,
		ObservedGeneration: cfg.Generation,
	}

	if !*spec.MemoryPolicy.Enabled {
		status.State = halov1alpha1.ComponentDisabled
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "Disabled"
		condition.Message = "the memory policy component is disabled on this config"
		return status, condition
	}

	if observer == nil {
		// No source of truth is wired up. Saying "compliant" here would assert
		// an observation that was never made.
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "no node observation source is configured, so compliance cannot be established"
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NoObservationSource"
		condition.Message = status.Message
		return status, condition
	}

	observations, err := observer.Observe(ctx, spec.NodeSelector)
	if err != nil {
		status.State = halov1alpha1.ComponentNotObserved
		status.Message = "observing the targeted nodes failed: " + err.Error()
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ObservationFailed"
		condition.Message = status.Message
		return status, condition
	}

	policy := observe.MemoryPolicy(spec.MemoryPolicy)
	findings := make([]memorypolicy.Finding, 0, len(observations))
	for _, obs := range observations {
		findings = append(findings, memorypolicy.Evaluate(policy, obs))
	}
	summary := memorypolicy.Summarize(findings)

	condition.Message = summary.Message
	status.Message = summary.Message

	switch summary.Verdict {
	case memorypolicy.VerdictCompliant:
		status.State = halov1alpha1.ComponentReady
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Compliant"
	case memorypolicy.VerdictDrifted:
		status.State = halov1alpha1.ComponentDegraded
		condition.Status = metav1.ConditionFalse
		condition.Reason = "Drifted"
		if summary.RebootPending {
			// Still False: a staged value is not in effect. The reason says
			// the remedy is a reboot, not a configuration change.
			condition.Reason = "RebootPending"
		}
		if detail := firstDriftDetail(findings); detail != "" {
			condition.Message = summary.Message + ": " + detail
			status.Message = condition.Message
		}
	default:
		status.State = halov1alpha1.ComponentNotObserved
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "NotObserved"
	}
	return status, condition
}

// firstDriftDetail names one concrete disagreement, so an operator reading the
// condition has somewhere to start rather than only a count.
func firstDriftDetail(findings []memorypolicy.Finding) string {
	for _, f := range findings {
		if f.Verdict != memorypolicy.VerdictDrifted {
			continue
		}
		if detail := f.Describe(); detail != "" {
			if f.Node != "" {
				return f.Node + ": " + detail
			}
			return detail
		}
	}
	return ""
}
