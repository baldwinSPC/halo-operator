// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package nodeagent

import (
	"errors"
	"time"
)

// SyslogReader reads the kernel log, which exists only on Linux.
type SyslogReader struct{}

var errNotLinux = errors.New("the kernel log is read only on Linux")

// Read always fails off Linux.
func (SyslogReader) Read() ([]byte, error) { return nil, errNotLinux }

// Now always fails off Linux.
func (SyslogReader) Now() (time.Duration, error) { return 0, errNotLinux }
