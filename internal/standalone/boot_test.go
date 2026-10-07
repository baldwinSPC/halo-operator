// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package standalone

import (
	"errors"
	"strings"
	"testing"
	"time"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

type fakeBoot struct {
	state    string
	stateErr error
	uptime   time.Duration
	upErr    error
	units    []string
}

func (f fakeBoot) SystemState() (string, error)       { return f.state, f.stateErr }
func (f fakeBoot) Uptime() (time.Duration, error)     { return f.uptime, f.upErr }
func (f fakeBoot) ActivatingUnits() ([]string, error) { return f.units, nil }

func TestCheckBootCompleted(t *testing.T) {
	stalled := fakeBoot{state: "starting", uptime: 26 * time.Hour, units: []string{"plymouth-quit-wait.service"}}
	cases := []struct {
		name  string
		boot  fakeBoot
		want  halov1alpha1.ComponentState
		inMsg string
	}{
		{"running is complete", fakeBoot{state: "running"}, halov1alpha1.ComponentReady, "completed"},
		// amd-halo on 2026-10-06: degraded only because amd-uep-uploader got a
		// 502. The boot finished; a failed unit is a different finding.
		{"degraded is complete", fakeBoot{state: "degraded"}, halov1alpha1.ComponentReady, "degraded"},
		// amd-halo on 2026-10-03/04: 26 hours in starting, held by plymouth.
		{"starting long after boot is the stall, naming the unit", stalled, halov1alpha1.ComponentDegraded, "plymouth-quit-wait.service"},
		{"starting just after boot is not judged yet",
			fakeBoot{state: "starting", uptime: time.Minute}, halov1alpha1.ComponentNotObserved, "still booting"},
		{"maintenance is a failed boot", fakeBoot{state: "maintenance"}, halov1alpha1.ComponentDegraded, "maintenance"},
		{"an unreadable state is not observed", fakeBoot{stateErr: errors.New("no bus")}, halov1alpha1.ComponentNotObserved, "could not be read"},
		{"starting with an unreadable uptime is not observed",
			fakeBoot{state: "starting", upErr: errors.New("gone")}, halov1alpha1.ComponentNotObserved, "uptime"},
		{"stopping says nothing about the boot", fakeBoot{state: "stopping"}, halov1alpha1.ComponentNotObserved, "says nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CheckBootCompleted(tc.boot, DefaultBootGrace)
			if c.State != tc.want {
				t.Errorf("state = %s (%s), want %s", c.State, c.Message, tc.want)
			}
			if !strings.Contains(c.Message, tc.inMsg) {
				t.Errorf("message %q does not mention %q", c.Message, tc.inMsg)
			}
			if !c.counted {
				t.Error("a judged boot check must count toward the outcome")
			}
		})
	}
}

// A stalled boot must change the exit code: "nothing has failed" is the
// exact cover it hides behind.
func TestAStalledBootIsDrift(t *testing.T) {
	r := Evaluate("amd-halo", amdHaloSpec(), config(platformPolicy()))
	r.AddComponent(CheckBootCompleted(fakeBoot{state: "starting", uptime: time.Hour}, DefaultBootGrace))
	if r.Outcome != OutcomeDrifted || r.ExitCode != ExitDrifted {
		t.Errorf("outcome = %s (exit %d), want drifted", r.Outcome, r.ExitCode)
	}
	skipped := Evaluate("amd-halo", amdHaloSpec(), config(platformPolicy()))
	skipped.AddComponent(SkippedBootCompleted(halov1alpha1.ComponentNotObserved, "skipped"))
	if skipped.ExitCode != ExitCompliant {
		t.Errorf("a skipped boot check changed the exit code to %d", skipped.ExitCode)
	}
}
