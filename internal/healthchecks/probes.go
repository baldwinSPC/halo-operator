// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package healthchecks decides whether a node is exhibiting one of the
// documented Strix Halo failure modes. As with the memory policy, every
// decision here is a pure function of an observation: nothing reads sysfs,
// parses a kernel log, or needs a GPU, so all of it runs in CI on hardware
// that has never seen an APU.
//
// The failure modes are the ones the community and the ROCm tracker have
// documented, not invented ones:
//
//   - clock-under-load: the amdgpu idle-clock lock (ROCm/ROCm#5750). The node
//     reports healthy while running at idle clocks under full load, so
//     utilisation alone never reveals it; only clocks measured against
//     utilisation do.
//   - permission-fault: PERMISSION_FAULT storms (ROCm/ROCm#6186).
//   - queue-create: KFD queue creation failures (ROCm/ROCm#6118).
package healthchecks

import (
	"fmt"
	"sort"
)

// Probe names, matching the enum the CRD accepts.
const (
	ProbeClockUnderLoad  = "clock-under-load"
	ProbePermissionFault = "permission-fault"
	ProbeQueueCreate     = "queue-create"
)

// AllProbes is every probe this package implements, in a stable order.
var AllProbes = []string{ProbeClockUnderLoad, ProbePermissionFault, ProbeQueueCreate}

// Result is the outcome of one probe on one node.
//
// Three values, for the same reason as everywhere else in this operator: a
// probe whose inputs were not available has not found the node healthy. It has
// found nothing.
type Result string

const (
	// ResultHealthy means the probe ran on real inputs and found no fault.
	ResultHealthy Result = "Healthy"
	// ResultUnhealthy means the probe ran and found the fault it looks for.
	ResultUnhealthy Result = "Unhealthy"
	// ResultNotObserved means the probe could not run, because an input it
	// needs was not available.
	ResultNotObserved Result = "NotObserved"
)

// Observation is what was read from one node for the health probes. Every
// field is a pointer because a reading that was not taken must stay
// distinguishable from a reading of zero — on these probes that difference is
// the difference between "nothing to report" and "the fault is present".
type Observation struct {
	// Node is the node this observation came from.
	Node string

	// GPUBusyPercent is amdgpu's gpu_busy_percent, 0 to 100.
	GPUBusyPercent *int32
	// SCLKMHz is the current shader clock.
	SCLKMHz *int32
	// RatedSCLKMHz is the top of the pp_dpm_sclk ladder, the clock the part
	// is rated to reach.
	RatedSCLKMHz *int32

	// PermissionFaults counts PERMISSION_FAULT entries seen in the sampling
	// window.
	PermissionFaults *int32
	// QueueCreateFailures counts KFD queue creation failures seen in the
	// sampling window.
	QueueCreateFailures *int32
}

// Thresholds tune the probes. The defaults are deliberately conservative: this
// operator would rather say "not observed" than accuse a healthy node.
type Thresholds struct {
	// BusyPercentFloor is the utilisation above which the clock probe
	// considers the GPU to be genuinely under load.
	BusyPercentFloor int32
	// ClockFractionFloor is the fraction of the rated clock below which a
	// loaded GPU is considered to be stuck at idle clocks, expressed in
	// percent.
	ClockFractionFloor int32
}

// DefaultThresholds reflects the documented behaviour of this part.
//
// A gfx1151 under real load sits near the top of its ladder. The 30 percent
// floor is far below anything a working part does under load, so crossing it
// means the clock is genuinely stuck rather than merely varying — which is what
// ROCm#5750 describes.
var DefaultThresholds = Thresholds{
	BusyPercentFloor:   90,
	ClockFractionFloor: 30,
}

// ProbeResult is one probe's outcome, with the reasoning that produced it.
type ProbeResult struct {
	Probe   string
	Result  Result
	Message string
}

// Evaluate runs the requested probes against one observation.
//
// Unknown probe names produce a NotObserved result rather than being silently
// dropped, so a typo in a config surfaces instead of quietly reducing coverage.
func Evaluate(probes []string, thresholds Thresholds, obs Observation) []ProbeResult {
	if thresholds.BusyPercentFloor == 0 && thresholds.ClockFractionFloor == 0 {
		thresholds = DefaultThresholds
	}
	results := make([]ProbeResult, 0, len(probes))
	for _, probe := range probes {
		switch probe {
		case ProbeClockUnderLoad:
			results = append(results, clockUnderLoad(thresholds, obs))
		case ProbePermissionFault:
			results = append(results, countProbe(ProbePermissionFault, obs.PermissionFaults,
				"PERMISSION_FAULT entries"))
		case ProbeQueueCreate:
			results = append(results, countProbe(ProbeQueueCreate, obs.QueueCreateFailures,
				"queue creation failures"))
		default:
			results = append(results, ProbeResult{
				Probe:   probe,
				Result:  ResultNotObserved,
				Message: fmt.Sprintf("unknown probe %q; no such check is implemented", probe),
			})
		}
	}
	return results
}

