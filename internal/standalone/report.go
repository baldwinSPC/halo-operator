// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package standalone runs the AMD Halo Operator's judgement on a single node,
// in-process, with no Kubernetes at all.
//
// It is not a second implementation. Every decision goes through the same
// functions the cluster path uses: the reading is lifted by
// nodeagent.SpecFromReading, translated by observe.MemoryObservation,
// observe.RuntimeObservation and observe.HealthObservation, and judged by the
// memorypolicy, runtimeprofile and healthchecks packages. The only thing this
// package removes is the transport — the
// HaloNodeObservation round trip through an API server — so a node judged
// here and a node judged by the controller get the same answer for the same
// reading, by construction rather than by agreement.
//
// It keeps the operator's three-valued discipline all the way to the process
// exit code. A naive CLI returns 0 when it cannot tell; this one returns a
// distinct code for "not observed", because a script that treats unknown as
// healthy is the exact failure the operator exists to refuse.
package standalone

import (
	"fmt"
	"os"
	"sort"
	"time"

	"sigs.k8s.io/yaml"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/observe"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
)

// Exit codes. The first three are the operator's answer about the node; the
// last two mean the tool itself went wrong. They are kept disjoint so that a
// script can never mistake "halo broke" for "the node drifted" — which rules
// out 2 for usage errors, the value Go's flag package would pick by default.
const (
	// ExitCompliant: every enabled, implemented component was observed and
	// matched.
	ExitCompliant = 0
	// ExitDrifted: something was observed to disagree with the policy.
	ExitDrifted = 1
	// ExitNotObserved: nothing disagreed, but compliance could not be
	// established — at least one thing could not be read.
	ExitNotObserved = 2
	// ExitUsage: bad invocation or unreadable policy file (sysexits EX_USAGE).
	ExitUsage = 64
	// ExitSoftware: an internal failure (sysexits EX_SOFTWARE).
	ExitSoftware = 70
)

// Outcome is the whole-node answer, one of three.
type Outcome string

const (
	OutcomeCompliant   Outcome = "compliant"
	OutcomeDrifted     Outcome = "drifted"
	OutcomeNotObserved Outcome = "not-observed"
)

// Line results for one checked item.
const (
	LineOK         = "ok"
	LineDrift      = "drift"
	LineUnobserved = "unobserved"
)

// ParamLine explains one memory-policy check: what was expected, what was
// found, and where it was found.
type ParamLine struct {
	Name     string              `json:"name"`
	Expected string              `json:"expected"`
	Actual   string              `json:"actual,omitempty"`
	Source   memorypolicy.Source `json:"source"`
	Result   string              `json:"result"`
}

// ProbeLine is one health probe's outcome. It mirrors
// healthchecks.ProbeResult with serialization tags, so presentation concerns
// stay out of the decision package.
type ProbeLine struct {
	Probe   string `json:"probe"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// BackendLine is one compute backend's outcome. It mirrors
// runtimeprofile.BackendResult with serialization tags, as ProbeLine does for
// the health probes.
type BackendLine struct {
	Backend string `json:"backend"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

// ComponentReport is one component's state, in the same vocabulary the
// HaloDeviceConfig status uses.
type ComponentReport struct {
	Name     string                      `json:"name"`
	State    halov1alpha1.ComponentState `json:"state"`
	Message  string                      `json:"message,omitempty"`
	Params   []ParamLine                 `json:"params,omitempty"`
	Probes   []ProbeLine                 `json:"probes,omitempty"`
	Backends []BackendLine               `json:"backends,omitempty"`
	Images   []ImageLine                 `json:"images,omitempty"`
	counted  bool                        // whether it contributes to the Outcome
}

// Report is the evaluation of one node against one policy.
type Report struct {
	Node       string            `json:"node"`
	ObservedAt time.Time         `json:"observedAt"`
	Outcome    Outcome           `json:"outcome"`
	ExitCode   int               `json:"exitCode"`
	Components []ComponentReport `json:"components"`
	// Warnings are about the policy as applied to a single node.
	Warnings []string `json:"warnings,omitempty"`
	// Messages are the collector's own explanations of every reading it could
	// not take — why something is "not observed".
	Messages []string `json:"readingMessages,omitempty"`
}

// LoadPolicy reads a HaloDeviceConfig manifest — the same file a cluster user
// would `kubectl apply`. One policy format for both modes is deliberate: a
// fleet's declared configuration should not need translating to be checked
// on a node that is not in the fleet.
func LoadPolicy(path string) (*halov1alpha1.HaloDeviceConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg halov1alpha1.HaloDeviceConfig
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not a valid HaloDeviceConfig: %w", path, err)
	}
	if cfg.Kind != "" && cfg.Kind != "HaloDeviceConfig" {
		return nil, fmt.Errorf("%s has kind %q, want HaloDeviceConfig", path, cfg.Kind)
	}
	return &cfg, nil
}

