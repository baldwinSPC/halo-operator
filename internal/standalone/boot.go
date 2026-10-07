// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

// Component names for the two boot checks, which run only in `halo check`.
const (
	BootCompletedComponent = "boot-completed"
	BootHoldComponent      = "boot-hold"
)

// DefaultBootGrace is how long a boot may still be "starting" before that is
// a finding rather than a boot in progress. On amd-halo a healthy boot reaches
// multi-user.target in about a minute; a stalled one sat in "starting" for 26
// hours (2026-10-03, until glimmer-agent released plymouth). Fifteen minutes
// is far from both.
const DefaultBootGrace = 15 * time.Minute

// BootStateReader reads what systemd says about this boot. An interface so the
// judgement is tested without a systemd host.
type BootStateReader interface {
	// SystemState is `systemctl is-system-running`: running, degraded,
	// starting, initializing, maintenance, stopping, offline or unknown.
	SystemState() (string, error)
	// Uptime is how long ago the kernel booted.
	Uptime() (time.Duration, error)
	// ActivatingUnits lists the units still activating, which on a stalled
	// headless Halo is plymouth-quit-wait.service.
	ActivatingUnits() ([]string, error)
}

// SystemdBootState reads the running host through systemctl and /proc/uptime.
type SystemdBootState struct {
	// ProcRoot is where /proc is visible.
	ProcRoot string
}

// IsSystemdHost reports whether this host booted with systemd, by the same
// test sd_booted(3) documents: /run/systemd/system exists.
func IsSystemdHost() bool {
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// SystemState runs `systemctl is-system-running`. It exits non-zero for every
// state but running and still prints the state, so the output is used
// whenever there is one.
func (s SystemdBootState) SystemState() (string, error) {
	out, err := exec.Command("systemctl", "is-system-running").Output()
	state := strings.TrimSpace(string(out))
	if state == "" {
		if err == nil {
			err = errors.New("systemctl printed nothing")
		}
		return "", err
	}
	return state, nil
}

// Uptime reads the first field of /proc/uptime.
func (s SystemdBootState) Uptime() (time.Duration, error) {
	raw, err := os.ReadFile(filepath.Join(s.ProcRoot, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, errors.New("/proc/uptime is empty")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// ActivatingUnits lists units in the activating state.
func (s SystemdBootState) ActivatingUnits() ([]string, error) {
	out, err := exec.Command("systemctl", "list-units", "--state=activating", "--no-legend", "--plain").Output()
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

// CheckBootCompleted judges whether this boot finished (halo-operator#11).
//
// "degraded" counts as finished: the boot reached its target and some unit
// failed, which is a different problem with its own tooling (on amd-halo it is
// amd-uep-uploader failing on a 502 from its upload host). What this catches
// is the boot that never finished: "starting" long after the kernel came up,
// with multi-user.target waiting, which no failed-unit check sees because
// nothing has failed. On a headless Halo the platform's own memory-sizing
// units are among those that never run.
func CheckBootCompleted(r BootStateReader, grace time.Duration) ComponentReport {
	c := ComponentReport{Name: BootCompletedComponent, counted: true}
	state, err := r.SystemState()
	if err != nil {
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("systemd's state could not be read: %v", err)
		return c
	}
	switch state {
	case "running":
		c.State = halov1alpha1.ComponentReady
		c.Message = "the boot completed (running)"
		return c
	case "degraded":
		c.State = halov1alpha1.ComponentReady
		c.Message = "the boot completed; systemd reports degraded because a unit failed (see systemctl --failed)"
		return c
	case "maintenance":
		c.State = halov1alpha1.ComponentDegraded
		c.Message = "systemd is in maintenance mode: the boot fell back to a rescue or emergency target"
		return c
	case "starting", "initializing":
	default:
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("systemd reports %q, which says nothing about whether the boot completed", state)
		return c
	}

	up, err := r.Uptime()
	if err != nil {
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("systemd reports %q and the uptime could not be read: %v", state, err)
		return c
	}
	if up < grace {
		c.State = halov1alpha1.ComponentNotObserved
		c.Message = fmt.Sprintf("still booting: systemd reports %q %s after boot, inside the %s grace period",
			state, up.Round(time.Second), grace)
		return c
	}
	c.State = halov1alpha1.ComponentDegraded
	c.Message = fmt.Sprintf("the boot never completed: systemd still reports %q %s after boot", state, up.Round(time.Minute))
	if units, err := r.ActivatingUnits(); err == nil && len(units) > 0 {
		c.Message += ", waiting on " + strings.Join(units, ", ")
	}
	c.Message += " (halo-operator#11)"
	return c
}

// SkippedBootCompleted reports the boot check as not run, and not counted.
func SkippedBootCompleted(state halov1alpha1.ComponentState, reason string) ComponentReport {
	return ComponentReport{Name: BootCompletedComponent, State: state, Message: reason}
}

// CheckBootHold judges the boot hold (`splash` on a headless unit) from the
// reading, through the same translation and decision the controller uses.
// It is added outside Evaluate for the reason the boot images are: the loader
// entries are root-only, and Evaluate is what the controller shares.
func CheckBootHold(node string, spec halov1alpha1.HaloNodeObservationSpec) ComponentReport {
	c := ComponentReport{Name: BootHoldComponent, counted: true}
	result := boothold.Decide(observe.BootHoldObservation(node, &spec))
	c.Message = result.Message
	switch result.Verdict {
	case boothold.Cleared:
		c.State = halov1alpha1.ComponentReady
	case boothold.Held, boothold.RebootPending:
		c.State = halov1alpha1.ComponentDegraded
	default:
		c.State = halov1alpha1.ComponentNotObserved
	}
	return c
}

// SkippedBootHold reports the boot-hold check as not run, and not counted.
func SkippedBootHold(state halov1alpha1.ComponentState, reason string) ComponentReport {
	return ComponentReport{Name: BootHoldComponent, State: state, Message: reason}
}
