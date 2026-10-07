// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

// amdHaloSpec is the reading taken off the physical AMD Ryzen AI Developer
// Platform (amd-halo, kernel 6.18.44+rex+5-amd64) on 2026-09-27, unprivileged.
// ttm is sized by /etc/modprobe.d/ttm.conf, so ttm.pages_limit appears as a
// module parameter and nowhere on the command line; amdgpu's parameters are
// mode 0400 and so are absent. The render node and the KFD target come from
// #8's reading of the same unit on 2026-09-14 (/dev/dri/renderD128, and KFD
// topology node 1 at gfx_target_version 110501).
func amdHaloSpec() halov1alpha1.HaloNodeObservationSpec {
	return halov1alpha1.HaloNodeObservationSpec{
		KernelCmdline:    ptr(`initrd=\amd-ryzen-ai-developer-platform\6.18.44+rex+5-amd64\initrd.img-6.18.44+rex+5-amd64 root=UUID=643b044c-c24b-418b-8ba5-72632dcb1892 splash quiet loglevel=3`),
		ModuleParams:     map[string]string{"ttm.pages_limit": "24692260", "ttm.page_pool_size": "16395959"},
		GTTTotalBytes:    ptr(int64(101139496960)),
		GPUBusyPercent:   ptr(int32(0)),
		SCLKMHz:          ptr(int32(600)),
		RatedSCLKMHz:     ptr(int32(2900)),
		DRMRenderNode:    ptr("renderD128"),
		GFXTargetVersion: ptr(int32(110501)),
		// Read 2026-10-06: the inbox amdgpu (no taint), the kernel's own TTM,
		// and the unit's firmware.
		AMDGPUTaint:  ptr(""),
		TTMModule:    ptr("ttm"),
		VBIOSVersion: ptr("113-STRXLGEN-001"),
		FirmwareVersions: map[string]string{
			"asd":     "0x21000110",
			"dmcub":   "0x09004700",
			"imu":     "0x0b352300",
			"mec":     "0x00000023",
			"me":      "0x00000020",
			"mes":     "0x00000088",
			"mes_kiq": "0x0000006f",
			"pfp":     "0x00000032",
			"rlc":     "0x11530506",
			"sdma":    "0x00000012",
			"smc":     "0x0a640900",
			"vcn":     "0x09118022",
		},
	}
}

func config(spec halov1alpha1.HaloDeviceConfigSpec) halov1alpha1.HaloDeviceConfig {
	return halov1alpha1.HaloDeviceConfig{Spec: spec}
}

// platformPolicy is what the platform itself configures. On the real node it
// is the first policy ever to evaluate compliant — the point of this test is
// that it stays that way.
func platformPolicy() halov1alpha1.HaloDeviceConfigSpec {
	return halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs:       map[string]string{"ttm.pages_limit": "24692260"},
			MinEffectiveGPUMemoryGiB: 90,
		},
		HealthChecks: halov1alpha1.HealthChecksSpec{Probes: []string{"clock-under-load"}},
	}
}

func TestTheRealNodeIsCompliantWithWhatThePlatformConfigures(t *testing.T) {
	r := Evaluate("amd-halo", amdHaloSpec(), config(platformPolicy()))
	if r.Outcome != OutcomeCompliant || r.ExitCode != ExitCompliant {
		t.Fatalf("outcome = %s (exit %d), want compliant (exit 0); components %+v", r.Outcome, r.ExitCode, r.Components)
	}
	// And the value is attributed to the module parameter — the modprobe.d
	// finding — not to a command line it is not on.
	for _, p := range r.Components[0].Params {
		if p.Name == "ttm.pages_limit" && p.Source != "module parameter" {
			t.Errorf("ttm.pages_limit source = %q, want module parameter", p.Source)
		}
	}
}

