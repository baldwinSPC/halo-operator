// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"fmt"
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
	etcRoot  string
	espRoot  string
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
		etcRoot:  filepath.Join(root, "etc"),
		espRoot:  filepath.Join(root, "efi"),
	}
	f.writeFile(filepath.Join(f.procRoot, "cmdline"), fixtureCmdline)
	f.writeFile(filepath.Join(f.procRoot, "sys", "kernel", "osrelease"), fixtureRelease+"\n")
	// A healthy node is readable in EVERY source the collector reads, and the
	// boot-hold sources are two of them now. Leaving them out would make this
	// fixture sample the HOST's /etc and /efi, which is how it first broke.
	f.writeFile(filepath.Join(f.etcRoot, "kernel", "cmdline"), "root=/dev/nvme0n1p2 quiet\n")
	f.writeFile(filepath.Join(f.espRoot, "loader", "entries", "fixture.conf"),
		"title Fixture\noptions     root=/dev/nvme0n1p2 quiet\n")
	f.addDevice("0000:c5:00.0", "0x1002", "0x030000", "amdgpu", map[string]string{
		"mem_info_gtt_total": "133143986176\n", // 124 GiB
		"gpu_busy_percent":   "97\n",
		"pp_dpm_sclk":        fixtureLadder,
	})
	f.addModuleParams("ttm", map[string]string{"pages_limit": "32505856\n"})
	f.addModuleParams("amdgpu", map[string]string{"gttsize": "126976\n"})
	// The device's DRM minors and KFD's topology, shaped as on amd-halo: a
	// render node under the PCI device, and two KFD agents, the CPU (target
	// 0) and the gfx1151 GPU.
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "drm", "renderD128", "dev"), "226:128\n")
	f.addKFDNode("0", 0)
	f.addKFDNode("1", 110501)
	// The inbox amdgpu declares no taint, and the firmware is the unit's own,
	// read 2026-10-06.
	f.writeFile(filepath.Join(f.sysRoot, "module", "amdgpu", "taint"), "\n")
	f.writeFile(filepath.Join(f.etcRoot, "modprobe.d", "ttm.conf"), "options ttm pages_limit=32505856\n")
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "vbios_version"), "113-STRXLGEN-001\n")
	for name, version := range fixtureFirmware {
		f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "fw_version", name+"_fw_version"), version+"\n")
	}
	return f
}

// fixtureFirmware is amd-halo's fw_version directory, 2026-10-06.
var fixtureFirmware = map[string]string{
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
}

// addKFDNode writes one KFD topology node's properties file.
func (f *fixture) addKFDNode(name string, gfxTargetVersion int) {
	f.t.Helper()
	cores, simds := 0, 80
	if gfxTargetVersion == 0 {
		cores, simds = 32, 0
	}
	f.writeFile(filepath.Join(f.kfdNodes(), name, "properties"), fmt.Sprintf(
		"cpu_cores_count %d\nsimd_count %d\ngfx_target_version %d\n", cores, simds, gfxTargetVersion))
}

func (f *fixture) kfdNodes() string {
	return filepath.Join(f.sysRoot, "class", "kfd", "kfd", "topology", "nodes")
}

// addModuleParams creates /sys/module/<module>/parameters/<name> files.
func (f *fixture) addModuleParams(module string, params map[string]string) {
	f.t.Helper()
	for name, content := range params {
		f.writeFile(filepath.Join(f.sysRoot, "module", module, "parameters", name), content)
	}
}

