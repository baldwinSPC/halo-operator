// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package runtimeprofile decides whether a node can run each declared compute
// backend. As with the memory policy and the health checks, every decision is
// a pure function of an observation: nothing here reads sysfs.
//
// It answers a narrower question than "which backend should this node use",
// on purpose. Whether Vulkan or ROCm is faster flips per model and per phase
// (GEP-0753 pain point #4), so that choice belongs to the workload. What a
// node can truthfully report is whether the kernel exposes what each backend
// needs, since a backend's userspace ships in the workload's image:
//
//   - vulkan needs a DRM render node for the GPU.
//   - rocm needs KFD to list the GPU as a compute agent, and the render node
//     as well: ROCm's runtime opens both /dev/kfd and the render node.
package runtimeprofile

import (
	"fmt"
	"sort"
	"strings"
)

// Backend names, matching the enum the CRD accepts.
const (
	BackendVulkan = "vulkan"
	BackendROCm   = "rocm"
)

// Result is whether one backend can run on one node.
//
// Three values, as everywhere in this operator: a node whose interfaces could
// not be read has not been shown able to run anything. It has shown nothing.
type Result string

const (
	// ResultAvailable means everything the backend needs was observed.
	ResultAvailable Result = "Available"
	// ResultUnavailable means something the backend needs was observed to be
	// missing.
	ResultUnavailable Result = "Unavailable"
	// ResultNotObserved means an input was not available, so neither answer
	// is established.
	ResultNotObserved Result = "NotObserved"
)

// Observation is what was read from one node. Both readings are pointers, and
// for both the zero value is a reading rather than an absence: an empty render
// node and a zero gfx target are each the kernel saying "there is none".
type Observation struct {
	// Node is the node this observation came from.
	Node string
	// RenderNode is the DRM render node registered for the GPU. Empty means
	// observed to have none; nil means not read.
	RenderNode *string
	// GFXTargetVersion is KFD's gfx_target_version for the GPU agent. Zero
	// means KFD was read and lists no GPU agent; nil means not read.
	GFXTargetVersion *int32
}

// BackendResult is one backend's outcome on one node, with the reasoning that
// produced it.
type BackendResult struct {
	Node    string
	Backend string
	Result  Result
	Message string
}

// Evaluate checks each requested backend against one observation.
//
// An unknown backend name produces a NotObserved result rather than being
// dropped, so a typo surfaces instead of quietly reducing what is checked.
func Evaluate(backends []string, obs Observation) []BackendResult {
	results := make([]BackendResult, 0, len(backends))
	for _, backend := range backends {
		r := BackendResult{Node: obs.Node, Backend: backend}
		switch backend {
		case BackendVulkan:
			r.Result, r.Message = vulkan(obs)
		case BackendROCm:
			r.Result, r.Message = rocm(obs)
		default:
			r.Result = ResultNotObserved
			r.Message = fmt.Sprintf("unknown backend %q; no such check is implemented", backend)
		}
		results = append(results, r)
	}
	return results
}

func vulkan(obs Observation) (Result, string) {
	switch {
	case obs.RenderNode == nil:
		return ResultNotObserved, "the GPU's DRM render node could not be read"
	case *obs.RenderNode == "":
		return ResultUnavailable, "the kernel registered no DRM render node for the GPU, which Vulkan needs"
	default:
		return ResultAvailable, fmt.Sprintf("render node %s", *obs.RenderNode)
	}
}

// rocm needs two things, and an observed absence of either outranks a
// failure to read the other: a node with no KFD GPU agent cannot run ROCm
// whatever its render node turns out to be.
func rocm(obs Observation) (Result, string) {
	var missing, unread []string
	switch {
	case obs.GFXTargetVersion == nil:
		unread = append(unread, "KFD's topology")
	case *obs.GFXTargetVersion == 0:
		missing = append(missing, "KFD lists no GPU compute agent, or the kernel has no KFD")
	}
	switch {
	case obs.RenderNode == nil:
		unread = append(unread, "the GPU's DRM render node")
	case *obs.RenderNode == "":
		missing = append(missing, "the kernel registered no DRM render node for the GPU")
	}
	switch {
	case len(missing) > 0:
		return ResultUnavailable, strings.Join(missing, ", and ") + "; ROCm needs a KFD GPU agent and a render node"
	case len(unread) > 0:
		return ResultNotObserved, strings.Join(unread, " and ") + " could not be read"
	default:
		return ResultAvailable, fmt.Sprintf("KFD lists the GPU as %s, render node %s",
			GFXName(*obs.GFXTargetVersion), *obs.RenderNode)
	}
}

// GFXName renders a KFD gfx_target_version as the target name ROCm uses.
// KFD encodes major*10000 + minor*100 + stepping, and the name spells minor
// and stepping as hex digits: 110501 is gfx1151, 90010 is gfx90a.
func GFXName(version int32) string {
	major := version / 10000
	minor := (version / 100) % 100
	stepping := version % 100
	return fmt.Sprintf("gfx%d%x%x", major, minor, stepping)
}

// Summary aggregates backend results across nodes.
type Summary struct {
	// Result is the worst outcome: Unavailable if any backend was observed
	// unable to run on any node, otherwise NotObserved if anything could not
	// be read or nothing was checked at all, otherwise Available.
	Result Result
	// Message is a one-line summary suitable for a status condition.
	Message string
	// Unavailable names the backends found unable to run, deduplicated and
	// sorted.
	Unavailable []string
	// Detail is one concrete failure with its node, so a reader has somewhere
	// to start rather than only a list of names.
	Detail string
}

// Summarize reduces backend results to the state of the component.
//
// An empty set is NotObserved, never Available: a fleet where nothing was
// checked has not been shown able to run anything.
func Summarize(results []BackendResult) Summary {
	summary := Summary{}
	unavailable := map[string]bool{}
	nodes := map[string]bool{}
	notObserved := 0

	for _, r := range results {
		nodes[r.Node] = true
		switch r.Result {
		case ResultUnavailable:
			unavailable[r.Backend] = true
			if summary.Detail == "" {
				summary.Detail = fmt.Sprintf("%s on node %s: %s", r.Backend, r.Node, r.Message)
			}
		case ResultNotObserved:
			notObserved++
		}
	}
	for backend := range unavailable {
		summary.Unavailable = append(summary.Unavailable, backend)
	}
	sort.Strings(summary.Unavailable)

	switch {
	case len(results) == 0:
		summary.Result = ResultNotObserved
		summary.Message = "no backend has been checked yet"
	case len(summary.Unavailable) > 0:
		summary.Result = ResultUnavailable
		summary.Message = "unavailable: " + strings.Join(summary.Unavailable, ", ")
	case notObserved > 0:
		summary.Result = ResultNotObserved
		summary.Message = fmt.Sprintf(
			"%d of %d backend checks could not be observed; availability is not established",
			notObserved, len(results))
	default:
		summary.Result = ResultAvailable
		summary.Message = fmt.Sprintf("every declared backend is available on %s", nodeCount(len(nodes)))
	}
	return summary
}

func nodeCount(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}
