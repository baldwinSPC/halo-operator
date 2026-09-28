// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// End to end through the CLI against a fixture tree shaped like the real node.
func TestCheckEndToEnd(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(root, "sys/bus/pci/drivers/amdgpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sys/bus/pci/drivers/amdgpu"), filepath.Join(root, dev, "driver")); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "policy.yaml")
	must("policy.yaml", `kind: HaloDeviceConfig
spec:
  memoryPolicy:
    expectedKernelArgs: {ttm.pages_limit: "24692260"}
    minEffectiveGPUMemoryGiB: 90
  healthChecks: {probes: [clock-under-load]}
`)

	code, out, errOut := runHalo("check", "-policy", policy,
		"-proc-root", filepath.Join(root, "proc"), "-sys-root", filepath.Join(root, "sys"), "-node", "amd-halo")
	if code != standalone.ExitCompliant {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "from module parameter") {
		t.Errorf("the report does not say ttm.pages_limit came from the module parameter:\n%s", out)
	}
}