func (f *fixture) collector() Collector {
	return Collector{ProcRoot: f.procRoot, SysRoot: f.sysRoot, EtcRoot: f.etcRoot, ESPRoot: f.espRoot}
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
	if r.DRMRenderNode == nil || *r.DRMRenderNode != "renderD128" {
		t.Errorf("DRMRenderNode = %v, want renderD128", r.DRMRenderNode)
	}
	if r.GFXTargetVersion == nil || *r.GFXTargetVersion != 110501 {
		t.Errorf("GFXTargetVersion = %v, want 110501", r.GFXTargetVersion)
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
	if r.DRMRenderNode != nil || r.GFXTargetVersion != nil {
		t.Error("backend readings are set with no amdgpu present; they describe the device, so they must be unread")
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

// A drm directory that lists the card but no render node is a reading: the
// kernel registered no render node for this GPU.
func TestCollectNoRenderNodeIsEmptyNotNil(t *testing.T) {
	f := newFixture(t)
	drm := filepath.Join(f.device("0000:c5:00.0"), "drm")
	if err := os.RemoveAll(filepath.Join(drm, "renderD128")); err != nil {
		t.Fatal(err)
	}
	f.writeFile(filepath.Join(drm, "card0", "dev"), "226:0\n")

	r := f.collector().Collect()
	if r.DRMRenderNode == nil {
		t.Fatal("DRMRenderNode is nil for a drm directory that was read; want present and empty")
	}
	if *r.DRMRenderNode != "" {
		t.Errorf("DRMRenderNode = %q, want empty", *r.DRMRenderNode)
	}
}

// A missing drm directory is unread, never "no render node": calling a node
// unable to run Vulkan on the strength of a sysfs layout this reader has not
// seen would be an accusation nothing measured.
func TestCollectMissingDRMDirectoryIsNil(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(filepath.Join(f.device("0000:c5:00.0"), "drm")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.DRMRenderNode != nil {
		t.Errorf("DRMRenderNode = %q for a missing drm directory, want nil", *r.DRMRenderNode)
	}
	messagesMention(t, r, "render node")
	if r.GFXTargetVersion == nil {
		t.Error("the KFD reading went unread because the render node did")
	}
}

func TestCollectTwoRenderNodesIsNil(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.device("0000:c5:00.0"), "drm", "renderD129", "dev"), "226:129\n")

	r := f.collector().Collect()
	if r.DRMRenderNode != nil {
		t.Errorf("DRMRenderNode = %q with two render nodes, want nil", *r.DRMRenderNode)
	}
	messagesMention(t, r, "renderD128, renderD129")
}

// A kernel with no KFD at all is an observation: the node cannot run ROCm.
func TestCollectNoKFDIsZero(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(filepath.Join(f.sysRoot, "class", "kfd")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.GFXTargetVersion == nil || *r.GFXTargetVersion != 0 {
		t.Errorf("GFXTargetVersion = %v with no KFD, want a present 0", r.GFXTargetVersion)
	}
}

func TestCollectKFDWithOnlyTheCPUIsZero(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(filepath.Join(f.kfdNodes(), "1")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.GFXTargetVersion == nil || *r.GFXTargetVersion != 0 {
		t.Errorf("GFXTargetVersion = %v with only a CPU agent, want a present 0", r.GFXTargetVersion)
	}
}

// An unreadable topology is unread, not "no KFD". A regular file where the
// directory should be fails with ENOTDIR whatever uid the suite runs as; a
// chmod fixture would be inert under root, and CI runs as root.
func TestCollectUnreadableKFDIsNil(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(f.kfdNodes()); err != nil {
		t.Fatal(err)
	}
	f.writeFile(f.kfdNodes(), "not a directory\n")

	r := f.collector().Collect()
	if r.GFXTargetVersion != nil {
		t.Errorf("GFXTargetVersion = %d for an unreadable topology, want nil", *r.GFXTargetVersion)
	}
	messagesMention(t, r, "kfd")
}

func TestCollectGarbageKFDPropertyIsNil(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.kfdNodes(), "1", "properties"), "simd_count 80\ngfx_target_version gfx1151\n")

	r := f.collector().Collect()
	if r.GFXTargetVersion != nil {
		t.Errorf("GFXTargetVersion = %d for a non-numeric property, want nil", *r.GFXTargetVersion)
	}
	messagesMention(t, r, "gfx_target_version")
}

func TestCollectKFDNodeWithoutTheTargetIsNil(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.kfdNodes(), "1", "properties"), "simd_count 80\n")

	r := f.collector().Collect()
	if r.GFXTargetVersion != nil {
		t.Errorf("GFXTargetVersion = %d for a node with no target property, want nil", *r.GFXTargetVersion)
	}
	messagesMention(t, r, "no gfx_target_version")
}

// One amdgpu device on the PCI bus and two GPU agents in KFD disagree, and
// choosing one would attribute another device's target to this one.
func TestCollectRefusesToChooseBetweenTwoKFDAgents(t *testing.T) {
	f := newFixture(t)
	f.addKFDNode("2", 110000)

	r := f.collector().Collect()
	if r.GFXTargetVersion != nil {
		t.Errorf("GFXTargetVersion = %d with two GPU agents, want nil", *r.GFXTargetVersion)
	}
	messagesMention(t, r, "2 GPU agents")
}