// Evaluate judges one node's reading against a policy.
func Evaluate(node string, spec halov1alpha1.HaloNodeObservationSpec, cfg halov1alpha1.HaloDeviceConfig) Report {
	resolved := cfg.Spec.WithDefaults()
	r := Report{Node: node, ObservedAt: spec.ObservedAt.Time, Messages: spec.Messages}

	if len(cfg.Spec.NodeSelector) > 0 {
		r.Warnings = append(r.Warnings,
			"spec.nodeSelector is ignored in standalone mode: there is exactly one node, and it is this one")
	}

	r.Components = []ComponentReport{
		evaluateMemory(node, &spec, resolved),
		evaluateRuntime(node, &spec, resolved),
		evaluateHealth(node, &spec, resolved),
	}
	r.Outcome, r.ExitCode = outcome(r.Components)
	return r
}

// AddComponent adds a component judged outside Evaluate and recomputes the
// outcome with it.
//
// The boot-image check is the one that needs this, and it stays outside
// Evaluate on purpose. Evaluate is the judgement the controller shares, and
// the controller's input is the agent's unprivileged reading; the boot images
// live on the EFI system partition, which only root can read. Adding them
// inside Evaluate would make the two modes disagree about the same node by
// construction — which is what TestStandaloneAgreesWithTheController exists
// to prevent.
func (r *Report) AddComponent(c ComponentReport) {
	r.Components = append(r.Components, c)
	r.Outcome, r.ExitCode = outcome(r.Components)
}

func evaluateMemory(node string, spec *halov1alpha1.HaloNodeObservationSpec, s halov1alpha1.HaloDeviceConfigSpec) ComponentReport {
	c := ComponentReport{Name: "memory-policy"}
	if !*s.MemoryPolicy.Enabled {
		c.State = halov1alpha1.ComponentDisabled
		c.Message = "disabled by the policy"
		return c
	}
	c.counted = true

	obs := observe.MemoryObservation(node, spec)
	policy := observe.MemoryPolicy(s.MemoryPolicy)
	finding := memorypolicy.Evaluate(policy, obs)

	names := make([]string, 0, len(policy.ExpectedKernelArgs))
	for name := range policy.ExpectedKernelArgs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		expected := policy.ExpectedKernelArgs[name]
		actual, source := memorypolicy.ResolveParam(obs, name)
		line := ParamLine{Name: name, Expected: expected, Actual: actual, Source: source}
		switch {
		case source == memorypolicy.SourceUnreadable:
			line.Result = LineUnobserved
		case source == memorypolicy.SourceUnset, actual != expected:
			line.Result = LineDrift
		default:
			line.Result = LineOK
		}
		c.Params = append(c.Params, line)
	}

	if policy.MinEffectiveGPUMemoryGiB > 0 {
		line := ParamLine{
			Name:     "effectiveGPUMemoryGiB",
			Expected: fmt.Sprintf(">=%d", policy.MinEffectiveGPUMemoryGiB),
			Source:   "amdgpu mem_info_gtt_total",
		}
		switch {
		case obs.EffectiveGPUMemoryGiB == nil:
			line.Result = LineUnobserved
			line.Source = memorypolicy.SourceUnreadable
		case *obs.EffectiveGPUMemoryGiB < policy.MinEffectiveGPUMemoryGiB:
			line.Actual = fmt.Sprintf("%d", *obs.EffectiveGPUMemoryGiB)
			line.Result = LineDrift
		default:
			line.Actual = fmt.Sprintf("%d", *obs.EffectiveGPUMemoryGiB)
			line.Result = LineOK
		}
		c.Params = append(c.Params, line)
	}

	// The state comes from the decision package's verdict, never from the
	// lines above. The lines explain; the verdict decides.
	switch finding.Verdict {
	case memorypolicy.VerdictCompliant:
		c.State = halov1alpha1.ComponentReady
		c.Message = "every declared parameter was observed and matched"
	case memorypolicy.VerdictDrifted:
		c.State = halov1alpha1.ComponentDegraded
		c.Message = fmt.Sprintf("%d of %d checks drifted", countResult(c.Params, LineDrift), len(c.Params))
	default:
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("%d of %d checks could not be observed; compliance is not established",
			countResult(c.Params, LineUnobserved), len(c.Params))
	}
	if len(c.Params) == 0 {
		c.Message = "the policy declares no memory expectations, so there was nothing to check"
	}
	return c
}

