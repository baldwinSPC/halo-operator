// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/standalone"
)

func runHalo(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// The exit-code contract. The tool's own failures must be 64 or 70 — never 0,
// 1 or 2, which are verdicts about the node. The sharpest case is a bad flag:
// Go's flag package exits 2 by default, and 2 here means "not observed".
func TestExitCodeContract(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, standalone.ExitUsage},
		{"unknown command", []string{"frobnicate"}, standalone.ExitUsage},
		{"bad flag is usage, never the flag package's 2", []string{"check", "-bogus"}, standalone.ExitUsage},
		{"stray positional argument", []string{"inspect", "extra"}, standalone.ExitUsage},
		{"check requires a policy", []string{"check"}, standalone.ExitUsage},
		{"apply requires a policy", []string{"apply"}, standalone.ExitUsage},
		{"a missing policy file", []string{"check", "-policy", "/nonexistent.yaml"}, standalone.ExitUsage},
		{"help", []string{"help"}, standalone.ExitCompliant},
		{"subcommand help", []string{"check", "-h"}, standalone.ExitCompliant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := runHalo(tc.args...); got != tc.want {
				t.Errorf("exit %d, want %d", got, tc.want)
			}
		})
	}
}

// asUID pins the privilege path a test exercises, instead of inheriting the
// runner's — CI's job container is root, a workstation usually is not.
func asUID(t *testing.T, uid int) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = prev })
}

// nodeFixture is a filesystem tree shaped like the real node — /proc, /sys and
// /etc/modprobe.d as amd-halo has them — and the policy the platform
// configures. It returns the root and the policy's path.
func nodeFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	must := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("proc/cmdline", "root=UUID=x splash quiet\n")
	must("sys/module/ttm/parameters/pages_limit", "24692260\n")
	dev := "sys/bus/pci/devices/0000:c2:00.0/"
	must(dev+"vendor", "0x1002\n")
	must(dev+"class", "0x038000\n")
	must(dev+"mem_info_gtt_total", "101139496960\n")
	must(dev+"gpu_busy_percent", "0\n")
	must(dev+"pp_dpm_sclk", "0: 600Mhz *\n1: 1100Mhz \n2: 2900Mhz \n")
	must(dev+"drm/renderD128/dev", "226:128\n")
	must(dev+"vbios_version", "113-STRXLGEN-001\n")
	must(dev+"fw_version/mes_fw_version", "0x00000088\n")
	must("sys/module/amdgpu/taint", "\n")
	must("sys/class/kfd/kfd/topology/nodes/0/properties", "cpu_cores_count 32\nsimd_count 0\ngfx_target_version 0\n")
	must("sys/class/kfd/kfd/topology/nodes/1/properties", "cpu_cores_count 0\nsimd_count 80\ngfx_target_version 110501\n")
	if err := os.MkdirAll(filepath.Join(root, "sys/bus/pci/drivers/amdgpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sys/bus/pci/drivers/amdgpu"), filepath.Join(root, dev, "driver")); err != nil {
		t.Fatal(err)
	}
	must("etc/modprobe.d/ttm.conf", ttmConf)
	must("policy.yaml", `kind: HaloDeviceConfig
spec:
  memoryPolicy:
    expectedKernelArgs: {ttm.pages_limit: "24692260"}
    minEffectiveGPUMemoryGiB: 90
  healthChecks: {probes: [clock-under-load]}
`)
	return root, filepath.Join(root, "policy.yaml")
}

const ttmConf = "options ttm pages_limit=24692260\n"

// checkArgs points check at the fixture tree.
func checkArgs(root, policy string, extra ...string) []string {
	return append([]string{"check", "-policy", policy, "-root", root,
		"-proc-root", filepath.Join(root, "proc"), "-sys-root", filepath.Join(root, "sys"), "-node", "amd-halo"},
		extra...)
}

// End to end through the CLI against a fixture tree shaped like the real node,
// unprivileged: the boot images are skipped and say so, and the skip does not
// change the answer.
func TestCheckEndToEnd(t *testing.T) {
	asUID(t, 1000)
	root, policy := nodeFixture(t)

	code, out, errOut := runHalo(checkArgs(root, policy)...)
	if code != standalone.ExitCompliant {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "from module parameter") {
		t.Errorf("the report does not say ttm.pages_limit came from the module parameter:\n%s", out)
	}
	if !strings.Contains(out, "boot-images") || !strings.Contains(out, "skipped, not counted") {
		t.Errorf("an unprivileged check does not say it skipped the boot images:\n%s", out)
	}
}

