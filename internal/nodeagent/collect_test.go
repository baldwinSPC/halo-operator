// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a fake /proc + /sys tree and returns a Collector over it.
// The default tree is one healthy Halo-shaped node: a kernel command line with
// the recommended arguments, and a single amdgpu-driven display device with
// every documented counter present. Tests spoil exactly one thing about it.
type fixture struct {
	t        *testing.T
	procRoot string
	sysRoot  string
}

const fixtureCmdline = "BOOT_IMAGE=/vmlinuz root=/dev/nvme0n1p2 amd_iommu=off amdgpu.gttsize=126976 ttm.pages_limit=32505856 quiet\n"

// fixtureRelease is the physical unit's running kernel, 2026-09-27.
const fixtureRelease = "6.18.44+rex+5-amd64"

const fixtureLadder = "0: 200Mhz\n1: 1100Mhz\n2: 2900Mhz *\n"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:        t,
		procRoot: filepath.Join(root, "proc"),
		sysRoot:  filepath.Join(root, "sys"),
	}
	f.writeFile(filepath.Join(f.procRoot, "cmdline"), fixtureCmdline)
	f.writeFile(filepath.Join(f.procRoot, "sys", "kernel", "osrelease"), fixtureRelease+"\n")
	f.addDevice("0000:c5:00.0", "0x1002", "0x030000", "amdgpu", map[string]string{
		"mem_info_gtt_total": "133143986176\n", // 124 GiB
		"gpu_busy_percent":   "97\n",
		"pp_dpm_sclk":        fixtureLadder,
	})
	f.addModuleParams("ttm", map[string]string{"pages_limit": "32505856\n"})
	f.addModuleParams("amdgpu", map[string]string{"gttsize": "126976\n"})
	return f
}

// addModuleParams creates /sys/module/<module>/parameters/<name> files.
func (f *fixture) addModuleParams(module string, params map[string]string) {
	f.t.Helper()
	for name, content := range params {
		f.writeFile(filepath.Join(f.sysRoot, "module", module, "parameters", name), content)
	}
}

func (f *fixture) collector() Collector {
	return Collector{ProcRoot: f.procRoot, SysRoot: f.sysRoot}
}

func (f *fixture) writeFile(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// addDevice creates a PCI device directory with the given identity and
// attribute files. driver may be "" for a device with no driver bound.
func (f *fixture) addDevice(addr, vendor, class, driver string, files map[string]string) {
	f.t.Helper()
	dev := filepath.Join(f.sysRoot, "bus", "pci", "devices", addr)
	f.writeFile(filepath.Join(dev, "vendor"), vendor+"\n")
	f.writeFile(filepath.Join(dev, "class"), class+"\n")
	for name, content := range files {
		f.writeFile(filepath.Join(dev, name), content)
	}
	if driver != "" {
		target := filepath.Join(f.sysRoot, "bus", "pci", "drivers", driver)
		if err := os.MkdirAll(target, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dev, "driver")); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) device(addr string) string {
	return filepath.Join(f.sysRoot, "bus", "pci", "devices", addr)
}

// messagesMention fails the test unless some message contains the substring.
// Every nil reading must be explained; a bare nil tells the operator nothing.
func messagesMention(t *testing.T, r Reading, substring string) {
	t.Helper()
	for _, m := range r.Messages {
		if strings.Contains(m, substring) {
			return
		}
	}
	t.Errorf("no message mentions %q; messages were %q", substring, r.Messages)
}

func TestCollectReadsAHealthyNode(t *testing.T) {
	r := newFixture(t).collector().Collect()

	if r.KernelCmdline == nil {
		t.Fatal("KernelCmdline is nil for a readable cmdline")
	}
	if want := strings.TrimRight(fixtureCmdline, "\n"); *r.KernelCmdline != want {
		t.Errorf("KernelCmdline = %q, want %q", *r.KernelCmdline, want)
	}
	if r.GTTTotalBytes == nil || *r.GTTTotalBytes != 133143986176 {
		t.Errorf("GTTTotalBytes = %v, want 133143986176", r.GTTTotalBytes)
	}
	if r.GPUBusyPercent == nil || *r.GPUBusyPercent != 97 {
		t.Errorf("GPUBusyPercent = %v, want 97", r.GPUBusyPercent)
	}
	if r.SCLKMHz == nil || *r.SCLKMHz != 2900 {
		t.Errorf("SCLKMHz = %v, want 2900", r.SCLKMHz)
	}
	if r.RatedSCLKMHz == nil || *r.RatedSCLKMHz != 2900 {
		t.Errorf("RatedSCLKMHz = %v, want 2900", r.RatedSCLKMHz)
	}
	if len(r.Messages) != 0 {
		t.Errorf("a fully readable node produced messages: %q", r.Messages)
	}
}

// TestCollectZeroIsAMeasurement is the contract test for the whole package: a
// counter that reads zero must arrive as a present zero, because zero busy is
// a real state of a real GPU and turning it into nil (or nil into zero) is the
// collapse this operator exists to refuse.
func TestCollectZeroIsAMeasurement(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "gpu_busy_percent"), "0\n")

	r := f.collector().Collect()
	if r.GPUBusyPercent == nil {
		t.Fatal("GPUBusyPercent = nil for a readable counter holding 0")
	}
	if *r.GPUBusyPercent != 0 {
		t.Errorf("GPUBusyPercent = %d, want 0", *r.GPUBusyPercent)
	}
}

