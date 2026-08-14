// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package memorypolicy

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is what a node's memory configuration was found to be.
//
// There are three of these on purpose. A node whose configuration could not be
// read is NOT compliant and NOT drifted — it is unknown, and saying anything
// else would be inventing an observation. Only VerdictCompliant asserts that
// the node was actually looked at and actually matched.
type Verdict string

const (
	// VerdictCompliant means every checked parameter was observed and matched.
	VerdictCompliant Verdict = "Compliant"
	// VerdictDrifted means at least one parameter was observed and did not match.
	VerdictDrifted Verdict = "Drifted"
	// VerdictNotObserved means nothing contradicted the policy, but at least
	// one parameter could not be read, so compliance is not established.
	VerdictNotObserved Verdict = "NotObserved"
)

// Observation is what was actually read from one node.
//
// Each field is separately optional because "the probe did not run" and "the
// probe ran and found nothing" are different facts. A nil map is the former; an
// empty map is the latter, and the latter is grounds for reporting drift.
type Observation struct {
	// Node is the node this observation came from.
	Node string
	// KernelParams is the parsed kernel command line, or nil if it could not
	// be read.
	KernelParams map[string]string
	// EffectiveGPUMemoryGiB is the GTT-visible GPU memory, or nil if it could
	// not be read.
	EffectiveGPUMemoryGiB *int32
}

// Policy is the declared expectation, lifted out of the CRD so this package
// stays free of API types and can be tested on its own.
type Policy struct {
	// ExpectedKernelArgs maps a kernel parameter to the value it must have.
	ExpectedKernelArgs map[string]string
	// MinEffectiveGPUMemoryGiB is the floor for GTT-visible GPU memory. Zero
	// means the floor is not checked.
	MinEffectiveGPUMemoryGiB int32
}

// Drift is one parameter that was observed and did not match the policy.
type Drift struct {
	// Parameter is the kernel parameter, or the pseudo-parameter
	// "effectiveGPUMemoryGiB" for the memory floor.
	Parameter string
	// Expected is what the policy declared.
	Expected string
	// Actual is what the node reported. Empty means the parameter was absent
	// from a command line that was itself read successfully.
	Actual string
	// Absent distinguishes "set to the wrong value" from "not set at all".
	Absent bool
}

// Finding is the result of checking one node against one policy.
type Finding struct {
	Node string
	// Verdict summarises Drifts and Unobserved below.
	Verdict Verdict
	// Drifts are parameters observed to disagree with the policy.
	Drifts []Drift
	// Unobserved names what could not be read. Its presence is why a Finding
	// may be NotObserved rather than Compliant.
	Unobserved []string
}

// Evaluate checks one observation against one policy.
//
// The ordering rule is deliberate and is the whole point of the function:
// observed disagreement outranks missing information, and missing information
// outranks a claim of compliance. A node is only ever called Compliant when
// every parameter the policy names was actually read and actually matched.
func Evaluate(policy Policy, obs Observation) Finding {
	finding := Finding{Node: obs.Node}

	for _, name := range sortedKeys(policy.ExpectedKernelArgs) {
		expected := policy.ExpectedKernelArgs[name]
		if obs.KernelParams == nil {
			// The command line itself could not be read, so nothing can be
			// said about any parameter on it.
			finding.Unobserved = append(finding.Unobserved,
				fmt.Sprintf("kernel parameter %q", name))
			continue
		}
		actual, present := obs.KernelParams[name]
		switch {
		case !present:
			finding.Drifts = append(finding.Drifts, Drift{
				Parameter: name, Expected: expected, Absent: true,
			})
		case actual != expected:
			finding.Drifts = append(finding.Drifts, Drift{
				Parameter: name, Expected: expected, Actual: actual,
			})
		}
	}

	if policy.MinEffectiveGPUMemoryGiB > 0 {
		switch {
		case obs.EffectiveGPUMemoryGiB == nil:
			finding.Unobserved = append(finding.Unobserved, "effective GPU memory")
		case *obs.EffectiveGPUMemoryGiB < policy.MinEffectiveGPUMemoryGiB:
			finding.Drifts = append(finding.Drifts, Drift{
				Parameter: "effectiveGPUMemoryGiB",
				Expected:  fmt.Sprintf(">=%d", policy.MinEffectiveGPUMemoryGiB),
				Actual:    fmt.Sprintf("%d", *obs.EffectiveGPUMemoryGiB),
			})
		}
	}

	switch {
	case len(finding.Drifts) > 0:
		finding.Verdict = VerdictDrifted
	case len(finding.Unobserved) > 0:
		finding.Verdict = VerdictNotObserved
	default:
		finding.Verdict = VerdictCompliant
	}
	return finding
}

// Summary aggregates findings across the nodes a config targets.
type Summary struct {
	// Verdict is the worst verdict across all nodes: Drifted if any node
	// drifted, otherwise NotObserved if any node was not fully observed or if
	// there were no nodes at all, otherwise Compliant.
	Verdict Verdict
	// Message is a human-readable one-liner suitable for a status condition.
	Message string
	// Drifted, NotObserved and Compliant count nodes by verdict.
	Drifted     int
	NotObserved int
	Compliant   int
}

// Summarize reduces per-node findings to the state of the component.
//
// An empty set of findings is NotObserved, never Compliant. Reporting "no
// drift" for a policy that has not been checked against a single node would be
// the exact failure this operator exists to avoid.
func Summarize(findings []Finding) Summary {
	summary := Summary{}
	for _, f := range findings {
		switch f.Verdict {
		case VerdictDrifted:
			summary.Drifted++
		case VerdictNotObserved:
			summary.NotObserved++
		case VerdictCompliant:
			summary.Compliant++
		}
	}

	switch {
	case len(findings) == 0:
		summary.Verdict = VerdictNotObserved
		summary.Message = "no node has been observed yet"
	case summary.Drifted > 0:
		summary.Verdict = VerdictDrifted
		summary.Message = fmt.Sprintf("%s drifted from the declared memory policy",
			nodeCount(summary.Drifted))
	case summary.NotObserved > 0:
		summary.Verdict = VerdictNotObserved
		summary.Message = fmt.Sprintf("%s not fully observed; compliance is not established",
			nodeCount(summary.NotObserved))
	default:
		summary.Verdict = VerdictCompliant
		summary.Message = fmt.Sprintf("%s match the declared memory policy",
			nodeCount(summary.Compliant))
	}
	return summary
}

// Describe renders a finding's drifts as a stable, readable string.
func (f Finding) Describe() string {
	if len(f.Drifts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f.Drifts))
	for _, d := range f.Drifts {
		if d.Absent {
			parts = append(parts, fmt.Sprintf("%s expected %q but was not set", d.Parameter, d.Expected))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s expected %q but was %q", d.Parameter, d.Expected, d.Actual))
	}
	return strings.Join(parts, "; ")
}

func nodeCount(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
