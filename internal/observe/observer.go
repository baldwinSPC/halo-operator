// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package observe turns published HaloNodeObservations into the observations
// the decision packages consume. It is the controller-side half of the node
// agent: the agent writes what it read, this package decides which of those
// writings still count as knowledge — and nothing more. All policy about what
// readings MEAN stays in internal/memorypolicy and internal/healthchecks.
//
// The join is deliberately node-first: the set of nodes the policy targets
// comes from listing Nodes by selector, and observations are matched to that
// set by name. A node the agent never reached, or whose observation has gone
// stale, is still in the set — as an observation with every reading nil — so
// it degrades the summary to "not fully observed" instead of silently
// vanishing from the compliance calculus. A fleet is not more compliant
// because some of it stopped reporting.
package observe

import (
	"context"
	"math"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
)

// NodeFeatureLabelAMDGPU is the label Node Feature Discovery applies to nodes
// with an AMD GPU, via the rule AMD's GPU Operator installs. An empty
// nodeSelector on a HaloDeviceConfig means "every node carrying this label",
// per that field's documentation; this operator consumes the labelling rather
// than performing its own.
const NodeFeatureLabelAMDGPU = "feature.node.kubernetes.io/amd-gpu"

// DefaultMaxAge is how old an observation may be and still count. The agent
// publishes every minute by default, so five minutes means five consecutive
// missed publishes before the controller stops believing the last one.
const DefaultMaxAge = 5 * time.Minute

// Observer reads Nodes and HaloNodeObservations from the cluster and produces
// per-node observations for the reconciler. It implements the reconciler's
// NodeObserver and HealthObserver interfaces.
type Observer struct {
	// Reader is the cluster to read from, usually the manager's cached client.
	Reader client.Reader
	// MaxAge is the freshness window; observations older than this are
	// treated as no observation at all. Zero means DefaultMaxAge.
	MaxAge time.Duration
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// Observe returns one memory-policy observation per targeted node.
func (o *Observer) Observe(ctx context.Context, selector map[string]string) ([]memorypolicy.Observation, error) {
	nodes, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]memorypolicy.Observation, 0, len(nodes))
	for _, node := range nodes {
		// A node with no fresh observation gets a nil spec, and so an
		// observation with every reading nil — it stays in the calculus.
		out = append(out, MemoryObservation(node, readings[node]))
	}
	return out, nil
}

// MemoryObservation translates one node's published spec into what the memory
// policy judges. A nil spec — no fresh observation — yields an observation
// with every reading nil, which the policy treats as unobserved, never as
// compliant.
//
// Exported because it is shared with the standalone `halo` binary, which has
// no cluster and so no Observer, but must judge a node exactly as the
// controller would. One translation, two callers.
func MemoryObservation(node string, spec *halov1alpha1.HaloNodeObservationSpec) memorypolicy.Observation {
	obs := memorypolicy.Observation{Node: node}
	if spec == nil {
		return obs
	}
	if spec.KernelCmdline != nil {
		// Parsing happens here, not on the node, so the parse rules live in
		// one versioned place and a raw cmdline is always inspectable on the
		// published observation. Note an empty command line parses to an
		// empty map: that is "read fine, nothing set", which is drift if
		// parameters were expected — nil, "could not read", stays reserved
		// for the nil field.
		obs.KernelParams = memorypolicy.ParseKernelCmdline(*spec.KernelCmdline)
	}
	// Passed through as read. Unlike the command line there is nothing to
	// parse — each entry is already one parameter and one value — and nil
	// stays nil so "no module directory was readable" does not arrive as
	// "read fine, nothing set".
	obs.ModuleParams = spec.ModuleParams
	obs.EffectiveGPUMemoryGiB = gibFloor(spec.GTTTotalBytes)
	return obs
}

// ObserveHealth returns one health observation per targeted node.
func (o *Observer) ObserveHealth(ctx context.Context, selector map[string]string) ([]healthchecks.Observation, error) {
	nodes, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]healthchecks.Observation, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, HealthObservation(node, readings[node]))
	}
	return out, nil
}

// HealthObservation translates one node's published spec into what the health
// probes judge. A nil spec yields every reading nil, which each probe reports
// as not observed — never as a clean bill of health. Shared with the
// standalone binary for the same reason as MemoryObservation.
func HealthObservation(node string, spec *halov1alpha1.HaloNodeObservationSpec) healthchecks.Observation {
	obs := healthchecks.Observation{Node: node}
	if spec == nil {
		return obs
	}
	obs.GPUBusyPercent = copyInt32(spec.GPUBusyPercent)
	obs.SCLKMHz = copyInt32(spec.SCLKMHz)
	obs.RatedSCLKMHz = copyInt32(spec.RatedSCLKMHz)
	obs.PermissionFaults = copyInt32(spec.PermissionFaults)
	obs.QueueCreateFailures = copyInt32(spec.QueueCreateFailures)
	return obs
}

// MemoryPolicy lifts the CRD stanza into the decision package's own type, so
// that package never imports the API. Shared by the controller and the
// standalone binary so a policy file means the same thing to both.
func MemoryPolicy(spec halov1alpha1.MemoryPolicySpec) memorypolicy.Policy {
	return memorypolicy.Policy{
		ExpectedKernelArgs:       spec.ExpectedKernelArgs,
		MinEffectiveGPUMemoryGiB: spec.MinEffectiveGPUMemoryGiB,
	}
}

// join lists the targeted nodes and the fresh observation, if any, for each.
// Node names come back sorted so findings and their summaries are stable.
func (o *Observer) join(ctx context.Context, selector map[string]string) ([]string, map[string]*halov1alpha1.HaloNodeObservationSpec, error) {
	if len(selector) == 0 {
		selector = map[string]string{NodeFeatureLabelAMDGPU: "true"}
	}

	var nodeList corev1.NodeList
	if err := o.Reader.List(ctx, &nodeList, client.MatchingLabelsSelector{
		Selector: labels.SelectorFromSet(selector),
	}); err != nil {
		return nil, nil, err
	}
	nodes := make([]string, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		nodes = append(nodes, nodeList.Items[i].Name)
	}
	sort.Strings(nodes)

	var obsList halov1alpha1.HaloNodeObservationList
	if err := o.Reader.List(ctx, &obsList); err != nil {
		return nil, nil, err
	}

	maxAge := o.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}

	readings := make(map[string]*halov1alpha1.HaloNodeObservationSpec, len(obsList.Items))
	for i := range obsList.Items {
		obs := &obsList.Items[i]
		// An observation with a future timestamp is clock skew, not a
		// problem; only age beyond the window discounts it. A stale
		// observation is dropped entirely rather than partially trusted:
		// its node stays in the calculus as unread.
		if now().Sub(obs.Spec.ObservedAt.Time) > maxAge {
			continue
		}
		readings[obs.Name] = &obs.Spec
	}
	return nodes, readings, nil
}

// gibFloor converts bytes to whole GiB, rounding down: a floor policy must not
// be satisfied by rounding 95.9 GiB up to 96. Nil stays nil, and a value too
// large to state exactly is treated as unobserved rather than misstated.
func gibFloor(bytes *int64) *int32 {
	if bytes == nil {
		return nil
	}
	gib := *bytes >> 30
	if gib > math.MaxInt32 {
		return nil
	}
	v := int32(gib)
	return &v
}

func copyInt32(p *int32) *int32 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