func TestCollectUnreadableCmdlineIsNilWithAMessage(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(filepath.Join(f.procRoot, "cmdline")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.KernelCmdline != nil {
		t.Errorf("KernelCmdline = %q for an unreadable cmdline, want nil", *r.KernelCmdline)
	}
	messagesMention(t, r, "cmdline")
	// The sysfs side must be unaffected: one unreadable source never poisons
	// another.
	if r.GTTTotalBytes == nil {
		t.Error("GTTTotalBytes became nil because cmdline was unreadable")
	}
}

func TestCollectUnreadableCounterIsNilNotZero(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(filepath.Join(f.device("0000:c5:00.0"), "mem_info_gtt_total")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.GTTTotalBytes != nil {
		t.Errorf("GTTTotalBytes = %d for an unreadable counter, want nil", *r.GTTTotalBytes)
	}
	messagesMention(t, r, "gtt")
	if r.GPUBusyPercent == nil || r.RatedSCLKMHz == nil {
		t.Error("other counters became nil because one was unreadable")
	}
}

func TestCollectGarbageCounterIsNilWithAMessage(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "mem_info_gtt_total"), "not a number\n")

	r := f.collector().Collect()
	if r.GTTTotalBytes != nil {
		t.Errorf("GTTTotalBytes = %d for a garbage counter, want nil", *r.GTTTotalBytes)
	}
	messagesMention(t, r, "gtt")
}

func TestCollectBusyPercentOutsideRangeIsNil(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "gpu_busy_percent"), "150\n")

	r := f.collector().Collect()
	if r.GPUBusyPercent != nil {
		t.Errorf("GPUBusyPercent = %d for an out-of-range value, want nil", *r.GPUBusyPercent)
	}
	messagesMention(t, r, "0..100")
}

func TestCollectNoAMDGPU(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(f.device("0000:c5:00.0")); err != nil {
		t.Fatal(err)
	}
	// An NVIDIA display device alone is not an amdgpu.
	f.addDevice("0000:01:00.0", "0x10de", "0x030000", "nvidia", nil)

	r := f.collector().Collect()
	if r.GTTTotalBytes != nil || r.GPUBusyPercent != nil || r.SCLKMHz != nil || r.RatedSCLKMHz != nil {
		t.Error("device counters are set with no amdgpu present")
	}
	if r.KernelCmdline == nil {
		t.Error("the kernel command line went unread because no GPU was found")
	}
	messagesMention(t, r, "no amdgpu")
}

