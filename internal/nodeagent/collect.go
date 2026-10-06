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
//     the generic PCI attributes vendor, class and driver, the device's DRM
//     minors, and KFD's topology properties. What is NOT yet
//     validated is how the first physical gfx1151 unit presents them — device
//     enumeration, ladder shape, counter population (amd-smi is documented
//     blind on APUs; sysfs is expected 1:1 but expected is not observed).
//     Anything that deviates from the documented format is reported as nil
//     with a message, never guessed at. See "Hardware validation pending" in
//     the README.
package nodeagent

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
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
	// KernelRelease is <procRoot>/sys/kernel/osrelease, trimmed.
	KernelRelease *string
	// ModuleParams is the running value of each readable parameter of the
	// modules in moduleParamModules, keyed as the kernel command line would
	// name it ("ttm.pages_limit"). Nil when no module directory could be read
	// at all; a key is absent when that one parameter could not be read.
	ModuleParams map[string]string
	// GTTTotalBytes is mem_info_gtt_total for the discovered amdgpu device.
	GTTTotalBytes *int64
	// GPUBusyPercent is gpu_busy_percent for the discovered amdgpu device.
	GPUBusyPercent *int32
	// SCLKMHz is the currently selected level of the pp_dpm_sclk ladder.
	SCLKMHz *int32
	// RatedSCLKMHz is the top of the pp_dpm_sclk ladder.
	RatedSCLKMHz *int32
	// DRMRenderNode is the render node the kernel registered for the device,
	// "" when its drm directory lists none.
	DRMRenderNode *string
	// GFXTargetVersion is KFD's gfx_target_version for the GPU agent, 0 when
	// KFD lists none or is absent.
	GFXTargetVersion *int32
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

	releasePath := filepath.Join(procRoot, "sys", "kernel", "osrelease")
	if raw, err := os.ReadFile(releasePath); err != nil {
		r.Messages = append(r.Messages, fmt.Sprintf("kernel release: %v", err))
	} else if rel := strings.TrimSpace(string(raw)); rel == "" {
		// An empty release is not a kernel version; passing it on would
		// report "read fine, it is the empty string" for a file that should
		// never be empty.
		r.Messages = append(r.Messages, "kernel release: osrelease is empty")
	} else {
		r.KernelRelease = &rel
	}

	params, paramMsgs := readModuleParams(sysRoot)
	r.ModuleParams = params
	r.Messages = append(r.Messages, paramMsgs...)

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

	if node, msg := readRenderNode(dev); node == nil {
		r.Messages = append(r.Messages, msg)
	} else {
		r.DRMRenderNode = node
	}

	if target, msg := readKFDTarget(sysRoot); target == nil {
		r.Messages = append(r.Messages, msg)
	} else {
		r.GFXTargetVersion = target
	}

	// PermissionFaults and QueueCreateFailures are deliberately not collected.
	// They come from the kernel log, and the line formats to match are
	// hardware facts pending a physical gfx1151 unit; an unvalidated pattern
	// that matches nothing would count zero faults, which reads as a healthy
	// node. Absent readings keep those probes honestly NotObserved.

	return r
}

// moduleParamModules are the modules whose running parameters govern unified
// memory on this hardware, and so the only ones read. The list is fixed rather
// than derived from the policy because the agent does not see policies — and
// because an agent that read an arbitrary caller-named path under /sys would
// be a different, larger thing than this one.
var moduleParamModules = []string{"amdgpu", "ttm"}