func TestExitCodesCarryTheThreeValuedAnswer(t *testing.T) {
	cases := []struct {
		name   string
		spec   halov1alpha1.HaloNodeObservationSpec
		policy halov1alpha1.HaloDeviceConfigSpec
		want   Outcome
		exit   int
	}{
		{"matched", amdHaloSpec(), platformPolicy(), OutcomeCompliant, ExitCompliant},
		{
			"observed disagreement is drift",
			amdHaloSpec(),
			halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: halov1alpha1.MemoryPolicySpec{
				ExpectedKernelArgs: map[string]string{"amd_iommu": "off"}}},
			OutcomeDrifted, ExitDrifted,
		},
		{
			// amdgpu's parameters are 0400: unreadable is not observed, never drift.
			"an unreadable module parameter is not observed",
			amdHaloSpec(),
			halov1alpha1.HaloDeviceConfigSpec{
				MemoryPolicy: halov1alpha1.MemoryPolicySpec{
					ExpectedKernelArgs: map[string]string{"amdgpu.gttsize": "126976"}},
				HealthChecks: halov1alpha1.HealthChecksSpec{Probes: []string{"clock-under-load"}},
			},
			OutcomeNotObserved, ExitNotObserved,
		},
		{
			"a node that could not be read at all is not observed",
			halov1alpha1.HaloNodeObservationSpec{},
			platformPolicy(),
			OutcomeNotObserved, ExitNotObserved,
		},
		{
			// The default probe set includes the kernel-log probes, which have
			// no reader, so health is undecidable — and that is reported, not
			// hidden behind a 0.
			"default probes are not observed rather than healthy",
			amdHaloSpec(),
			halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: platformPolicy().MemoryPolicy},
			OutcomeNotObserved, ExitNotObserved,
		},
		{
			"drift outranks missing information",
			halov1alpha1.HaloNodeObservationSpec{KernelCmdline: ptr("quiet")},
			halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: halov1alpha1.MemoryPolicySpec{
				ExpectedKernelArgs:       map[string]string{"amd_iommu": "off"},
				MinEffectiveGPUMemoryGiB: 90}},
			OutcomeDrifted, ExitDrifted,
		},
		{
			"nothing enabled establishes nothing",
			amdHaloSpec(),
			halov1alpha1.HaloDeviceConfigSpec{
				MemoryPolicy:   halov1alpha1.MemoryPolicySpec{Enabled: ptr(false)},
				RuntimeProfile: halov1alpha1.RuntimeProfileSpec{Enabled: ptr(false)},
				HealthChecks:   halov1alpha1.HealthChecksSpec{Enabled: ptr(false)},
				StackPolicy:    halov1alpha1.StackPolicySpec{Enabled: ptr(false)},
			},
			OutcomeNotObserved, ExitNotObserved,
		},
		{
			// The runtime profile counts toward the answer now that it is
			// implemented: a kernel without KFD cannot run ROCm, observed.
			"a node without KFD cannot run the default backends",
			func() halov1alpha1.HaloNodeObservationSpec {
				s := amdHaloSpec()
				s.GFXTargetVersion = ptr(int32(0))
				return s
			}(),
			platformPolicy(),
			OutcomeDrifted, ExitDrifted,
		},
		{
			"the same node passes when only vulkan is declared",
			func() halov1alpha1.HaloNodeObservationSpec {
				s := amdHaloSpec()
				s.GFXTargetVersion = ptr(int32(0))
				return s
			}(),
			func() halov1alpha1.HaloDeviceConfigSpec {
				p := platformPolicy()
				p.RuntimeProfile.Backends = []string{"vulkan"}
				return p
			}(),
			OutcomeCompliant, ExitCompliant,
		},
		{
			"an unread render node is not observed, never unavailable",
			func() halov1alpha1.HaloNodeObservationSpec {
				s := amdHaloSpec()
				s.DRMRenderNode = nil
				return s
			}(),
			platformPolicy(),
			OutcomeNotObserved, ExitNotObserved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate("n", tc.spec, config(tc.policy))
			if r.Outcome != tc.want || r.ExitCode != tc.exit {
				t.Errorf("outcome = %s (exit %d), want %s (exit %d); components %+v",
					r.Outcome, r.ExitCode, tc.want, tc.exit, r.Components)
			}
		})
	}
}

// The tool's own failures must never be readable as a verdict about the node.
func TestToolErrorsAreDisjointFromVerdicts(t *testing.T) {
	verdicts := map[int]bool{ExitCompliant: true, ExitDrifted: true, ExitNotObserved: true}
	for _, code := range []int{ExitUsage, ExitSoftware} {
		if verdicts[code] {
			t.Errorf("tool error code %d collides with a verdict", code)
		}
	}
}