// TestCollectIgnoresNonGPUFunctions covers the audio function every AMD GPU
// exposes: same vendor, class 0x0403, and it must not be mistaken for the GPU.
func TestCollectIgnoresNonGPUFunctions(t *testing.T) {
	f := newFixture(t)
	f.addDevice("0000:c5:00.1", "0x1002", "0x040300", "snd_hda_intel", nil)

	r := f.collector().Collect()
	if r.GTTTotalBytes == nil || *r.GTTTotalBytes != 133143986176 {
		t.Errorf("GTTTotalBytes = %v; the audio function confused device discovery", r.GTTTotalBytes)
	}
}

// TestCollectIgnoresDriverlessDevice: an AMD display device bound to vfio-pci
// (or nothing) has no amdgpu counter files, and reading it would fail one file
// at a time. It is not an observable GPU.
func TestCollectIgnoresDriverlessDevice(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(f.device("0000:c5:00.0")); err != nil {
		t.Fatal(err)
	}
	f.addDevice("0000:c5:00.0", "0x1002", "0x030000", "vfio-pci", map[string]string{
		"mem_info_gtt_total": "133143986176\n",
	})

	r := f.collector().Collect()
	if r.GTTTotalBytes != nil {
		t.Error("counters were read from a device not driven by amdgpu")
	}
	messagesMention(t, r, "no amdgpu")
}

// TestCollectRefusesToChooseBetweenTwoGPUs: with two amdgpu devices there is
// no validated rule for which one the policy is about, and reading one of them
// silently would attribute one device's counters to the node.
func TestCollectRefusesToChooseBetweenTwoGPUs(t *testing.T) {
	f := newFixture(t)
	f.addDevice("0000:03:00.0", "0x1002", "0x030000", "amdgpu", map[string]string{
		"mem_info_gtt_total": "1024\n",
	})

	r := f.collector().Collect()
	if r.GTTTotalBytes != nil || r.GPUBusyPercent != nil || r.RatedSCLKMHz != nil {
		t.Error("counters were read despite two candidate GPUs")
	}
	messagesMention(t, r, "pending hardware validation")
}

func TestParseSCLKLadder(t *testing.T) {
	testCases := []struct {
		name        string
		ladder      string
		wantCurrent *int32
		wantRated   int32
		wantErr     string
		wantNoStar  bool
	}{
		{
			name:        "documented format with a starred level",
			ladder:      "0: 200Mhz\n1: 1100Mhz\n2: 2900Mhz *\n",
			wantCurrent: i32(2900),
			wantRated:   2900,
		},
		{
			name:        "the starred level is not the top",
			ladder:      "0: 200Mhz *\n1: 2900Mhz\n",
			wantCurrent: i32(200),
			wantRated:   2900,
		},
		{
			name:      "no starred level leaves current nil but keeps rated",
			ladder:    "0: 200Mhz\n1: 2900Mhz\n",
			wantRated: 2900,
		},
		{
			name:        "the MHz spelling parses too",
			ladder:      "0: 200MHz\n1: 2900MHz *\n",
			wantCurrent: i32(2900),
			wantRated:   2900,
		},
		{
			name:    "one unrecognised line fails the whole reading",
			ladder:  "0: 200Mhz\n1: fine-grained\n2: 2900Mhz *\n",
			wantErr: "does not match",
		},
		{
			name:    "an empty file is an error, not a zero clock",
			ladder:  "\n",
			wantErr: "empty",
		},
		{
			name:    "an all-zero ladder cannot be a rated clock",
			ladder:  "0: 0Mhz *\n",
			wantErr: "cannot be a rated clock",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "pp_dpm_sclk")
			if err := os.WriteFile(path, []byte(tc.ladder), 0o644); err != nil {
				t.Fatal(err)
			}

			current, rated, err := parseSCLKLadder(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSCLKLadder: %v", err)
			}
			if rated != tc.wantRated {
				t.Errorf("rated = %d, want %d", rated, tc.wantRated)
			}
			switch {
			case tc.wantCurrent == nil && current != nil:
				t.Errorf("current = %d, want nil", *current)
			case tc.wantCurrent != nil && current == nil:
				t.Errorf("current = nil, want %d", *tc.wantCurrent)
			case tc.wantCurrent != nil && *current != *tc.wantCurrent:
				t.Errorf("current = %d, want %d", *current, *tc.wantCurrent)
			}
		})
	}
}

