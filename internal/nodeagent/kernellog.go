// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"time"
)

// KernelLogReader reads the kernel's log ring buffer and the clock its
// timestamps are on. An interface so the counting runs against fixtures.
type KernelLogReader interface {
	// Read returns the ring buffer as syslog(2) SYSLOG_ACTION_READ_ALL does:
	// one "<level>[seconds.micros] text" line per message.
	Read() ([]byte, error)
	// Now is the current time on the printk clock, as time since boot.
	Now() (time.Duration, error)
}

// DefaultKernelLogWindow is how far back a reading counts faults: a storm is
// many faults close together, and a fault from yesterday's run should not hold
// a node unhealthy today.
const DefaultKernelLogWindow = 10 * time.Minute

// The patterns count one GPU page fault each, matched against the exact
// strings amdgpu prints for gfx11.5 (gfx1151), and validated against the
// dmesg that real gfx1151 units logged in ROCm#6186 and ROCm#6118. Both
// reports show one and the same fault block:
//
//	amdgpu 0000:bd:00.0: amdgpu: [gfxhub] page fault (src_id:0 ring:153 vmid:8 pasid:2634)
//	amdgpu: GCVM_L2_PROTECTION_FAULT_STATUS:0x00800932
//	amdgpu: Faulty UTCL2 client ID: CPF (0x4)
//	amdgpu: WALKER_ERROR: 0x1
//	amdgpu: PERMISSION_FAULTS: 0x3
//	amdgpu: MAPPING_ERROR: 0x1
//
// The format strings are gmc_v11_0.c:144 ("[%s] page fault (src_id:...") and
// gfxhub_v11_5_0.c:87-103 (the status block), torvalds/linux at 2026-10-05.
//
//   - permissionFault counts status blocks whose PERMISSION_FAULTS field is
//     non-zero: ROCm#6186's storm.
//   - queueCreate counts faults whose UTCL2 client is CPF, the command
//     processor fetching a queue: the fault hsa_queue_create triggers in
//     ROCm#6118. A failed queue creation logs nothing of its own in the
//     kernel; this fault is how it shows.
//
// On these reports both probes fire on the same event, which matches AMD
// closing both issues with one cause (amdgpu-dkms on an APU; see #34). The
// kernel rate-limits these prints, so a count is a lower bound on a storm.
var (
	permissionFault = regexp.MustCompile(`amdgpu.*\bPERMISSION_FAULTS: 0x([0-9a-fA-F]+)`)
	queueCreate     = regexp.MustCompile(`amdgpu.*\bFaulty UTCL2 client ID: CPF\b`)
	printkStamp     = regexp.MustCompile(`^(?:<\d+>)?\[\s*(\d+)\.(\d+)\]`)
)

// countFaults counts the two fault signatures in the lines whose printk
// timestamp falls within window of now. A line without a timestamp cannot be
// placed in the window and is not counted.
func countFaults(buf []byte, now, window time.Duration) (permission, queue int32) {
	since := now - window
	sc := bufio.NewScanner(bytes.NewReader(buf))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m := printkStamp.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		secs, _ := strconv.ParseInt(m[1], 10, 64)
		frac, _ := strconv.ParseFloat("0."+m[2], 64)
		at := time.Duration(secs)*time.Second + time.Duration(frac*float64(time.Second))
		if at < since {
			continue
		}
		if p := permissionFault.FindStringSubmatch(line); p != nil {
			if v, err := strconv.ParseUint(p[1], 16, 64); err == nil && v != 0 {
				permission++
			}
		}
		if queueCreate.MatchString(line) {
			queue++
		}
	}
	return permission, queue
}