// -execute writes /etc/modprobe.d and rebuilds the initramfs, so without root
// it must refuse up front with its own exit code, before reading or planning
// anything. The privilege is pinned rather than read, so this runs on CI too,
// whose job container is root — it used to skip there.
func TestExecuteRequiresRoot(t *testing.T) {
	asUID(t, 1000)
	policy := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(policy, []byte("kind: HaloDeviceConfig\nspec: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _, errOut := runHalo("apply", "-policy", policy, "-execute"); got != standalone.ExitNoPerm {
		t.Errorf("exit %d, want %d; stderr: %s", got, standalone.ExitNoPerm, errOut)
	}
}

// images is a boot-image reader over canned contents, keyed by path relative
// to the fixture root.
type images struct {
	root   string
	list   []string
	files  map[string]map[string]string
	broken map[string]error
	calls  *int
}

func (im images) InitramfsImages() ([]string, error) {
	if im.calls != nil {
		*im.calls++
	}
	var out []string
	for _, rel := range im.list {
		out = append(out, filepath.Join(im.root, rel))
	}
	return out, nil
}

func (im images) ModprobeFiles(image string) (map[string]string, error) {
	rel, _ := filepath.Rel(im.root, image)
	if err := im.broken[rel]; err != nil {
		return nil, err
	}
	return im.files[rel], nil
}

func useImages(t *testing.T, r standalone.BootImageReader) {
	t.Helper()
	prev := newBootImageReader
	newBootImageReader = func(string) standalone.BootImageReader { return r }
	t.Cleanup(func() { newBootImageReader = prev })
}

// As root, check reads every boot image, and the answer about them reaches the
// exit code — the whole reason the check exists (halo-operator#20).
func TestCheckBootImagesAsRoot(t *testing.T) {
	const (
		boot = "boot/initrd.img-6.18+unreleased-amd64"
		esp  = "efi/amd-ryzen-ai-developer-platform/6.18+unreleased-amd64/initrd.img-6.18+unreleased-amd64"
	)
	carries := map[string]string{"ttm.conf": ttmConf}

	cases := []struct {
		name     string
		files    map[string]map[string]string
		broken   map[string]error
		wantExit int
		wantOut  []string
	}{
		{"every image carries modprobe.d as on disk",
			map[string]map[string]string{boot: carries, esp: carries}, nil,
			standalone.ExitCompliant, []string{"all 2 boot images carry /etc/modprobe.d as it is on disk"}},
		{"the ESP image lacks ttm.conf (#20)",
			map[string]map[string]string{boot: carries, esp: {}}, nil,
			standalone.ExitDrifted, []string{"/" + esp, "/etc/modprobe.d/ttm.conf is not present", "update-initramfs -u -k all"}},
		{"an image cannot be read",
			map[string]map[string]string{boot: carries}, map[string]error{esp: errors.New("unmkinitramfs failed")},
			standalone.ExitNotObserved, []string{"could not be read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asUID(t, 0)
			root, policy := nodeFixture(t)
			useImages(t, images{root: root, list: []string{boot, esp}, files: tc.files, broken: tc.broken})

			code, out, errOut := runHalo(checkArgs(root, policy)...)
			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.wantExit, out, errOut)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
		})
	}

	t.Run("the JSON report carries the component and each image", func(t *testing.T) {
		asUID(t, 0)
		root, policy := nodeFixture(t)
		useImages(t, images{root: root, list: []string{boot, esp},
			files: map[string]map[string]string{boot: carries, esp: {}}})

		code, out, _ := runHalo(checkArgs(root, policy, "-json")...)
		var report standalone.Report
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if code != standalone.ExitDrifted || report.ExitCode != code {
			t.Errorf("exit %d, report.exitCode %d; want both %d", code, report.ExitCode, standalone.ExitDrifted)
		}
		var found bool
		for _, c := range report.Components {
			if c.Name == standalone.BootImagesComponent {
				found = true
				if c.State != halov1alpha1.ComponentDegraded || len(c.Images) != 2 {
					t.Errorf("boot-images = %s with %d images, want Degraded with 2", c.State, len(c.Images))
				}
			}
		}
		if !found {
			t.Error("the JSON report has no boot-images component")
		}
	})

	t.Run("-boot-images=false skips it without reading anything", func(t *testing.T) {
		asUID(t, 0)
		root, policy := nodeFixture(t)
		calls := 0
		useImages(t, images{root: root, list: []string{esp}, files: map[string]map[string]string{esp: {}}, calls: &calls})

		code, out, _ := runHalo(checkArgs(root, policy, "-boot-images=false")...)
		if code != standalone.ExitCompliant || !strings.Contains(out, "skipped: -boot-images=false") {
			t.Errorf("exit %d, want 0 with the skip reported:\n%s", code, out)
		}
		if calls != 0 {
			t.Errorf("the boot images were listed %d times despite -boot-images=false", calls)
		}
	})
}