// The lines explain the verdict; they must never contradict it.
func TestParamLinesAgreeWithTheVerdict(t *testing.T) {
	r := Evaluate("n", amdHaloSpec(), config(halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{ExpectedKernelArgs: map[string]string{
			"amd_iommu": "off", "amdgpu.gttsize": "126976", "ttm.pages_limit": "32505856"}},
	}))
	mem := r.Components[0]
	if mem.State != halov1alpha1.ComponentDegraded {
		t.Fatalf("state = %s, want Degraded", mem.State)
	}
	want := map[string]string{"amd_iommu": LineDrift, "amdgpu.gttsize": LineUnobserved, "ttm.pages_limit": LineDrift}
	for _, p := range mem.Params {
		if want[p.Name] != p.Result {
			t.Errorf("%s = %s, want %s", p.Name, p.Result, want[p.Name])
		}
	}
}

func TestNodeSelectorIsWarnedAboutNotSilentlyApplied(t *testing.T) {
	spec := platformPolicy()
	spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "somewhere-else"}
	r := Evaluate("amd-halo", amdHaloSpec(), config(spec))
	if len(r.Warnings) == 0 {
		t.Error("a nodeSelector was accepted without a warning that standalone mode ignores it")
	}
}

func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := LoadPolicy("../../config/samples/amdhalo_v1alpha1_halodeviceconfig.yaml"); err != nil {
		t.Errorf("the repository's own sample does not load: %v", err)
	}

	// A misspelled key must be an error. Silently ignoring it would check
	// nothing and pass — the worst available failure for this tool.
	typo := write("typo.yaml", "kind: HaloDeviceConfig\nspec:\n  memoryPolicy:\n    expectedKernelArg:\n      ttm.pages_limit: \"1\"\n")
	if _, err := LoadPolicy(typo); err == nil {
		t.Error("a misspelled policy key was accepted")
	}

	// runtimeProfile.backend was the node-wide backend choice, removed when
	// the stanza was rescoped to what a node can report (#27). A policy that
	// still sets it is refused rather than silently ignored.
	oldBackend := write("old-backend.yaml", "kind: HaloDeviceConfig\nspec:\n  runtimeProfile:\n    backend: vulkan\n")
	if _, err := LoadPolicy(oldBackend); err == nil {
		t.Error("the removed runtimeProfile.backend field was accepted")
	}

	// runtimeImages has a field per backend, so a misspelt backend is an
	// error rather than a catalogue entry no workload will ever look up.
	misspelt := write("misspelt.yaml", "kind: HaloDeviceConfig\nspec:\n  runtimeProfile:\n    runtimeImages:\n      rcom: example.com/rocm@sha256:0\n")
	if _, err := LoadPolicy(misspelt); err == nil {
		t.Error("a runtime image under a misspelt backend was accepted")
	}

	wrongKind := write("wrong.yaml", "kind: ConfigMap\nspec: {}\n")
	if _, err := LoadPolicy(wrongKind); err == nil {
		t.Error("a manifest of the wrong kind was accepted")
	}

	if _, err := LoadPolicy(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing policy file was accepted")
	}
}

// ExitNoPerm joins the tool-error codes, and must be as disjoint from the
// verdicts as they are.
func TestExitNoPermIsNotAVerdict(t *testing.T) {
	for _, v := range []int{ExitCompliant, ExitDrifted, ExitNotObserved, ExitUsage, ExitSoftware} {
		if ExitNoPerm == v {
			t.Errorf("ExitNoPerm (%d) collides with %d", ExitNoPerm, v)
		}
	}
}

func TestDeprecatedParameterIsWarnedAbout(t *testing.T) {
	r := Evaluate("n", amdHaloSpec(), config(halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{ExpectedKernelArgs: map[string]string{"amdgpu.gttsize": "126976"}}}))
	found := false
	for _, w := range r.Warnings {
		found = found || strings.Contains(w, "deprecated")
	}
	if !found {
		t.Errorf("warnings %q do not mention the deprecated gttsize", r.Warnings)
	}
}
