// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
	"github.com/baldwinSPC/halo-operator/internal/stackpolicy"
)

// ObservationTimer says when each targeted node's fresh observation was taken.
type ObservationTimer interface {
	ObservedAt(ctx context.Context, selector map[string]string) (map[string]metav1.Time, error)
}

// nodeStatuses computes every targeted node's state per enabled component
// (halo-operator#30), through the same decision functions the config-wide
// conditions use, so a node's line can never disagree with the count it
// contributes to. A component whose observer is missing or failed is left out
// of the per-node lists: its config-wide condition already says why.
func (r *HaloDeviceConfigReconciler) nodeStatuses(
	ctx context.Context, spec halov1alpha1.HaloDeviceConfigSpec, now metav1.Time,
) ([]halov1alpha1.NodeStatus, int32) {
	byNode := map[string]*halov1alpha1.NodeStatus{}
	node := func(name string) *halov1alpha1.NodeStatus {
		if n, ok := byNode[name]; ok {
			return n
		}
		n := &halov1alpha1.NodeStatus{Name: name}
		byNode[name] = n
		return n
	}
	add := func(name, component string, state halov1alpha1.ComponentState, message string) {
		n := node(name)
		n.Components = append(n.Components, halov1alpha1.ComponentStatus{
			Name: component, State: state, Message: message, LastObserved: now})
	}

	if *spec.MemoryPolicy.Enabled && r.MemoryObserver != nil {
		if obs, err := r.MemoryObserver.Observe(ctx, spec.NodeSelector); err == nil {
			policy := observe.MemoryPolicy(spec.MemoryPolicy)
			for _, o := range obs {
				f := memorypolicy.Evaluate(policy, o)
				add(o.Node, componentMemoryPolicy, memoryState(f.Verdict), memoryMessage(f))
			}
		}
	}
	if *spec.RuntimeProfile.Enabled && r.RuntimeObserver != nil {
		if obs, err := r.RuntimeObserver.ObserveRuntime(ctx, spec.NodeSelector); err == nil {
			for _, o := range obs {
				s := runtimeprofile.Summarize(runtimeprofile.Evaluate(spec.RuntimeProfile.Backends, o))
				msg := s.Message
				if s.Detail != "" {
					msg += ": " + s.Detail
				}
				add(o.Node, componentRuntimeProfile, runtimeState(s.Result), msg)
			}
		}
	}
	if *spec.HealthChecks.Enabled && r.HealthObserver != nil {
		if obs, err := r.HealthObserver.ObserveHealth(ctx, spec.NodeSelector); err == nil {
			for _, o := range obs {
				s := healthchecks.Summarize(
					healthchecks.Evaluate(spec.HealthChecks.Probes, healthchecks.DefaultThresholds, o))
				msg := s.Message
				if s.Detail != "" {
					msg += ": " + s.Detail
				}
				add(o.Node, componentHealthChecks, healthState(s.Result), msg)
			}
		}
	}
	if *spec.StackPolicy.Enabled && r.StackObserver != nil {
		if obs, err := r.StackObserver.ObserveStack(ctx, spec.NodeSelector); err == nil {
			policy := observe.StackPolicy(spec.StackPolicy)
			for _, o := range obs {
				stack := stackpolicy.EvaluateStack(policy, o)
				firmware := stackpolicy.EvaluateFirmware(policy, o)
				state, msg := stackNodeState(stack, firmware)
				add(o.Node, componentStackPolicy, state, msg)
			}
		}
	}
	if r.BootHoldObserver != nil {
		if results, err := r.BootHoldObserver.ObserveBootHold(ctx, spec.NodeSelector); err == nil {
			for _, res := range results {
				add(res.Node, componentBootHold, bootHoldState(res.Verdict), res.Message)
			}
		}
	}

	if timer, ok := r.observationTimer(); ok {
		if times, err := timer.ObservedAt(ctx, spec.NodeSelector); err == nil {
			for name, at := range times {
				if n, ok := byNode[name]; ok {
					t := at
					n.ObservedAt = &t
				}
			}
		}
	}

	out := make([]halov1alpha1.NodeStatus, 0, len(byNode))
	for _, n := range byNode {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := nodeRank(out[i]), nodeRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	var omitted int32
	if len(out) > halov1alpha1.MaxNodeStatuses {
		omitted = int32(len(out) - halov1alpha1.MaxNodeStatuses)
		out = out[:halov1alpha1.MaxNodeStatuses]
	}
	return out, omitted
}

// observationTimer finds the observer that can say when nodes were read. The
// shipped Observer serves every interface, so whichever field holds it works.
func (r *HaloDeviceConfigReconciler) observationTimer() (ObservationTimer, bool) {
	for _, o := range []any{r.MemoryObserver, r.RuntimeObserver, r.HealthObserver, r.StackObserver, r.BootHoldObserver} {
		if t, ok := o.(ObservationTimer); ok && o != nil {
			return t, true
		}
	}
	return nil, false
}

// nodeRank puts a node with any Degraded component first, then one with any
// component not Ready, then fully Ready nodes: the bound on the list must
// never cut the nodes a reader is looking for.
func nodeRank(n halov1alpha1.NodeStatus) int {
	rank := 2
	for _, c := range n.Components {
		switch c.State {
		case halov1alpha1.ComponentDegraded:
			return 0
		case halov1alpha1.ComponentReady, halov1alpha1.ComponentDisabled:
		default:
			rank = 1
		}
	}
	return rank
}

func memoryState(v memorypolicy.Verdict) halov1alpha1.ComponentState {
	switch v {
	case memorypolicy.VerdictCompliant:
		return halov1alpha1.ComponentReady
	case memorypolicy.VerdictDrifted:
		return halov1alpha1.ComponentDegraded
	default:
		return halov1alpha1.ComponentNotObserved
	}
}

func runtimeState(r runtimeprofile.Result) halov1alpha1.ComponentState {
	switch r {
	case runtimeprofile.ResultAvailable:
		return halov1alpha1.ComponentReady
	case runtimeprofile.ResultUnavailable:
		return halov1alpha1.ComponentDegraded
	default:
		return halov1alpha1.ComponentNotObserved
	}
}

func healthState(r healthchecks.Result) halov1alpha1.ComponentState {
	switch r {
	case healthchecks.ResultHealthy:
		return halov1alpha1.ComponentReady
	case healthchecks.ResultUnhealthy:
		return halov1alpha1.ComponentDegraded
	default:
		return halov1alpha1.ComponentNotObserved
	}
}

func bootHoldState(v boothold.Verdict) halov1alpha1.ComponentState {
	switch v {
	case boothold.Cleared:
		return halov1alpha1.ComponentReady
	case boothold.Held, boothold.RebootPending:
		return halov1alpha1.ComponentDegraded
	default:
		return halov1alpha1.ComponentNotObserved
	}
}

// stackNodeState is the worse of a node's two stack findings, with every
// problem or unread check named.
func stackNodeState(stack, firmware stackpolicy.Finding) (halov1alpha1.ComponentState, string) {
	var problems, unread []string
	for _, f := range []stackpolicy.Finding{stack, firmware} {
		problems = append(problems, f.Problems...)
		unread = append(unread, f.Unread...)
	}
	switch {
	case len(problems) > 0:
		return halov1alpha1.ComponentDegraded, strings.Join(problems, "; ")
	case len(unread) > 0:
		return halov1alpha1.ComponentNotObserved, strings.Join(unread, "; ")
	default:
		return halov1alpha1.ComponentReady, "matches the declared stack and GPU firmware"
	}
}

func memoryMessage(f memorypolicy.Finding) string {
	switch f.Verdict {
	case memorypolicy.VerdictDrifted:
		return f.Describe()
	case memorypolicy.VerdictCompliant:
		return "matches the declared memory policy"
	default:
		if len(f.Unobserved) == 0 {
			return "not observed"
		}
		return "not observed: " + strings.Join(f.Unobserved, ", ")
	}
}
