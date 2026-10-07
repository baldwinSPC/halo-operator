// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package stackpolicy

import (
	"strings"
	"testing"
)

func str(s string) *string { return &s }

// amdHalo is the unit as read on 2026-10-06.
func amdHalo() Observation {
	return Observation{
		Node:          "amd-halo",
		KernelRelease: str("6.18.44+rex+5-amd64"),
		AMDGPUTaint:   str(""),
		TTMModule:     str("ttm"),
		Firmware:      map[string]string{"mes": "0x00000088", "smc": "0x0a640900", "vbios": "113-STRXLGEN-001"},
	}
}

func TestEvaluateStack(t *testing.T) {
	inbox := Policy{RequireInboxDriver: true}
	cases := []struct {
		name   string
		policy Policy
		mutate func(*Observation)
		want   Verdict
		reason string
	}{
		{"the physical unit runs the inbox driver", inbox, nil, Compliant, ""},
		{"an out-of-tree amdgpu is drift", inbox, func(o *Observation) { o.AMDGPUTaint = str("O") }, Drifted, "out-of-tree"},
		{"other taint flags are not dkms", inbox, func(o *Observation) { o.AMDGPUTaint = str("E") }, Compliant, ""},
		{"amdttm alone gives dkms away", inbox, func(o *Observation) { o.TTMModule = str("amdttm") }, Drifted, "amdttm"},
		{"an unread taint is not observed, never inbox", inbox, func(o *Observation) { o.AMDGPUTaint = nil }, NotObserved, ""},
		{"the driver check can be turned off", Policy{}, func(o *Observation) { o.AMDGPUTaint = str("O") }, Compliant, ""},
		{"above the floor", Policy{MinKernelVersion: "6.18.4"}, nil, Compliant, ""},
		{"below the floor by patch", Policy{MinKernelVersion: "6.18.45"}, nil, Drifted, "below the floor"},
		{"below the floor by minor", Policy{MinKernelVersion: "6.19"}, nil, Drifted, "below the floor"},
		{"a floor without a patch level", Policy{MinKernelVersion: "6.18"}, nil, Compliant, ""},
		// The release does not state its patch level, so it cannot be ordered
		// against a floor that has one: refused, not guessed.
		{"a release without a patch level is not ordered against a patch floor",
			Policy{MinKernelVersion: "6.18.4"}, func(o *Observation) { o.KernelRelease = str("6.18+unreleased-amd64") }, NotObserved, ""},
		{"but it is ordered when major.minor decides",
			Policy{MinKernelVersion: "6.19.1"}, func(o *Observation) { o.KernelRelease = str("6.18+unreleased-amd64") }, Drifted, "below the floor"},
		{"an unread release is not observed", Policy{MinKernelVersion: "6.18.4"}, func(o *Observation) { o.KernelRelease = nil }, NotObserved, ""},
		{"drift outranks missing information",
			Policy{RequireInboxDriver: true, MinKernelVersion: "6.19"}, func(o *Observation) { o.AMDGPUTaint = nil }, Drifted, "below the floor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := amdHalo()
			if tc.mutate != nil {
				tc.mutate(&obs)
			}
			got := EvaluateStack(tc.policy, obs)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %d, want %d (problems %q, unread %q)", got.Verdict, tc.want, got.Problems, got.Unread)
			}
			if tc.reason != "" && !strings.Contains(strings.Join(got.Problems, " "), tc.reason) {
				t.Errorf("problems %q do not mention %q", got.Problems, tc.reason)
			}
		})
	}
}

func TestEvaluateFirmware(t *testing.T) {
	bad := []KnownBad{{Component: "mes", Version: "0x00000088", Reason: "example"}}
	cases := []struct {
		name   string
		policy Policy
		mutate func(*Observation)
		want   Verdict
	}{
		{"read firmware and an empty list is compliant", Policy{}, nil, Compliant},
		{"unread firmware is not observed, never compliant", Policy{}, func(o *Observation) { o.Firmware = nil }, NotObserved},
		{"a known-bad version is drift", Policy{KnownBadFirmware: bad}, nil, Drifted},
		{"another version is compliant", Policy{KnownBadFirmware: bad}, func(o *Observation) { o.Firmware["mes"] = "0x00000089" }, Compliant},
		{"an unreported component is not observed", Policy{KnownBadFirmware: bad}, func(o *Observation) { delete(o.Firmware, "mes") }, NotObserved},
		{"vbios is a component like any other",
			Policy{KnownBadFirmware: []KnownBad{{Component: "vbios", Version: "113-STRXLGEN-001"}}}, nil, Drifted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := amdHalo()
			if tc.mutate != nil {
				tc.mutate(&obs)
			}
			if got := EvaluateFirmware(tc.policy, obs); got.Verdict != tc.want {
				t.Errorf("verdict = %d, want %d (problems %q, unread %q)", got.Verdict, tc.want, got.Problems, got.Unread)
			}
		})
	}
}

func TestSummarizeNamesEveryDriftedNode(t *testing.T) {
	got := Summarize("stack", []Finding{
		{Node: "b", Verdict: Drifted, Problems: []string{"x"}},
		{Node: "a", Verdict: Drifted, Problems: []string{"y"}},
		{Node: "c", Verdict: Compliant},
	})
	if got.Verdict != Drifted {
		t.Fatalf("verdict = %d, want Drifted", got.Verdict)
	}
	if !strings.Contains(got.Message, "a, b") {
		t.Errorf("message %q does not name both drifted nodes, sorted", got.Message)
	}
	if empty := Summarize("stack", nil); empty.Verdict != NotObserved {
		t.Errorf("no nodes = %d, want NotObserved", empty.Verdict)
	}
}
