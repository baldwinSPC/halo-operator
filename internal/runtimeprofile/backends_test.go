// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package runtimeprofile

import (
	"strings"
	"testing"
)

func str(s string) *string { return &s }
func i32(v int32) *int32   { return &v }

// amdHalo is what the physical unit exposes: render node renderD128 and a KFD
// GPU agent at gfx_target_version 110501 (#8's comment, 2026-09-14).
func amdHalo() Observation {
	return Observation{Node: "amd-halo", RenderNode: str("renderD128"), GFXTargetVersion: i32(110501)}
}

func TestEvaluate(t *testing.T) {
	both := []string{BackendVulkan, BackendROCm}
	cases := []struct {
		name   string
		obs    Observation
		vulkan Result
		rocm   Result
	}{
		{"the physical unit runs both", amdHalo(), ResultAvailable, ResultAvailable},
		{"nothing readable is not observed, never unavailable",
			Observation{Node: "n"}, ResultNotObserved, ResultNotObserved},
		{"no render node blocks both backends",
			Observation{Node: "n", RenderNode: str(""), GFXTargetVersion: i32(110501)},
			ResultUnavailable, ResultUnavailable},
		{"no KFD GPU agent blocks only ROCm",
			Observation{Node: "n", RenderNode: str("renderD128"), GFXTargetVersion: i32(0)},
			ResultAvailable, ResultUnavailable},
		{"unreadable KFD leaves ROCm unproven",
			Observation{Node: "n", RenderNode: str("renderD128")},
			ResultAvailable, ResultNotObserved},
		// Observed absence outranks a failure to read: a node with no KFD GPU
		// agent cannot run ROCm whatever its render node turns out to be.
		{"an observed KFD absence outranks an unread render node",
			Observation{Node: "n", GFXTargetVersion: i32(0)},
			ResultNotObserved, ResultUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(both, tc.obs)
			if len(got) != 2 {
				t.Fatalf("got %d results, want 2", len(got))
			}
			if got[0].Backend != BackendVulkan || got[0].Result != tc.vulkan {
				t.Errorf("vulkan = %s (%s), want %s", got[0].Result, got[0].Message, tc.vulkan)
			}
			if got[1].Backend != BackendROCm || got[1].Result != tc.rocm {
				t.Errorf("rocm = %s (%s), want %s", got[1].Result, got[1].Message, tc.rocm)
			}
			for _, r := range got {
				if r.Message == "" {
					t.Errorf("%s has no message; every result must say why", r.Backend)
				}
				if r.Node != tc.obs.Node {
					t.Errorf("%s result names node %q, want %q", r.Backend, r.Node, tc.obs.Node)
				}
			}
		})
	}
}

func TestAvailableROCmNamesTheTarget(t *testing.T) {
	got := Evaluate([]string{BackendROCm}, amdHalo())
	if !strings.Contains(got[0].Message, "gfx1151") {
		t.Errorf("message %q does not name the gfx target", got[0].Message)
	}
}

func TestUnknownBackendIsNotObservedNotDropped(t *testing.T) {
	got := Evaluate([]string{"cuda"}, amdHalo())
	if len(got) != 1 || got[0].Result != ResultNotObserved {
		t.Fatalf("got %+v, want one NotObserved result", got)
	}
	if !strings.Contains(got[0].Message, "cuda") {
		t.Errorf("message %q does not name the unknown backend", got[0].Message)
	}
}

func TestGFXName(t *testing.T) {
	for version, want := range map[int32]string{
		110501: "gfx1151", // Strix Halo, read off amd-halo
		110000: "gfx1100",
		90010:  "gfx90a", // stepping 10 is the hex digit a
		90402:  "gfx942",
	} {
		if got := GFXName(version); got != want {
			t.Errorf("GFXName(%d) = %s, want %s", version, got, want)
		}
	}
}

func TestSummarize(t *testing.T) {
	available := BackendResult{Node: "a", Backend: BackendVulkan, Result: ResultAvailable, Message: "ok"}
	unread := BackendResult{Node: "b", Backend: BackendVulkan, Result: ResultNotObserved, Message: "unread"}
	missing := BackendResult{Node: "c", Backend: BackendROCm, Result: ResultUnavailable, Message: "no KFD"}

	cases := []struct {
		name    string
		results []BackendResult
		want    Result
	}{
		{"nothing checked is not observed, never available", nil, ResultNotObserved},
		{"all available", []BackendResult{available}, ResultAvailable},
		{"one unread node withholds availability", []BackendResult{available, unread}, ResultNotObserved},
		{"observed absence outranks missing information", []BackendResult{available, unread, missing}, ResultUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summarize(tc.results); got.Result != tc.want {
				t.Errorf("Result = %s (%s), want %s", got.Result, got.Message, tc.want)
			}
		})
	}
}

func TestSummarizeDetailNamesTheNodeAndBackend(t *testing.T) {
	got := Summarize([]BackendResult{
		{Node: "a", Backend: BackendVulkan, Result: ResultAvailable, Message: "ok"},
		{Node: "halo-2", Backend: BackendROCm, Result: ResultUnavailable, Message: "KFD lists no GPU compute agent"},
	})
	if len(got.Unavailable) != 1 || got.Unavailable[0] != BackendROCm {
		t.Errorf("Unavailable = %v, want [rocm]", got.Unavailable)
	}
	for _, want := range []string{"halo-2", "rocm", "KFD lists no GPU compute agent"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("Detail %q does not mention %q", got.Detail, want)
		}
	}
}
