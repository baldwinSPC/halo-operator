// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package memorypolicy

import (
	"reflect"
	"testing"
)

func gib(v int32) *int32 { return &v }

// haloDefaults is the kernel-argument set the Strix Halo guidance calls for.
var haloDefaults = Policy{
	ExpectedKernelArgs: map[string]string{
		"amd_iommu":       "off",
		"amdgpu.gttsize":  "126976",
		"ttm.pages_limit": "32505856",
	},
	MinEffectiveGPUMemoryGiB: 100,
}

// compliantObservation is a node that satisfies haloDefaults, used as the
// starting point for tests that spoil exactly one thing about it.
func compliantObservation() Observation {
	return Observation{
		Node: "halo-1",
		KernelParams: map[string]string{
			"amd_iommu":       "off",
			"amdgpu.gttsize":  "126976",
			"ttm.pages_limit": "32505856",
			"quiet":           "",
		},
		EffectiveGPUMemoryGiB: gib(124),
	}
}

func TestEvaluate(t *testing.T) {
	testCases := []struct {
		name           string
		policy         Policy
		observation    Observation
		wantVerdict    Verdict
		wantDrifts     int
		wantUnobserved int
	}{
		{
			name:        "a fully observed matching node is compliant",
			policy:      haloDefaults,
			observation: compliantObservation(),
			wantVerdict: VerdictCompliant,
		},
		{
			name:   "a parameter set to the wrong value is drift",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				o.KernelParams["amdgpu.gttsize"] = "8192"
				return o
			}(),
			wantVerdict: VerdictDrifted,
			wantDrifts:  1,
		},
		{
			name:   "a parameter missing from a readable command line is drift, not absence of information",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				delete(o.KernelParams, "amd_iommu")
				return o
			}(),
			wantVerdict: VerdictDrifted,
			wantDrifts:  1,
		},
		{
			name:   "an unreadable command line is unobserved, not drift",
			policy: haloDefaults,
			observation: Observation{
				Node:                  "halo-1",
				KernelParams:          nil,
				EffectiveGPUMemoryGiB: gib(124),
			},
			wantVerdict:    VerdictNotObserved,
			wantUnobserved: 3,
		},
		{
			name:   "memory below the floor is drift",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				o.EffectiveGPUMemoryGiB = gib(16)
				return o
			}(),
			wantVerdict: VerdictDrifted,
			wantDrifts:  1,
		},
		{
			name:   "memory exactly at the floor is compliant",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				o.EffectiveGPUMemoryGiB = gib(100)
				return o
			}(),
			wantVerdict: VerdictCompliant,
		},
		{
			name:   "unreadable memory is unobserved, not a zero reading",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				o.EffectiveGPUMemoryGiB = nil
				return o
			}(),
			wantVerdict:    VerdictNotObserved,
			wantUnobserved: 1,
		},
		{
			name:   "observed drift outranks missing information",
			policy: haloDefaults,
			observation: func() Observation {
				o := compliantObservation()
				o.KernelParams["amd_iommu"] = "on"
				o.EffectiveGPUMemoryGiB = nil
				return o
			}(),
			wantVerdict:    VerdictDrifted,
			wantDrifts:     1,
			wantUnobserved: 1,
		},
		{
			name:        "a policy that declares nothing is compliant without observing anything",
			policy:      Policy{},
			observation: Observation{Node: "halo-1"},
			wantVerdict: VerdictCompliant,
		},
		{
			name:   "a zero memory floor is not checked",
			policy: Policy{MinEffectiveGPUMemoryGiB: 0},
			observation: Observation{
				Node:                  "halo-1",
				EffectiveGPUMemoryGiB: nil,
			},
			wantVerdict: VerdictCompliant,
		},
		{
			name:   "a bare flag counts as present with an empty value",
			policy: Policy{ExpectedKernelArgs: map[string]string{"nomodeset": ""}},
			observation: Observation{
				Node:         "halo-1",
				KernelParams: map[string]string{"nomodeset": ""},
			},
			wantVerdict: VerdictCompliant,
		},
		{
			name:   "an empty command line that was read is drift for every expected parameter",
			policy: haloDefaults,
			observation: Observation{
				Node:                  "halo-1",
				KernelParams:          map[string]string{},
				EffectiveGPUMemoryGiB: gib(124),
			},
			wantVerdict: VerdictDrifted,
			wantDrifts:  3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.policy, tc.observation)
			if got.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %v, want %v (drifts %+v, unobserved %+v)",
					got.Verdict, tc.wantVerdict, got.Drifts, got.Unobserved)
			}
			if len(got.Drifts) != tc.wantDrifts {
				t.Errorf("got %d drifts, want %d: %+v", len(got.Drifts), tc.wantDrifts, got.Drifts)
			}
			if len(got.Unobserved) != tc.wantUnobserved {
				t.Errorf("got %d unobserved, want %d: %+v",
					len(got.Unobserved), tc.wantUnobserved, got.Unobserved)
			}
		})
	}
}

