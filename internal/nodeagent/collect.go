// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// Package nodeagent is the one part of this operator that touches a node. It
// reads the kernel command line and the amdgpu sysfs counters and publishes
// what it found as a HaloNodeObservation; every decision about what the
// readings mean stays in internal/memorypolicy and internal/healthchecks,
// which never see a file.
//
// The package is read-only by construction: it opens files for reading and
// writes nothing anywhere on the host, ever. Applying kernel arguments is the
// Glimmer signed-task path, not this operator.
//
// Two disciplines govern every reader here:
//
//   - A reading that could not be taken is nil, never zero and never an empty
//     string. Zero is a real measurement ("the GPU is 0% busy"); nil is the
//     absence of one. Each nil is accompanied by a message saying why, for
//     humans; the controller acts only on the nil.
//
//   - Only kernel-documented amdgpu ABI is read (Documentation/gpu/amdgpu and
//     Documentation/ABI): mem_info_gtt_total, gpu_busy_percent, pp_dpm_sclk,
//     and the generic PCI attributes vendor, class and driver. What is NOT yet
//     validated is how the first physical gfx1151 unit presents them — device
//     enumeration, ladder shape, counter population (amd-smi is documented
//     blind on APUs; sysfs is expected 1:1 but expected is not observed).
//     Anything that deviates from the documented format is reported as nil
//     with a message, never guessed at. See "Hardware validation pending" in
//     the README.
package nodeagent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Reading is everything one collection pass produced. Absent fields are nil;
// Messages says why, one entry per absence, for the human reading the
// published observation.
type Reading struct {
	// KernelCmdline is the raw contents of <procRoot>/cmdline, whitespace
	// trimmed at the end only.
	KernelCmdline *string
	// GTTTotalBytes is mem_info_gtt_total for the discovered amdgpu device.
	GTTTotalBytes *int64
	// GPUBusyPercent is gpu_busy_percent for the discovered amdgpu device.
	GPUBusyPercent *int32
	// SCLKMHz is the currently selected level of the pp_dpm_sclk ladder.
	SCLKMHz *int32
	// RatedSCLKMHz is the top of the pp_dpm_sclk ladder.
	RatedSCLKMHz *int32
	// Messages explains every nil above.
	Messages []string
}

// Collector reads one node. Roots are parameterized so tests run against a
// fixture tree and the DaemonSet runs against read-only host mounts.
type Collector struct {
	// ProcRoot is where the host's /proc is visible, "/proc" by default.
	// Only <ProcRoot>/cmdline is ever read.
	ProcRoot string
	// SysRoot is where the host's /sys is visible, "/sys" by default.
	SysRoot string
}

// Collect performs one read of the node. It never fails as a whole: each
// reading that could not be taken is nil with a message, and a node with no
// AMD GPU at all simply produces a Reading with every device counter nil.
func (c Collector) Collect() Reading {
	var r Reading

	procRoot := c.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	sysRoot := c.SysRoot
	if sysRoot == "" {
		sysRoot = "/sys"
	}

	cmdlinePath := filepath.Join(procRoot, "cmdline")
	if raw, err := os.ReadFile(cmdlinePath); err != nil {
		r.Messages = append(r.Messages, fmt.Sprintf("cmdline: %v", err))
	} else {
		// /proc/cmdline ends in a newline; the trailing whitespace is not part
		// of the command line. Leading/inner whitespace is preserved — it is
		// the parser's business, not the reader's.
		s := strings.TrimRight(string(raw), "\n\r \t")
		r.KernelCmdline = &s
	}

	dev, msg := discoverAMDGPU(sysRoot)
	if dev == "" {
		r.Messages = append(r.Messages, msg)
		return r
	}

	if v, err := readSysfsInt64(filepath.Join(dev, "mem_info_gtt_total")); err != nil {
		r.Messages = append(r.Messages, fmt.Sprintf("gtt: %v", err))
	} else {
		r.GTTTotalBytes = &v
	}

	if v, err := readSysfsInt64(filepath.Join(dev, "gpu_busy_percent")); err != nil {
		r.Messages = append(r.Messages, fmt.Sprintf("busy: %v", err))
	} else if v > 100 {
		// The documented range is 0..100. A value outside it means the file
		// does not hold what this reader thinks it holds, and passing it on
		// would launder a misreading into a measurement.
		r.Messages = append(r.Messages, fmt.Sprintf(
			"busy: gpu_busy_percent read %d, outside the documented 0..100 range", v))
	} else {
		busy := int32(v)
		r.GPUBusyPercent = &busy
	}

	current, rated, err := parseSCLKLadder(filepath.Join(dev, "pp_dpm_sclk"))
	if err != nil {
		r.Messages = append(r.Messages, fmt.Sprintf("sclk: %v", err))
	} else {
		r.RatedSCLKMHz = &rated
		if current == nil {
			r.Messages = append(r.Messages,
				"sclk: no level in pp_dpm_sclk is marked current (*), so the current clock is unknown")
		} else {
			r.SCLKMHz = current
		}
	}

	// PermissionFaults and QueueCreateFailures are deliberately not collected.
	// They come from the kernel log, and the line formats to match are
	// hardware facts pending a physical gfx1151 unit; an unvalidated pattern
	// that matches nothing would count zero faults, which reads as a healthy
	// node. Absent readings keep those probes honestly NotObserved.

	return r
}

