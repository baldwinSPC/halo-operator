// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nodeagent

import (
	"time"

	"golang.org/x/sys/unix"
)

// SyslogReader reads the ring buffer with syslog(2). With
// kernel.dmesg_restrict=1, as on amd-halo, that needs CAP_SYSLOG; it needs no
// /dev/kmsg mount, which an unprivileged container's device policy would
// refuse anyway.
type SyslogReader struct{}

// Read returns the whole ring buffer.
func (SyslogReader) Read() ([]byte, error) {
	size, err := unix.Klogctl(unix.SYSLOG_ACTION_SIZE_BUFFER, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	n, err := unix.Klogctl(unix.SYSLOG_ACTION_READ_ALL, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Now is CLOCK_MONOTONIC, which, like printk's clock, counts from boot and
// does not advance during suspend.
func (SyslogReader) Now() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return time.Duration(ts.Nano()), nil
}