// TestEvaluateNeverClaimsComplianceOnPartialData is the invariant that matters
// most: no combination of missing readings may produce Compliant.
func TestEvaluateNeverClaimsComplianceOnPartialData(t *testing.T) {
	partials := []struct {
		name string
		obs  Observation
	}{
		{"no kernel parameters", Observation{Node: "n", EffectiveGPUMemoryGiB: gib(124)}},
		{"no memory reading", Observation{Node: "n", KernelParams: compliantObservation().KernelParams}},
		{"nothing at all", Observation{Node: "n"}},
	}
	for _, p := range partials {
		t.Run(p.name, func(t *testing.T) {
			if got := Evaluate(haloDefaults, p.obs); got.Verdict == VerdictCompliant {
				t.Errorf("verdict = Compliant on partial data, which asserts an observation that was never made")
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	testCases := []struct {
		name        string
		findings    []Finding
		wantVerdict Verdict
	}{
		{
			name:        "no findings is not observed, never compliant",
			findings:    nil,
			wantVerdict: VerdictNotObserved,
		},
		{
			name:        "all compliant is compliant",
			findings:    []Finding{{Verdict: VerdictCompliant}, {Verdict: VerdictCompliant}},
			wantVerdict: VerdictCompliant,
		},
		{
			name:        "one drifted node drifts the whole config",
			findings:    []Finding{{Verdict: VerdictCompliant}, {Verdict: VerdictDrifted}},
			wantVerdict: VerdictDrifted,
		},
		{
			name:        "drift outranks unobserved",
			findings:    []Finding{{Verdict: VerdictNotObserved}, {Verdict: VerdictDrifted}},
			wantVerdict: VerdictDrifted,
		},
		{
			name:        "one unobserved node blocks a claim of compliance",
			findings:    []Finding{{Verdict: VerdictCompliant}, {Verdict: VerdictNotObserved}},
			wantVerdict: VerdictNotObserved,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := Summarize(tc.findings)
			if got.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %v, want %v", got.Verdict, tc.wantVerdict)
			}
			if got.Message == "" {
				t.Errorf("summary carries no message")
			}
		})
	}
}

func TestParseKernelCmdline(t *testing.T) {
	testCases := []struct {
		name    string
		cmdline string
		want    map[string]string
	}{
		{
			name:    "an empty command line yields no parameters",
			cmdline: "",
			want:    map[string]string{},
		},
		{
			name:    "key=value pairs and bare flags",
			cmdline: "BOOT_IMAGE=/vmlinuz quiet amd_iommu=off",
			want: map[string]string{
				"BOOT_IMAGE": "/vmlinuz",
				"quiet":      "",
				"amd_iommu":  "off",
			},
		},
		{
			name:    "the Halo unified-memory arguments",
			cmdline: "amd_iommu=off amdgpu.gttsize=126976 ttm.pages_limit=32505856",
			want: map[string]string{
				"amd_iommu":       "off",
				"amdgpu.gttsize":  "126976",
				"ttm.pages_limit": "32505856",
			},
		},
		{
			name:    "runs of whitespace and a trailing newline are ignored",
			cmdline: "  amd_iommu=off   quiet \n",
			want:    map[string]string{"amd_iommu": "off", "quiet": ""},
		},
		{
			name:    "a quoted value keeps its spaces and loses its quotes",
			cmdline: `amd_iommu=off custom="a b c" quiet`,
			want: map[string]string{
				"amd_iommu": "off",
				"custom":    "a b c",
				"quiet":     "",
			},
		},
		{
			name:    "a later occurrence wins, as the kernel does",
			cmdline: "amd_iommu=on amd_iommu=off",
			want:    map[string]string{"amd_iommu": "off"},
		},
		{
			name:    "a value containing an equals sign is preserved",
			cmdline: "root=UUID=1234-5678",
			want:    map[string]string{"root": "UUID=1234-5678"},
		},
		{
			name:    "an explicitly empty value is not a bare flag",
			cmdline: "amd_iommu=",
			want:    map[string]string{"amd_iommu": ""},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseKernelCmdline(tc.cmdline)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseKernelCmdline(%q) = %+v, want %+v", tc.cmdline, got, tc.want)
			}
		})
	}
}

func TestFindingDescribe(t *testing.T) {
	f := Evaluate(
		Policy{ExpectedKernelArgs: map[string]string{"amd_iommu": "off", "amdgpu.gttsize": "126976"}},
		Observation{Node: "halo-1", KernelParams: map[string]string{"amd_iommu": "on"}},
	)
	got := f.Describe()
	want := `amd_iommu expected "off" but was "on"; amdgpu.gttsize expected "126976" but was not set`
	if got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}