// discoverAMDGPU finds the single amdgpu-driven GPU on the node and returns
// its PCI device directory. It returns "" and a reason when there is not
// exactly one, because with zero there is nothing to read and with several,
// choosing one silently would attribute one device's counters to the node as
// a whole. Multi-GPU selection is deliberately unimplemented until a policy
// can be validated against real hardware.
//
// Discovery uses only generic, documented PCI sysfs ABI: vendor 0x1002 (AMD),
// class 0x03xxxx (display) or 0x12xxxx (processing accelerator), and the
// driver symlink resolving to amdgpu. The driver check is what ties the
// device to the amdgpu counter files read above — a vfio-bound or driverless
// AMD device has none of them.
func discoverAMDGPU(sysRoot string) (dir string, reason string) {
	pciGlob := filepath.Join(sysRoot, "bus", "pci", "devices", "*")
	pciDirs, err := filepath.Glob(pciGlob)
	if err != nil || len(pciDirs) == 0 {
		return "", fmt.Sprintf("gpu: no PCI devices visible under %s", pciGlob)
	}
	sort.Strings(pciDirs)

	var candidates []string
	for _, d := range pciDirs {
		vendor, err := os.ReadFile(filepath.Join(d, "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != "0x1002" {
			continue
		}
		class, err := os.ReadFile(filepath.Join(d, "class"))
		if err != nil {
			continue
		}
		cls := strings.TrimSpace(string(class))
		if !strings.HasPrefix(cls, "0x03") && !strings.HasPrefix(cls, "0x12") {
			continue
		}
		driver, err := os.Readlink(filepath.Join(d, "driver"))
		if err != nil || filepath.Base(driver) != "amdgpu" {
			continue
		}
		candidates = append(candidates, d)
	}

	switch len(candidates) {
	case 1:
		return candidates[0], ""
	case 0:
		return "", "gpu: no amdgpu-driven AMD GPU found on the PCI bus"
	default:
		return "", fmt.Sprintf(
			"gpu: %d amdgpu devices found; device selection on multi-GPU nodes is pending hardware validation, so no counters were read",
			len(candidates))
	}
}

// readSysfsInt64 reads a sysfs attribute holding one non-negative integer.
func readSysfsInt64(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s does not hold a decimal integer: %v", filepath.Base(path), err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s read %d, which is negative", filepath.Base(path), v)
	}
	return v, nil
}

// sclkLine matches one level of the documented pp_dpm ladder format,
// "<index>: <freq>Mhz", with the currently selected level marked by a
// trailing asterisk. Case-insensitive on the unit because the kernel has
// printed both spellings across versions.
var sclkLine = regexp.MustCompile(`^\d+\s*:\s*(\d+)\s*[Mm][Hh]z\s*(\*)?$`)

// parseSCLKLadder reads pp_dpm_sclk and returns the currently selected clock
// (nil when no level is starred) and the top of the ladder.
//
// The parser is deliberately strict: one line it does not recognise fails the
// whole reading. A partial parse could report a plausible-but-wrong rated
// clock, and the clock-under-load probe would then judge a healthy node
// against a fiction. Format drift must surface as NotObserved, not as data.
func parseSCLKLadder(path string) (current *int32, rated int32, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var top int64
	seen := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := sclkLine.FindStringSubmatch(line)
		if m == nil {
			return nil, 0, fmt.Errorf(
				"pp_dpm_sclk line %q does not match the documented ladder format; parsing the whole file was abandoned rather than guessed at", truncate(line, 80))
		}
		mhz, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("pp_dpm_sclk level %q overflows: %v", truncate(line, 80), err)
		}
		if m[2] == "*" {
			c := int32(mhz)
			current = &c
		}
		if mhz > top {
			top = mhz
		}
		seen++
	}
	if seen == 0 {
		return nil, 0, fmt.Errorf("pp_dpm_sclk is empty")
	}
	if top <= 0 {
		return nil, 0, fmt.Errorf("pp_dpm_sclk's top level is %d MHz, which cannot be a rated clock", top)
	}
	return current, int32(top), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
