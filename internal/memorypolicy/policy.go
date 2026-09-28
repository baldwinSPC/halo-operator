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
	// ModuleParams is the EFFECTIVE value of each readable kernel module
	// parameter, keyed exactly as the kernel command line would name it —
	// "ttm.pages_limit", "amdgpu.gttsize". It is nil when nothing could be
	// read, and a key is absent when that one parameter could not be read,
	// which on this hardware is routine: /sys/module/amdgpu/parameters/* is
	// mode 0400 and the agent is unprivileged.
	//
	// This exists because the command line is not the only way a parameter
	// gets set, and on the AMD Ryzen AI Developer Platform it is not the way
	// that is used: the platform's own ttm-config.service writes
	// /etc/modprobe.d/ttm.conf instead. Checking the command line alone would
	// report permanent drift on a node that is configured exactly right.
	ModuleParams map[string]string
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
		actual, source := resolveParam(obs, name)
		switch source {
		case sourceUnreadable:
			// Neither the command line nor — for a module parameter — the
			// module's effective value could be read, so nothing can be said
			// about this parameter.
			finding.Unobserved = append(finding.Unobserved,
				fmt.Sprintf("kernel parameter %q", name))
		case sourceUnset:
			finding.Drifts = append(finding.Drifts, Drift{
				Parameter: name, Expected: expected, Absent: true,
			})
		default:
			if actual != expected {
				finding.Drifts = append(finding.Drifts, Drift{
					Parameter: name, Expected: expected, Actual: actual,
				})
			}
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

// paramSource says where a parameter's value came from, which is what decides
// between drift and ignorance when the value is missing.
type paramSource int

const (
	// sourceUnreadable means no surface that could carry this parameter was
	// readable, so its value is unknown.
	sourceUnreadable paramSource = iota
	// sourceUnset means a surface WAS read and the parameter is genuinely not
	// set on it. That is an observation, and grounds for drift.
	sourceUnset
	// sourceCmdline and sourceModule mean a value was found.
	sourceCmdline
	sourceModule
)

// resolveParam finds the effective value of one expected parameter.
//
// The command line is consulted first because it is what was *requested* at
// boot and it names every kind of parameter. A module parameter — anything
// spelled "<module>.<name>" — additionally has a running value under
// /sys/module, and that running value is authoritative when the command line
// does not mention it: a parameter can be set by modprobe.d instead, which is
// exactly what the AMD Ryzen AI Developer Platform does for ttm.pages_limit.
// Checking only the command line would call such a node drifted forever.
//
// The asymmetry between the two "missing" cases is the whole point. For a
// module parameter, missing from the command line is NOT evidence of anything
// until the module's own value has also been checked, and that value is often
// unreadable (/sys/module/amdgpu/parameters/* is mode 0400 and the agent is
// unprivileged). Reporting drift there would accuse a node on the strength of
// a permission error.
func resolveParam(obs Observation, name string) (string, paramSource) {
	if obs.KernelParams != nil {
		if actual, present := obs.KernelParams[name]; present {
			return actual, sourceCmdline
		}
	}

	if isModuleParam(name) {
		if obs.ModuleParams != nil {
			if actual, present := obs.ModuleParams[name]; present {
				return actual, sourceModule
			}
		}
		// The module's running value is the authority for this parameter and
		// it could not be read, so its absence from the command line proves
		// nothing.
		return "", sourceUnreadable
	}

	if obs.KernelParams == nil {
		return "", sourceUnreadable
	}
	return "", sourceUnset
}

// isModuleParam reports whether a parameter name is one the kernel routes to a
// module, which is the dotted "<module>.<name>" spelling. Only such parameters
// have a running value under /sys/module/<module>/parameters/<name>; a bare
// name like "amd_iommu" is core-kernel and lives on the command line alone.
func isModuleParam(name string) bool {
	module, param, found := strings.Cut(name, ".")
	return found && module != "" && param != "" && !strings.Contains(param, ".")
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

// Source says where a resolved parameter's value came from.
type Source string

const (
	// SourceCmdline means the value was on the kernel command line.
	SourceCmdline Source = "kernel command line"
	// SourceModule means the value is the module's running parameter, read
	// from /sys/module — how a modprobe.d setting shows up.
	SourceModule Source = "module parameter"
	// SourceUnset means a surface that could carry the parameter was read
	// and it is genuinely not set there. An observation, and drift if the
	// parameter was expected.
	SourceUnset Source = "not set"
	// SourceUnreadable means no surface that could carry the parameter was
	// readable, so its value is unknown.
	SourceUnreadable Source = "not observed"
)

// ResolveParam reports the effective value of one parameter and where it came
// from, by exactly the rules Evaluate uses to judge it.
//
// It exists so that anything *displaying* a verdict shows the same value the
// verdict was reached on. Re-deriving the value for display — command line
// first, then module parameters — would be a second implementation of the
// resolution rules, free to drift from the first, and a report whose numbers
// disagree with its own conclusion is worse than no report.
func ResolveParam(obs Observation, name string) (string, Source) {
	value, src := resolveParam(obs, name)
	switch src {
	case sourceCmdline:
		return value, SourceCmdline
	case sourceModule:
		return value, SourceModule
	case sourceUnset:
		return "", SourceUnset
	default:
		return "", SourceUnreadable
	}
}

// IsModuleParam reports whether a parameter is a module parameter — spelled
// "<module>.<name>" — and so has a running value under /sys/module and can be
// set by modprobe.d, rather than living on the kernel command line alone.
// Exported so that anything planning a change classifies parameters by the
// same rule the verdict does.
func IsModuleParam(name string) bool { return isModuleParam(name) }
