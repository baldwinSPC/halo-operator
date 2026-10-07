// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"errors"
	"testing"
	"time"
)

// gfx1151Fault is one GPU page fault block as real gfx1151 units logged it in
// ROCm#6186 (issue body and comments, read 2026-10-06), with printk levels and
// timestamps as syslog(2) READ_ALL returns them. ROCm#6118 shows the same
// status, client and fields.
const gfx1151Fault = `<3>[  600.000001] amdgpu 0000:bd:00.0: amdgpu: [gfxhub] page fault (src_id:0 ring:153 vmid:8 pasid:2634)
<3>[  600.000002] amdgpu 0000:bd:00.0: amdgpu:   in process llama-server pid 4242 thread llama-server pid 4242)
<3>[  600.000003] amdgpu 0000:bd:00.0: amdgpu: GCVM_L2_PROTECTION_FAULT_STATUS:0x00800932
<3>[  600.000004] amdgpu 0000:bd:00.0: amdgpu: 	 Faulty UTCL2 client ID: CPF (0x4)
<3>[  600.000005] amdgpu 0000:bd:00.0: amdgpu: 	 MORE_FAULTS: 0x0
<3>[  600.000006] amdgpu 0000:bd:00.0: amdgpu: 	 WALKER_ERROR: 0x1
<3>[  600.000007] amdgpu 0000:bd:00.0: amdgpu: 	 PERMISSION_FAULTS: 0x3
<3>[  600.000008] amdgpu 0000:bd:00.0: amdgpu: 	 MAPPING_ERROR: 0x1
<3>[  600.000009] amdgpu 0000:bd:00.0: amdgpu: 	 RW: 0x0
`

// benignNPU is the amdxdna line amd-halo logs once per XRT enumeration:
// settled as benign in #12 (an unimplemented sensors query in in-tree
// amdxdna at 6.18). It must never enter a fault count.
const benignNPU = "<3>[  601.000000] amdxdna 0000:c3:00.1: [drm] *ERROR* aie2_get_info: Not supported request parameter 4\n"

func TestCountFaultsOnRealGfx1151Text(t *testing.T) {
	perm, queue := countFaults([]byte(gfx1151Fault+benignNPU), 700*time.Second, 10*time.Minute)
	if perm != 1 || queue != 1 {
		t.Errorf("permission %d, queue %d; want 1 and 1 for one real fault block", perm, queue)
	}
}

func TestCountFaultsIgnoresTheBenignNPULine(t *testing.T) {
	if perm, queue := countFaults([]byte(benignNPU), 700*time.Second, 10*time.Minute); perm != 0 || queue != 0 {
		t.Errorf("the amdxdna line settled as benign in #12 was counted: permission %d, queue %d", perm, queue)
	}
}

func TestCountFaultsOnlyWithinTheWindow(t *testing.T) {
	// The fault is at 600s; at 2000s a 10-minute window starts at 1400s.
	if perm, queue := countFaults([]byte(gfx1151Fault), 2000*time.Second, 10*time.Minute); perm != 0 || queue != 0 {
		t.Errorf("a fault outside the window was counted: permission %d, queue %d", perm, queue)
	}
}

func TestCountFaultsNeedsANonZeroPermissionField(t *testing.T) {
	clean := "<3>[ 600.000007] amdgpu 0000:c2:00.0: amdgpu: \t PERMISSION_FAULTS: 0x0\n"
	if perm, _ := countFaults([]byte(clean), 700*time.Second, 10*time.Minute); perm != 0 {
		t.Errorf("PERMISSION_FAULTS: 0x0 was counted as a permission fault")
	}
}

func TestCountFaultsSkipsLinesItCannotPlace(t *testing.T) {
	unstamped := "amdgpu 0000:c2:00.0: amdgpu: \t PERMISSION_FAULTS: 0x3\n"
	if perm, _ := countFaults([]byte(unstamped), 700*time.Second, 10*time.Minute); perm != 0 {
		t.Errorf("a line without a printk timestamp was counted")
	}
}

type fakeKernelLog struct {
	buf    string
	err    error
	now    time.Duration
	nowErr error
}

func (f fakeKernelLog) Read() ([]byte, error)       { return []byte(f.buf), f.err }
func (f fakeKernelLog) Now() (time.Duration, error) { return f.now, f.nowErr }

func TestCollectCountsKernelLogFaults(t *testing.T) {
	c := newFixture(t).collector()
	c.KernelLog = fakeKernelLog{buf: gfx1151Fault, now: 700 * time.Second}
	r := c.Collect()
	if r.PermissionFaults == nil || *r.PermissionFaults != 1 || r.QueueCreateFailures == nil || *r.QueueCreateFailures != 1 {
		t.Errorf("counts = %v/%v, want 1/1", r.PermissionFaults, r.QueueCreateFailures)
	}

	quiet := newFixture(t).collector()
	quiet.KernelLog = fakeKernelLog{buf: benignNPU, now: 700 * time.Second}
	r = quiet.Collect()
	if r.PermissionFaults == nil || *r.PermissionFaults != 0 {
		t.Errorf("a read log with no faults gave %v, want a present 0", r.PermissionFaults)
	}
}

// Without a reader, or with one that fails, the counts are absent: zero is the
// healthy answer, and an unread log must never produce it.
func TestCollectUnreadKernelLogIsNilNotZero(t *testing.T) {
	r := newFixture(t).collector().Collect()
	if r.PermissionFaults != nil || r.QueueCreateFailures != nil {
		t.Errorf("no reader configured, yet counts were %v/%v", r.PermissionFaults, r.QueueCreateFailures)
	}
	c := newFixture(t).collector()
	c.KernelLog = fakeKernelLog{err: errors.New("operation not permitted")}
	r = c.Collect()
	if r.PermissionFaults != nil || r.QueueCreateFailures != nil {
		t.Errorf("an unreadable log gave counts %v/%v, want nil", r.PermissionFaults, r.QueueCreateFailures)
	}
	messagesMention(t, r, "kernel log")
}
