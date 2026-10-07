// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package stackpolicy decides whether a node runs the software stack a fleet
// declared under ROCm and Vulkan: the inbox amdgpu rather than amdgpu-dkms, a
// kernel at or above a floor, and no GPU firmware known to be bad. As with the
// other decision packages, every decision is a pure function of an
// observation, so it all runs in CI on hardware that has never seen an APU.
//
// Two verdicts come out of it, because they are acted on by different people:
// Stack (driver and kernel) is fixed by reinstalling or upgrading the OS
// stack; GPUFirmware by moving the firmware package. Neither is ever fixed by
// this operator, which only reports.
package stackpolicy

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Verdict is what a node was found to be against one part of the policy.
type Verdict int

const (
	// NotObserved: something needed was not read, so nothing is claimed.
	NotObserved Verdict = iota
	// Compliant: everything needed was read and matched.
	Compliant
	// Drifted: something was read and did not match.
	Drifted
)

// Observation is what was read from one node. Every reading is a pointer or a
// nil-able map, because a reading that was not taken must stay distinguishable
// from one that was taken and found empty: an empty taint is the inbox driver.
type Observation struct {
	Node string
	// KernelRelease is the running kernel's release, as `uname -r` prints it.
	KernelRelease *string
	// AMDGPUTaint is the amdgpu module's taint flags; "" for none.
	AMDGPUTaint *string
	// TTMModule is the loaded TTM module; "" for none.
	TTMModule *string
	// Firmware maps a GPU firmware component ("mes", "smc", ..., "vbios") to
	// its version as read. Nil when none could be read.
	Firmware map[string]string
}

// KnownBad is one firmware version to flag.
type KnownBad struct {
	Component string
	Version   string
	Reason    string
}

// Policy is the declared stack.
type Policy struct {
	// RequireInboxDriver fails a node running out-of-tree amdgpu.
	RequireInboxDriver bool
	// MinKernelVersion is a dotted floor ("6.18.4"); empty means none.
	MinKernelVersion string
	// KnownBadFirmware is checked by EvaluateFirmware.
	KnownBadFirmware []KnownBad
}

// Finding is one node's verdict on one part of the policy, with every check
// that produced it.
type Finding struct {
	Node    string
	Verdict Verdict
	// Problems names each observed disagreement, for the condition message.
	Problems []string
	// Unread names each check that could not be made.
	Unread []string
}

// outOfTreeTTM are the TTM copies only amdgpu-dkms ships.
var outOfTreeTTM = map[string]bool{"amdttm": true, "amd_ttm": true}

// EvaluateStack checks the driver and the kernel.
//
// The driver check rests on AMD's own guidance, not on this project's taste:
// for gfx1151 APUs AMD recommends the inbox amdgpu and closed ROCm#6186
// (PERMISSION_FAULT storms) and ROCm#6118 (queue-create faults) as caused by
// amdgpu-dkms. The kernel marks an out-of-tree module with the taint flag "O";
// amdgpu-dkms also ships its own TTM, amdttm, whose presence says the same.
func EvaluateStack(p Policy, obs Observation) Finding {
	f := Finding{Node: obs.Node}

	if p.RequireInboxDriver {
		switch {
		case obs.AMDGPUTaint == nil:
			f.Unread = append(f.Unread, "the amdgpu module's taint could not be read")
		case strings.Contains(*obs.AMDGPUTaint, "O"):
			f.Problems = append(f.Problems, fmt.Sprintf(
				"amdgpu is an out-of-tree module (taint %q), which is how amdgpu-dkms shows up; "+
					"AMD's guidance for gfx1151 APUs is the inbox driver (ROCm#6186, ROCm#6118)", *obs.AMDGPUTaint))
		case obs.TTMModule != nil && outOfTreeTTM[*obs.TTMModule]:
			f.Problems = append(f.Problems, fmt.Sprintf(
				"the loaded TTM is %s, which only amdgpu-dkms ships; "+
					"AMD's guidance for gfx1151 APUs is the inbox driver (ROCm#6186, ROCm#6118)", *obs.TTMModule))
		}
	}

	if p.MinKernelVersion != "" {
		switch {
		case obs.KernelRelease == nil:
			f.Unread = append(f.Unread, "the running kernel's release could not be read")
		default:
			cmp, ok := compareRelease(*obs.KernelRelease, p.MinKernelVersion)
			switch {
			case !ok:
				f.Unread = append(f.Unread, fmt.Sprintf(
					"kernel %s cannot be ordered against the floor %s", *obs.KernelRelease, p.MinKernelVersion))
			case cmp < 0:
				f.Problems = append(f.Problems, fmt.Sprintf(
					"kernel %s is below the floor %s", *obs.KernelRelease, p.MinKernelVersion))
			}
		}
	}

	f.Verdict = verdict(f)
	return f
}

