// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package observe turns published HaloNodeObservations into the observations
// the decision packages consume. It is the controller-side half of the node
// agent: the agent writes what it read, this package decides which of those
// writings still count as knowledge — and nothing more. All policy about what
// readings MEAN stays in internal/memorypolicy, internal/healthchecks and
// internal/runtimeprofile.
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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
	"github.com/baldwinSPC/halo-operator/internal/stackpolicy"
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
// NodeObserver, HealthObserver, RuntimeObserver, BootHoldObserver and
// StackObserver interfaces.
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
	obs.ModuleParams = aliasDKMSTTM(spec.ModuleParams, spec.TTMModule)
	obs.EffectiveGPUMemoryGiB = gibFloor(spec.GTTTotalBytes)
	return obs
}

// aliasDKMSTTM makes a policy that names ttm.<param> read the TTM module that
// is actually loaded. amdgpu-dkms ships its own TTM as amdttm (or amd_ttm), and
// on such a host GTT is sized by amdttm.pages_limit; a policy written against
// the inbox name would otherwise read nothing and report not observed forever.
// The out-of-tree driver itself is reported by the stack policy, not hidden by
// this: aliasing only lets the memory policy read the value in effect.
func aliasDKMSTTM(params map[string]string, ttmModule *string) map[string]string {
	if params == nil || ttmModule == nil || *ttmModule == "" || *ttmModule == "ttm" {
		return params
	}
	prefix := *ttmModule + "."
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	for k, v := range params {
		if name, ok := strings.CutPrefix(k, prefix); ok {
			if _, inbox := out["ttm."+name]; !inbox {
				out["ttm."+name] = v
			}
		}
	}
	return out
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

// ObserveRuntime returns one runtime-profile observation per targeted node.
func (o *Observer) ObserveRuntime(ctx context.Context, selector map[string]string) ([]runtimeprofile.Observation, error) {
	nodes, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]runtimeprofile.Observation, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, RuntimeObservation(node, readings[node]))
	}
	return out, nil
}

// RuntimeObservation translates one node's published spec into what the
// runtime profile judges. A nil spec yields both readings nil, which reports
// every backend as not observed, never as available. Shared with the
// standalone binary for the same reason as MemoryObservation.
func RuntimeObservation(node string, spec *halov1alpha1.HaloNodeObservationSpec) runtimeprofile.Observation {
	obs := runtimeprofile.Observation{Node: node}
	if spec == nil {
		return obs
	}
	if spec.DRMRenderNode != nil {
		v := *spec.DRMRenderNode
		obs.RenderNode = &v
	}
	obs.GFXTargetVersion = copyInt32(spec.GFXTargetVersion)
	return obs
}

// ObserveBootHold returns one boot-hold verdict per targeted node.
func (o *Observer) ObserveBootHold(ctx context.Context, selector map[string]string) ([]boothold.NodeResult, error) {
	nodes, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]boothold.NodeResult, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, boothold.NodeResult{Node: node, Result: boothold.Decide(BootHoldObservation(node, readings[node]))})
	}
	return out, nil
}

// etcKernelCmdlinePath names /etc/kernel/cmdline in boot-hold findings. The
// reading itself carries no path, and the host path is the one a reader acts on.
const etcKernelCmdlinePath = "/etc/kernel/cmdline"

// BootHoldObservation translates one node's published spec into what the
// boot-hold decision judges: whether the token is on the running command line,
// and in which persisted files. A nil spec, or one with no boot-hold reading,
// yields an observation the decision reports as not observed, never as
// cleared. Shared with the standalone binary for the same reason as
// MemoryObservation.
func BootHoldObservation(node string, spec *halov1alpha1.HaloNodeObservationSpec) boothold.Observation {
	obs := boothold.Observation{Node: node}
	if spec == nil {
		return obs
	}
	if spec.KernelCmdline != nil {
		has := hasToken(*spec.KernelCmdline, boothold.HoldToken)
		obs.RunningKernelHasToken = &has
	}
	bh := spec.BootHold
	if bh == nil {
		return obs
	}
	if bh.EtcKernelCmdline != nil {
		obs.Files = append(obs.Files, boothold.FileReading{
			Path: etcKernelCmdlinePath, HasToken: hasToken(*bh.EtcKernelCmdline, boothold.HoldToken)})
	}
	for _, e := range bh.LoaderEntries {
		obs.Files = append(obs.Files, boothold.FileReading{Path: e.Path, HasToken: hasToken(e.Options, boothold.HoldToken)})
	}
	obs.Unreadable = append([]string(nil), bh.Unreadable...)
	if bh.LoaderEntriesListed != nil {
		obs.LoaderEntriesListed = *bh.LoaderEntriesListed
	}
	if bh.LoaderEntriesFound != nil {
		obs.LoaderEntriesFound = int(*bh.LoaderEntriesFound)
	}
	return obs
}

// hasToken reports whether a kernel command line carries the token as a whole
// argument. "splash" is a bare flag, so a substring match would also find it
// inside a value such as "foo=splashscreen".
func hasToken(cmdline, token string) bool {
	for _, field := range strings.Fields(cmdline) {
		if field == token {
			return true
		}
	}
	return false
}

// ObserveStack returns one stack-policy observation per targeted node.
func (o *Observer) ObserveStack(ctx context.Context, selector map[string]string) ([]stackpolicy.Observation, error) {
	nodes, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]stackpolicy.Observation, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, StackObservation(node, readings[node]))
	}
	return out, nil
}

// StackObservation translates one node's published spec into what the stack
// policy judges. The VBIOS joins the per-block firmware map as "vbios", the
// name a known-bad entry uses for it. A nil spec yields every reading nil.
// Shared with the standalone binary for the same reason as MemoryObservation.
func StackObservation(node string, spec *halov1alpha1.HaloNodeObservationSpec) stackpolicy.Observation {
	obs := stackpolicy.Observation{Node: node}
	if spec == nil {
		return obs
	}
	obs.KernelRelease = copyString(spec.KernelRelease)
	obs.AMDGPUTaint = copyString(spec.AMDGPUTaint)
	obs.TTMModule = copyString(spec.TTMModule)
	if spec.FirmwareVersions != nil || spec.VBIOSVersion != nil {
		obs.Firmware = map[string]string{}
		for k, v := range spec.FirmwareVersions {
			obs.Firmware[k] = v
		}
		if spec.VBIOSVersion != nil {
			obs.Firmware["vbios"] = *spec.VBIOSVersion
		}
	}
	return obs
}

// StackPolicy lifts the CRD stanza into the decision package's own type.
// The spec must already carry its defaults.
func StackPolicy(spec halov1alpha1.StackPolicySpec) stackpolicy.Policy {
	p := stackpolicy.Policy{
		RequireInboxDriver: spec.RequireInboxDriver != nil && *spec.RequireInboxDriver,
		MinKernelVersion:   spec.MinKernelVersion,
	}
	for _, b := range spec.KnownBadFirmware {
		p.KnownBadFirmware = append(p.KnownBadFirmware,
			stackpolicy.KnownBad{Component: b.Component, Version: b.Version, Reason: b.Reason})
	}
	return p
}

func copyString(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
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

// ObservedAt returns, for each targeted node with a fresh observation, when it
// was taken. A node without one is absent from the map.
func (o *Observer) ObservedAt(ctx context.Context, selector map[string]string) (map[string]metav1.Time, error) {
	_, readings, err := o.join(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make(map[string]metav1.Time, len(readings))
	for node, spec := range readings {
		out[node] = spec.ObservedAt
	}
	return out, nil
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