func i32(v int32) *int32 { return &v }

// TestCollectReadsModuleParams pins the reading that makes a modprobe.d-
// configured node judgeable at all. On the AMD Ryzen AI Developer Platform
// ttm.pages_limit is set by /etc/modprobe.d/ttm.conf and appears nowhere on
// the kernel command line, so the running value is the only observation of it
// that exists.
func TestCollectReadsModuleParams(t *testing.T) {
	f := newFixture(t)
	r := f.collector().Collect()

	if r.ModuleParams == nil {
		t.Fatalf("module parameters were not read at all: %v", r.Messages)
	}
	for key, want := range map[string]string{
		"ttm.pages_limit": "32505856",
		"amdgpu.gttsize":  "126976",
	} {
		if got, ok := r.ModuleParams[key]; !ok {
			t.Errorf("%s missing from module parameters %v", key, r.ModuleParams)
		} else if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestUnreadableModuleParamIsAbsentNotEmpty is the real hardware case:
// /sys/module/amdgpu/parameters/* is mode 0400 and this agent is deliberately
// unprivileged, so amdgpu's parameters are routinely unreadable. An unreadable
// parameter must be ABSENT, because the decision packages read absence as "not
// observed" and an empty string would compare unequal to any expected value
// and so read as drift — accusing a node on the strength of a permission
// error.
func TestUnreadableModuleParamIsAbsentNotEmpty(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.sysRoot, "module", "amdgpu", "parameters", "gttsize")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file")
	}

	r := f.collector().Collect()

	if _, present := r.ModuleParams["amdgpu.gttsize"]; present {
		t.Errorf("an unreadable parameter was published as %q; it must be absent",
			r.ModuleParams["amdgpu.gttsize"])
	}
	// The readable module is unaffected: absence is per-parameter, not
	// all-or-nothing.
	if got := r.ModuleParams["ttm.pages_limit"]; got != "32505856" {
		t.Errorf("ttm.pages_limit = %q, want it still readable", got)
	}
}

// TestNoModuleDirectoriesYieldsNilNotEmptyMap keeps the nil/empty distinction
// the whole three-valued chain rests on. A node where neither module is loaded
// must produce nil, not an empty map, so the controller reads "nothing could
// be observed" rather than "observed, nothing set".
func TestNoModuleDirectoriesYieldsNilNotEmptyMap(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(filepath.Join(f.sysRoot, "module")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()

	if r.ModuleParams != nil {
		t.Errorf("ModuleParams = %v, want nil when no module directory is readable", r.ModuleParams)
	}
	if len(r.Messages) == 0 {
		t.Error("no message explained why the module parameters are absent")
	}
}

// TestCollectReadsTheKernelRelease pins the reading every other one is
// interpreted against: on the physical unit the installed kernels boot to
// different memory configurations (halo-operator#20).
func TestCollectReadsTheKernelRelease(t *testing.T) {
	r := newFixture(t).collector().Collect()
	if r.KernelRelease == nil || *r.KernelRelease != fixtureRelease {
		t.Fatalf("KernelRelease = %v, want %q", r.KernelRelease, fixtureRelease)
	}
}

// An unreadable or empty release is absent with a reason — never "" — so a
// consumer comparing against an expected kernel sees "not observed", not a
// kernel whose name is the empty string.
func TestCollectKernelReleaseAbsentNotEmpty(t *testing.T) {
	for name, spoil := range map[string]func(*fixture){
		"missing": func(f *fixture) {
			if err := os.Remove(filepath.Join(f.procRoot, "sys", "kernel", "osrelease")); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(f *fixture) {
			f.writeFile(filepath.Join(f.procRoot, "sys", "kernel", "osrelease"), "\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			spoil(f)
			r := f.collector().Collect()
			if r.KernelRelease != nil {
				t.Errorf("KernelRelease = %q, want absent", *r.KernelRelease)
			}
			messagesMention(t, r, "kernel release")
			if r.GTTTotalBytes == nil {
				t.Error("an unreadable kernel release poisoned the GTT reading")
			}
		})
	}
}