// readModuleParams reads the running value of every readable parameter of
// moduleParamModules, keyed as the kernel command line would name it.
//
// This exists because the command line is not the only way these parameters
// get set. The AMD Ryzen AI Developer Platform ships a ttm-config.service that
// writes /etc/modprobe.d/ttm.conf, so on a correctly configured unit
// ttm.pages_limit is set and yet appears nowhere on /proc/cmdline. A policy
// checked against the command line alone calls that node drifted forever.
//
// The running value is read rather than modprobe.d parsed, for the same reason
// the GTT ceiling is read rather than computed: it is what is actually in
// effect, and it needs no parser for a config-file format with includes,
// aliases and precedence rules.
//
// Unreadable parameters are simply absent, which the decision packages treat
// as "not observed". That is routine rather than exceptional here:
// /sys/module/amdgpu/parameters/* is mode 0400 on this platform and this agent
// is deliberately unprivileged, so amdgpu's parameters are expected to be
// missing. A nil map means not one module directory could be read.
func readModuleParams(sysRoot string) (map[string]string, []string) {
	var (
		params   map[string]string
		messages []string
	)
	for _, module := range moduleParamModules {
		dir := filepath.Join(sysRoot, "module", module, "parameters")
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A module that is not loaded has no directory. That is a fact
			// about the node, not a failure of the agent, so it is reported
			// as a message and leaves every parameter of that module absent.
			messages = append(messages, fmt.Sprintf("module %s: %v", module, err))
			continue
		}
		if params == nil {
			params = map[string]string{}
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				// The common case, and not worth a message per parameter:
				// most of these files are mode 0400 and there are dozens.
				continue
			}
			value := strings.TrimSpace(string(raw))
			if strings.ContainsAny(value, "\n\r") {
				// A multi-line parameter is not something a command-line
				// comparison can mean anything about.
				continue
			}
			params[module+"."+entry.Name()] = value
		}
	}
	return params, messages
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

// readRenderNode names the DRM render node the kernel registered for the
// device. DRM registers each minor as a child of its parent device, so the
// device's own drm directory lists them (card0 and renderD128 on a single-GPU
// node), and seeing that the render node exists needs no /dev mount.
//
// A missing drm directory is reported as unread, not as "no render node". An
// amdgpu-bound device without one is not a layout this reader has seen, and
// calling the node unable to run Vulkan on the strength of a layout assumption
// would be an accusation nothing measured.
func readRenderNode(dev string) (*string, string) {
	entries, err := os.ReadDir(filepath.Join(dev, "drm"))
	if err != nil {
		return nil, fmt.Sprintf("render node: %v", err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		none := ""
		return &none, ""
	case 1:
		return &found[0], ""
	default:
		return nil, fmt.Sprintf("render node: the device lists %d render nodes (%s) where one was expected",
			len(found), strings.Join(found, ", "))
	}
}

// readKFDTarget reads the gfx_target_version of the GPU compute agent in KFD's
// topology. KFD lists one topology node per agent, and the CPU is one of them
// with gfx_target_version 0; on amd-halo node 0 is the CPU and node 1 the GPU
// at 110501 (gfx1151). Each properties file holds one "name value" pair per
// line.
//
// Only an absent topology directory is a reading of zero: that is a kernel
// with no KFD, which cannot run ROCm. Every other failure is unread. The
// caller has already found exactly one amdgpu device, so more than one GPU
// agent means the topology and the PCI bus disagree, and no choice is made.
func readKFDTarget(sysRoot string) (*int32, string) {
	dir := filepath.Join(sysRoot, "class", "kfd", "kfd", "topology", "nodes")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		zero := int32(0)
		return &zero, ""
	}
	if err != nil {
		return nil, fmt.Sprintf("kfd: %v", err)
	}

	var targets []int64
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "properties"))
		if err != nil {
			return nil, fmt.Sprintf("kfd: %v", err)
		}
		v, err := kfdProperty(string(raw), "gfx_target_version")
		if err != nil {
			return nil, fmt.Sprintf("kfd topology node %s: %v", e.Name(), err)
		}
		if v > 0 {
			targets = append(targets, v)
		}
	}

	switch len(targets) {
	case 0:
		zero := int32(0)
		return &zero, ""
	case 1:
		if targets[0] > math.MaxInt32 {
			return nil, fmt.Sprintf("kfd: gfx_target_version %d is out of range", targets[0])
		}
		v := int32(targets[0])
		return &v, ""
	default:
		return nil, fmt.Sprintf(
			"kfd: topology lists %d GPU agents for one amdgpu device; no target was chosen", len(targets))
	}
}

// kfdProperty finds one named property in a KFD properties file.
func kfdProperty(properties, name string) (int64, error) {
	for _, line := range strings.Split(properties, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != name {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("%s %q is not a non-negative decimal integer", name, fields[1])
		}
		return v, nil
	}
	return 0, fmt.Errorf("properties carry no %s", name)
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