func evaluateHealth(node string, spec *halov1alpha1.HaloNodeObservationSpec, s halov1alpha1.HaloDeviceConfigSpec) ComponentReport {
	c := ComponentReport{Name: "health-checks"}
	if !*s.HealthChecks.Enabled {
		c.State = halov1alpha1.ComponentDisabled
		c.Message = "disabled by the policy"
		return c
	}
	c.counted = true

	obs := observe.HealthObservation(node, spec)
	results := healthchecks.Evaluate(s.HealthChecks.Probes, healthchecks.DefaultThresholds, obs)
	for _, pr := range results {
		c.Probes = append(c.Probes, ProbeLine{Probe: pr.Probe, Result: string(pr.Result), Message: pr.Message})
	}
	summary := healthchecks.Summarize(results)
	c.Message = summary.Message
	switch summary.Result {
	case healthchecks.ResultHealthy:
		c.State = halov1alpha1.ComponentReady
	case healthchecks.ResultUnhealthy:
		c.State = halov1alpha1.ComponentDegraded
	default:
		c.State = halov1alpha1.ComponentNotObserved
	}
	return c
}

func evaluateRuntime(node string, spec *halov1alpha1.HaloNodeObservationSpec, s halov1alpha1.HaloDeviceConfigSpec) ComponentReport {
	c := ComponentReport{Name: "runtime-profile"}
	if !*s.RuntimeProfile.Enabled {
		c.State = halov1alpha1.ComponentDisabled
		c.Message = "disabled by the policy"
		return c
	}
	c.counted = true

	obs := observe.RuntimeObservation(node, spec)
	results := runtimeprofile.Evaluate(s.RuntimeProfile.Backends, obs)
	for _, br := range results {
		c.Backends = append(c.Backends, BackendLine{Backend: br.Backend, Result: string(br.Result), Message: br.Message})
	}
	summary := runtimeprofile.Summarize(results)
	c.Message = summary.Message
	switch summary.Result {
	case runtimeprofile.ResultAvailable:
		c.State = halov1alpha1.ComponentReady
	case runtimeprofile.ResultUnavailable:
		c.State = halov1alpha1.ComponentDegraded
	default:
		c.State = halov1alpha1.ComponentNotObserved
	}
	return c
}

// outcome reduces component states to the node's answer, with the ordering
// the operator uses everywhere: observed disagreement outranks missing
// information, and missing information outranks any claim of compliance.
// A node where nothing was checked is not observed, never compliant.
func outcome(components []ComponentReport) (Outcome, int) {
	var ready, degraded, unobserved int
	for _, c := range components {
		if !c.counted {
			continue
		}
		switch c.State {
		case halov1alpha1.ComponentReady:
			ready++
		case halov1alpha1.ComponentDegraded:
			degraded++
		default:
			unobserved++
		}
	}
	switch {
	case degraded > 0:
		return OutcomeDrifted, ExitDrifted
	case unobserved > 0, ready == 0:
		return OutcomeNotObserved, ExitNotObserved
	default:
		return OutcomeCompliant, ExitCompliant
	}
}

func countResult(lines []ParamLine, result string) int {
	n := 0
	for _, l := range lines {
		if l.Result == result {
			n++
		}
	}
	return n
}