// EvaluateFirmware checks the GPU firmware against the known-bad list.
//
// A component the list names but the node did not report is unread rather
// than clean: a block whose version cannot be seen has not been shown to be
// any particular version. With an empty list, a node whose firmware was read
// is compliant, and one whose firmware was not read is not observed.
func EvaluateFirmware(p Policy, obs Observation) Finding {
	f := Finding{Node: obs.Node}
	if obs.Firmware == nil {
		f.Unread = append(f.Unread, "the GPU firmware versions could not be read")
		f.Verdict = verdict(f)
		return f
	}
	for _, bad := range p.KnownBadFirmware {
		got, ok := obs.Firmware[bad.Component]
		switch {
		case !ok:
			f.Unread = append(f.Unread, fmt.Sprintf("%s firmware was not reported", bad.Component))
		case got == bad.Version:
			problem := fmt.Sprintf("%s firmware is %s, which is known to be bad", bad.Component, got)
			if bad.Reason != "" {
				problem += ": " + bad.Reason
			}
			f.Problems = append(f.Problems, problem)
		}
	}
	f.Verdict = verdict(f)
	return f
}

// verdict ranks observed disagreement above missing information, and missing
// information above any claim of compliance.
func verdict(f Finding) Verdict {
	switch {
	case len(f.Problems) > 0:
		return Drifted
	case len(f.Unread) > 0:
		return NotObserved
	default:
		return Compliant
	}
}

// releaseVersion is the leading dotted version of a kernel release:
// "6.18.44" of "6.18.44+rex+5-amd64", "6.18" of "6.18+unreleased-amd64".
var releaseVersion = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// compareRelease orders a kernel release against a dotted floor. It answers
// only when the comparison is decidable from what the release states. A
// release without a patch level ("6.18+unreleased-amd64", whose real patch
// level the release string does not carry) cannot be ordered against a floor
// that has one in the same major.minor, so that comparison is refused rather
// than guessed in either direction.
func compareRelease(release, floor string) (int, bool) {
	r := releaseVersion.FindStringSubmatch(release)
	fl := releaseVersion.FindStringSubmatch(floor)
	if r == nil || fl == nil || fl[0] != floor {
		return 0, false
	}
	for i := 1; i <= 2; i++ {
		a, _ := strconv.Atoi(r[i])
		b, _ := strconv.Atoi(fl[i])
		if a != b {
			if a < b {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case fl[3] == "":
		return 0, true
	case r[3] == "":
		return 0, false
	}
	a, _ := strconv.Atoi(r[3])
	b, _ := strconv.Atoi(fl[3])
	switch {
	case a < b:
		return -1, true
	case a > b:
		return 1, true
	}
	return 0, true
}

// Summary aggregates findings across the nodes a config targets.
type Summary struct {
	// Verdict is the worst across nodes; no nodes at all is NotObserved.
	Verdict Verdict
	// Message is a one-line summary suitable for a status condition. It names
	// every drifted node, sorted, so a fleet reader knows where to look.
	Message string
}

// Summarize reduces per-node findings. what names the part of the policy
// ("stack", "GPU firmware") in the message.
func Summarize(what string, findings []Finding) Summary {
	var drifted, unread []string
	var firstProblem string
	for _, f := range findings {
		switch f.Verdict {
		case Drifted:
			drifted = append(drifted, f.Node)
			if firstProblem == "" {
				firstProblem = fmt.Sprintf("%s: %s", f.Node, strings.Join(f.Problems, "; "))
			}
		case NotObserved:
			unread = append(unread, f.Node)
		}
	}
	sort.Strings(drifted)
	sort.Strings(unread)
	switch {
	case len(findings) == 0:
		return Summary{NotObserved, "no node has been observed yet"}
	case len(drifted) > 0:
		return Summary{Drifted, fmt.Sprintf("%d of %d node(s) drifted from the declared %s (%s): %s",
			len(drifted), len(findings), what, strings.Join(drifted, ", "), firstProblem)}
	case len(unread) > 0:
		return Summary{NotObserved, fmt.Sprintf("%d of %d node(s) not fully observed, so the %s is not established: %s",
			len(unread), len(findings), what, strings.Join(unread, ", "))}
	default:
		return Summary{Compliant, fmt.Sprintf("all %d node(s) match the declared %s", len(findings), what)}
	}
}