// clockUnderLoad detects the amdgpu idle-clock lock.
//
// The fault is only meaningful while the GPU is actually busy: a low clock on
// an idle GPU is correct behaviour, not a fault. So an idle GPU reports
// healthy, and a busy GPU stuck near idle clocks reports unhealthy.
func clockUnderLoad(thresholds Thresholds, obs Observation) ProbeResult {
	result := ProbeResult{Probe: ProbeClockUnderLoad}

	if obs.GPUBusyPercent == nil || obs.SCLKMHz == nil || obs.RatedSCLKMHz == nil {
		result.Result = ResultNotObserved
		result.Message = "utilisation and clocks were not both readable, so the idle-clock lock cannot be ruled out"
		return result
	}
	if *obs.RatedSCLKMHz <= 0 {
		result.Result = ResultNotObserved
		result.Message = "the rated clock was reported as zero, so no ratio can be computed"
		return result
	}
	if *obs.GPUBusyPercent < thresholds.BusyPercentFloor {
		result.Result = ResultHealthy
		result.Message = fmt.Sprintf(
			"GPU is %d%% busy, below the %d%% floor at which clocks are meaningful",
			*obs.GPUBusyPercent, thresholds.BusyPercentFloor)
		return result
	}

	fraction := int32(int64(*obs.SCLKMHz) * 100 / int64(*obs.RatedSCLKMHz))
	if fraction < thresholds.ClockFractionFloor {
		result.Result = ResultUnhealthy
		result.Message = fmt.Sprintf(
			"GPU is %d%% busy but the shader clock is %d MHz, %d%% of the rated %d MHz; this is the amdgpu idle-clock lock (ROCm#5750)",
			*obs.GPUBusyPercent, *obs.SCLKMHz, fraction, *obs.RatedSCLKMHz)
		return result
	}
	result.Result = ResultHealthy
	result.Message = fmt.Sprintf(
		"GPU is %d%% busy at %d MHz, %d%% of the rated %d MHz",
		*obs.GPUBusyPercent, *obs.SCLKMHz, fraction, *obs.RatedSCLKMHz)
	return result
}

// countProbe reports a fault whenever a counter was read and is above zero.
func countProbe(name string, count *int32, what string) ProbeResult {
	result := ProbeResult{Probe: name}
	if count == nil {
		result.Result = ResultNotObserved
		result.Message = fmt.Sprintf("%s could not be counted", what)
		return result
	}
	if *count > 0 {
		result.Result = ResultUnhealthy
		result.Message = fmt.Sprintf("%d %s in the sampling window", *count, what)
		return result
	}
	result.Result = ResultHealthy
	result.Message = fmt.Sprintf("no %s in the sampling window", what)
	return result
}

// Summary aggregates probe results across nodes.
type Summary struct {
	// Result is the worst outcome: Unhealthy if any probe on any node found a
	// fault, otherwise NotObserved if any probe could not run or if nothing
	// was probed at all, otherwise Healthy.
	Result Result
	// Message is a one-line summary suitable for a status condition.
	Message string
	// Unhealthy names the probes that found a fault, deduplicated and sorted.
	Unhealthy []string
	// Detail is one concrete failing message, so a reader has somewhere to
	// start rather than only a list of names.
	Detail string
}

// Summarize reduces probe results to the state of the component.
//
// An empty set is NotObserved, never Healthy. Declaring a node healthy because
// nothing was checked is exactly the claim this operator refuses to make.
func Summarize(results []ProbeResult) Summary {
	summary := Summary{}
	unhealthy := map[string]bool{}
	notObserved := 0

	for _, r := range results {
		switch r.Result {
		case ResultUnhealthy:
			unhealthy[r.Probe] = true
			if summary.Detail == "" {
				summary.Detail = r.Message
			}
		case ResultNotObserved:
			notObserved++
		}
	}
	for probe := range unhealthy {
		summary.Unhealthy = append(summary.Unhealthy, probe)
	}
	sort.Strings(summary.Unhealthy)

	switch {
	case len(results) == 0:
		summary.Result = ResultNotObserved
		summary.Message = "no probe has run yet"
	case len(summary.Unhealthy) > 0:
		summary.Result = ResultUnhealthy
		summary.Message = fmt.Sprintf("failing: %s", joinProbes(summary.Unhealthy))
	case notObserved > 0:
		summary.Result = ResultNotObserved
		summary.Message = fmt.Sprintf(
			"%d of %d probes could not run; health is not established", notObserved, len(results))
	default:
		summary.Result = ResultHealthy
		summary.Message = fmt.Sprintf("all %d probes healthy", len(results))
	}
	return summary
}

func joinProbes(probes []string) string {
	out := ""
	for i, p := range probes {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