func TestCollectReadsTheStack(t *testing.T) {
	r := newFixture(t).collector().Collect()
	if r.AMDGPUTaint == nil || *r.AMDGPUTaint != "" {
		t.Errorf("AMDGPUTaint = %v, want a present empty taint for the inbox driver", r.AMDGPUTaint)
	}
	if r.TTMModule == nil || *r.TTMModule != "ttm" {
		t.Errorf("TTMModule = %v, want ttm", r.TTMModule)
	}
	if r.VBIOSVersion == nil || *r.VBIOSVersion != "113-STRXLGEN-001" {
		t.Errorf("VBIOSVersion = %v, want 113-STRXLGEN-001", r.VBIOSVersion)
	}
	if len(r.FirmwareVersions) != len(fixtureFirmware) || r.FirmwareVersions["mes"] != "0x00000088" {
		t.Errorf("FirmwareVersions = %v, want every fixture entry keyed without the _fw_version suffix", r.FirmwareVersions)
	}
}

// amdgpu-dkms marks its module out-of-tree and ships amdttm, whose pages_limit
// then sizes GTT. Both must be read, and amdttm must win over the kernel's ttm.
func TestCollectSeesAmdgpuDKMS(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.sysRoot, "module", "amdgpu", "taint"), "O\n")
	f.addModuleParams("amdttm", map[string]string{"pages_limit": "24692260\n"})

	r := f.collector().Collect()
	if r.AMDGPUTaint == nil || *r.AMDGPUTaint != "O" {
		t.Errorf("AMDGPUTaint = %v, want O", r.AMDGPUTaint)
	}
	if r.TTMModule == nil || *r.TTMModule != "amdttm" {
		t.Errorf("TTMModule = %v, want amdttm to win over ttm", r.TTMModule)
	}
	if r.ModuleParams["amdttm.pages_limit"] != "24692260" {
		t.Errorf("amdttm.pages_limit = %q, want it read", r.ModuleParams["amdttm.pages_limit"])
	}
}

func TestCollectUnreadableStackIsNil(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		filepath.Join(f.sysRoot, "module", "amdgpu", "taint"),
		filepath.Join(f.device("0000:c5:00.0"), "vbios_version"),
	} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(f.device("0000:c5:00.0"), "fw_version")); err != nil {
		t.Fatal(err)
	}

	r := f.collector().Collect()
	if r.AMDGPUTaint != nil || r.VBIOSVersion != nil || r.FirmwareVersions != nil {
		t.Errorf("unreadable stack readings arrived as values: taint %v vbios %v firmware %v",
			r.AMDGPUTaint, r.VBIOSVersion, r.FirmwareVersions)
	}
	for _, m := range []string{"taint", "vbios", "firmware"} {
		messagesMention(t, r, m)
	}
}

// The DKMS-only modules are absent on every inbox host; that is not news.
func TestCollectSaysNothingAboutAbsentDKMSModules(t *testing.T) {
	r := newFixture(t).collector().Collect()
	for _, m := range r.Messages {
		if strings.Contains(m, "amdttm") || strings.Contains(m, "amd_ttm") {
			t.Errorf("an inbox host produced a message about a DKMS-only module: %q", m)
		}
	}
}

// The declared options for the governed modules are carried verbatim, in
// file order; other modules and comments are not.
func TestCollectReadsDeclaredModprobeOptions(t *testing.T) {
	f := newFixture(t)
	f.writeFile(filepath.Join(f.etcRoot, "modprobe.d", "00-amdgpu.conf"),
		"# a comment\noptions amdgpu gttsize=126976\nblacklist nouveau\noptions snd_hda_intel power_save=0\n")
	f.writeFile(filepath.Join(f.etcRoot, "modprobe.d", "README"), "options ttm pages_limit=1\n")

	r := f.collector().Collect()
	want := []string{"options amdgpu gttsize=126976", "options ttm pages_limit=32505856"}
	if len(r.ModprobeOptions) != len(want) {
		t.Fatalf("ModprobeOptions = %q, want %q", r.ModprobeOptions, want)
	}
	for i := range want {
		if r.ModprobeOptions[i] != want[i] {
			t.Errorf("ModprobeOptions[%d] = %q, want %q", i, r.ModprobeOptions[i], want[i])
		}
	}
}

