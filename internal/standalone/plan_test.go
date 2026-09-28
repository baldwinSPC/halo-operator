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

func TestEditOptions(t *testing.T) {
	cases := []struct {
		name, in, module, param, value, want string
	}{
		{"rewrites an existing value in place",
			"options ttm pages_limit=24692260\n", "ttm", "pages_limit", "32505856",
			"options ttm pages_limit=32505856\n"},
		{"leaves other parameters on the line alone",
			"options ttm page_pool_size=1 pages_limit=2\n", "ttm", "pages_limit", "9",
			"options ttm page_pool_size=1 pages_limit=9\n"},
		{"appends to the module's existing options line",
			"options ttm page_pool_size=1\n", "ttm", "pages_limit", "9",
			"options ttm page_pool_size=1 pages_limit=9\n"},
		{"adds a line when the module has none",
			"# comment\noptions other x=1\n", "ttm", "pages_limit", "9",
			"# comment\noptions other x=1\noptions ttm pages_limit=9\n"},
		{"creates content for a new file",
			"", "amdgpu", "gttsize", "126976",
			"options amdgpu gttsize=126976\n"},
		{"never touches another module with a similar name",
			"options ttm_extra pages_limit=1\n", "ttm", "pages_limit", "9",
			"options ttm_extra pages_limit=1\noptions ttm pages_limit=9\n"},
		{"preserves comments and unrelated directives",
			"# sized by ttm-config\nblacklist foo\noptions ttm pages_limit=1\n", "ttm", "pages_limit", "2",
			"# sized by ttm-config\nblacklist foo\noptions ttm pages_limit=2\n"},
		{"is idempotent",
			"options ttm pages_limit=9\n", "ttm", "pages_limit", "9",
			"options ttm pages_limit=9\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := editOptions(tc.in, tc.module, tc.param, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A backslash-continued options line is legal and the parameter may sit on the
// continued half; editing there is a guess, and a plan that shows one thing
// while the file would say another is worse than no plan.
func TestEditOptionsRefusesLineContinuations(t *testing.T) {
	if _, err := editOptions("options ttm \\\n  pages_limit=1\n", "ttm", "pages_limit", "2"); err == nil {
		t.Error("a continued options line was edited rather than refused")
	}
}

// fixtureRoot builds a filesystem root with the given modprobe.d files.
func fixtureRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func samplePolicy() halov1alpha1.HaloDeviceConfig {
	return config(halov1alpha1.HaloDeviceConfigSpec{MemoryPolicy: halov1alpha1.MemoryPolicySpec{
		ExpectedKernelArgs: map[string]string{
			"amd_iommu": "off", "amdgpu.gttsize": "126976", "ttm.pages_limit": "32505856"},
	}})
}

// The real node, the repository's sample policy, and the real modprobe.d.
func TestPlanAgainstTheRealNode(t *testing.T) {
	root := fixtureRoot(t, map[string]string{"etc/modprobe.d/ttm.conf": "options ttm pages_limit=24692260\n"})
	plan := PlanApply("amd-halo", amdHaloSpec(), samplePolicy(), root)

	actions := map[string]string{}
	for _, c := range plan.Params {
		actions[c.Param] = c.Action
	}
	want := map[string]string{
		// Core kernel parameter: the command line is Glimmer's, never planned here.
		"amd_iommu": ActionOutOfScope,
		// Unreadable at 0400, but the desired state is known, so it is planned.
		"amdgpu.gttsize":  ActionModprobe,
		"ttm.pages_limit": ActionModprobe,
	}
	for k, v := range want {
		if actions[k] != v {
			t.Errorf("%s: action %q, want %q", k, actions[k], v)
		}
	}

	// ttm is edited in the platform's OWN file — never a second, competing
	// file whose effect would then depend on modprobe's merge order.
	edits := map[string]FileEdit{}
	for _, e := range plan.Edits {
		edits[e.Path] = e
	}
	ttm, ok := edits["/etc/modprobe.d/ttm.conf"]
	if !ok || !ttm.Exists {
		t.Fatalf("ttm was not planned as an edit of the existing /etc/modprobe.d/ttm.conf; edits: %+v", plan.Edits)
	}
	if ttm.Desired != "options ttm pages_limit=32505856\n" {
		t.Errorf("ttm.conf would become %q", ttm.Desired)
	}
	if amd, ok := edits["/etc/modprobe.d/amdgpu.conf"]; !ok || amd.Exists {
		t.Errorf("amdgpu was not planned as a new /etc/modprobe.d/amdgpu.conf; edits: %+v", plan.Edits)
	}

	// Honesty: the initramfs rebuild is named, and so is the time bomb of
	// skipping it.
	joined := strings.Join(append(plan.FollowUp, plan.Warnings...), "\n")
	for _, must := range []string{"update-initramfs", "reboot", "lands silently"} {
		if !strings.Contains(joined, must) {
			t.Errorf("the plan does not mention %q", must)
		}
	}
	if plan.ExitCode != ExitDrifted {
		t.Errorf("exit = %d, want %d (action needed)", plan.ExitCode, ExitDrifted)
	}
}

// Planning must write nothing, ever.
func TestPlanWritesNothing(t *testing.T) {
	root := fixtureRoot(t, map[string]string{"etc/modprobe.d/ttm.conf": "options ttm pages_limit=24692260\n"})
	PlanApply("amd-halo", amdHaloSpec(), samplePolicy(), root)

	raw, _ := os.ReadFile(filepath.Join(root, "etc/modprobe.d/ttm.conf"))
	if string(raw) != "options ttm pages_limit=24692260\n" {
		t.Errorf("planning modified ttm.conf: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/modprobe.d/amdgpu.conf")); err == nil {
		t.Error("planning created amdgpu.conf")
	}
}

func TestPlanNothingToDo(t *testing.T) {
	root := fixtureRoot(t, map[string]string{"etc/modprobe.d/ttm.conf": "options ttm pages_limit=24692260\n"})
	plan := PlanApply("amd-halo", amdHaloSpec(), config(platformPolicy()), root)
	if plan.ExitCode != ExitCompliant || len(plan.Edits) != 0 {
		t.Errorf("exit %d with %d edits, want 0 and none", plan.ExitCode, len(plan.Edits))
	}
}

// A module parameter set on the command line wins at boot over anything in
// modprobe.d, so planning a modprobe.d edit for it would plan a no-op.
func TestPlanDoesNotEditModprobeForACmdlineParameter(t *testing.T) {
	spec := amdHaloSpec()
	spec.KernelCmdline = ptr("quiet ttm.pages_limit=24692260")
	root := fixtureRoot(t, nil)
	plan := PlanApply("n", spec, samplePolicy(), root)
	for _, c := range plan.Params {
		if c.Param == "ttm.pages_limit" && c.Action != ActionOutOfScope {
			t.Errorf("ttm.pages_limit on the command line was planned as %q, want out-of-scope", c.Action)
		}
	}
}

func TestPlanWarnsWhenAParameterIsSetInSeveralFiles(t *testing.T) {
	root := fixtureRoot(t, map[string]string{
		"etc/modprobe.d/ttm.conf":     "options ttm pages_limit=1\n",
		"usr/lib/modprobe.d/ttm.conf": "options ttm pages_limit=2\n",
	})
	plan := PlanApply("n", amdHaloSpec(), samplePolicy(), root)
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "set in 2 files") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning about the parameter being set in two files; warnings: %q", plan.Warnings)
	}
}

func TestDiffLines(t *testing.T) {
	got := diffLines("a\nb\n", "a\nc\nd\n")
	want := []string{"  a", "- b", "+ c", "+ d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}
