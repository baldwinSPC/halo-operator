// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package healthchecks

import (
	"testing"
)

func i32(v int32) *int32 { return &v }

// loadedAndHealthy is a node running flat out near its rated clock.
func loadedAndHealthy() Observation {
	return Observation{
		Node:                "halo-1",
		GPUBusyPercent:      i32(99),
		SCLKMHz:             i32(2900),
		RatedSCLKMHz:        i32(2900),
		PermissionFaults:    i32(0),
		QueueCreateFailures: i32(0),
	}
}

func resultFor(t *testing.T, results []ProbeResult, probe string) ProbeResult {
	t.Helper()
	for _, r := range results {
		if r.Probe == probe {
			return r
		}
	}
	t.Fatalf("probe %q missing from results %+v", probe, results)
	return ProbeResult{}
}

func TestClockUnderLoad(t *testing.T) {
	testCases := []struct {
		name       string
		mutate     func(*Observation)
		wantResult Result
	}{
		{
			name:       "busy at the rated clock is healthy",
			mutate:     func(o *Observation) {},
			wantResult: ResultHealthy,
		},
		{
			name: "busy but stuck at idle clocks is the idle-clock lock",
			mutate: func(o *Observation) {
				o.SCLKMHz = i32(200)
			},
			wantResult: ResultUnhealthy,
		},
		{
			name: "an idle GPU at a low clock is healthy, not locked",
			mutate: func(o *Observation) {
				o.GPUBusyPercent = i32(2)
				o.SCLKMHz = i32(200)
			},
			wantResult: ResultHealthy,
		},
		{
			name: "busy at a moderately reduced clock is healthy",
			mutate: func(o *Observation) {
				o.SCLKMHz = i32(2000)
			},
			wantResult: ResultHealthy,
		},
		{
			name: "an unreadable clock is not observed, not healthy",
			mutate: func(o *Observation) {
				o.SCLKMHz = nil
			},
			wantResult: ResultNotObserved,
		},
		{
			name: "unreadable utilisation is not observed, not healthy",
			mutate: func(o *Observation) {
				o.GPUBusyPercent = nil
			},
			wantResult: ResultNotObserved,
		},
		{
			name: "a zero rated clock is not observed, never a division",
			mutate: func(o *Observation) {
				o.RatedSCLKMHz = i32(0)
			},
			wantResult: ResultNotObserved,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obs := loadedAndHealthy()
			tc.mutate(&obs)
			got := resultFor(t,
				Evaluate([]string{ProbeClockUnderLoad}, DefaultThresholds, obs), ProbeClockUnderLoad)
			if got.Result != tc.wantResult {
				t.Errorf("result = %v, want %v (%s)", got.Result, tc.wantResult, got.Message)
			}
			if got.Message == "" {
				t.Errorf("probe produced no message")
			}
		})
	}
}

func TestCountProbes(t *testing.T) {
	testCases := []struct {
		name       string
		probe      string
		mutate     func(*Observation)
		wantResult Result
	}{
		{
			name:       "zero permission faults is healthy",
			probe:      ProbePermissionFault,
			mutate:     func(o *Observation) {},
			wantResult: ResultHealthy,
		},
		{
			name:       "any permission fault is unhealthy",
			probe:      ProbePermissionFault,
			mutate:     func(o *Observation) { o.PermissionFaults = i32(1) },
			wantResult: ResultUnhealthy,
		},
		{
			name:       "an uncounted permission fault is not observed, not zero",
			probe:      ProbePermissionFault,
			mutate:     func(o *Observation) { o.PermissionFaults = nil },
			wantResult: ResultNotObserved,
		},
		{
			name:       "zero queue-create failures is healthy",
			probe:      ProbeQueueCreate,
			mutate:     func(o *Observation) {},
			wantResult: ResultHealthy,
		},
		{
			name:       "any queue-create failure is unhealthy",
			probe:      ProbeQueueCreate,
			mutate:     func(o *Observation) { o.QueueCreateFailures = i32(7) },
			wantResult: ResultUnhealthy,
		},
		{
			name:       "uncounted queue-create failures are not observed",
			probe:      ProbeQueueCreate,
			mutate:     func(o *Observation) { o.QueueCreateFailures = nil },
			wantResult: ResultNotObserved,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obs := loadedAndHealthy()
			tc.mutate(&obs)
			got := resultFor(t, Evaluate([]string{tc.probe}, DefaultThresholds, obs), tc.probe)
			if got.Result != tc.wantResult {
				t.Errorf("result = %v, want %v (%s)", got.Result, tc.wantResult, got.Message)
			}
		})
	}
}

// TestNoProbeReportsHealthyWithoutItsInputs is the invariant: a probe that
// could not read what it needs must never come back Healthy.
func TestNoProbeReportsHealthyWithoutItsInputs(t *testing.T) {
	blind := Observation{Node: "halo-1"}
	for _, r := range Evaluate(AllProbes, DefaultThresholds, blind) {
		if r.Result == ResultHealthy {
			t.Errorf("probe %q reported Healthy with no inputs at all", r.Probe)
		}
	}
}

func TestUnknownProbeIsNotSilentlyDropped(t *testing.T) {
	results := Evaluate([]string{"no-such-probe"}, DefaultThresholds, loadedAndHealthy())
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Result != ResultNotObserved {
		t.Errorf("unknown probe result = %v, want NotObserved", results[0].Result)
	}
	if Summarize(results).Result == ResultHealthy {
		t.Errorf("a config naming only an unknown probe summarised as Healthy")
	}
}

func TestEvaluateFallsBackToDefaultThresholds(t *testing.T) {
	obs := loadedAndHealthy()
	obs.SCLKMHz = i32(200)
	got := resultFor(t, Evaluate([]string{ProbeClockUnderLoad}, Thresholds{}, obs), ProbeClockUnderLoad)
	if got.Result != ResultUnhealthy {
		t.Errorf("result = %v with zero thresholds, want the defaults to apply", got.Result)
	}
}

func TestSummarize(t *testing.T) {
	testCases := []struct {
		name       string
		results    []ProbeResult
		wantResult Result
	}{
		{
			name:       "nothing probed is not observed, never healthy",
			results:    nil,
			wantResult: ResultNotObserved,
		},
		{
			name: "all healthy is healthy",
			results: []ProbeResult{
				{Probe: ProbeQueueCreate, Result: ResultHealthy},
				{Probe: ProbePermissionFault, Result: ResultHealthy},
			},
			wantResult: ResultHealthy,
		},
		{
			name: "one fault is unhealthy",
			results: []ProbeResult{
				{Probe: ProbeQueueCreate, Result: ResultHealthy},
				{Probe: ProbeClockUnderLoad, Result: ResultUnhealthy, Message: "stuck"},
			},
			wantResult: ResultUnhealthy,
		},
		{
			name: "a fault outranks a probe that could not run",
			results: []ProbeResult{
				{Probe: ProbeQueueCreate, Result: ResultNotObserved},
				{Probe: ProbeClockUnderLoad, Result: ResultUnhealthy, Message: "stuck"},
			},
			wantResult: ResultUnhealthy,
		},
		{
			name: "one probe that could not run blocks a claim of health",
			results: []ProbeResult{
				{Probe: ProbeQueueCreate, Result: ResultHealthy},
				{Probe: ProbeClockUnderLoad, Result: ResultNotObserved},
			},
			wantResult: ResultNotObserved,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := Summarize(tc.results)
			if got.Result != tc.wantResult {
				t.Errorf("result = %v, want %v", got.Result, tc.wantResult)
			}
			if got.Message == "" {
				t.Errorf("summary carries no message")
			}
			if tc.wantResult == ResultUnhealthy && got.Detail == "" {
				t.Errorf("an unhealthy summary carries no concrete detail")
			}
		})
	}
}

func TestSummarizeNamesEveryFailingProbeOnce(t *testing.T) {
	got := Summarize([]ProbeResult{
		{Probe: ProbeClockUnderLoad, Result: ResultUnhealthy, Message: "a"},
		{Probe: ProbeClockUnderLoad, Result: ResultUnhealthy, Message: "b"},
		{Probe: ProbeQueueCreate, Result: ResultUnhealthy, Message: "c"},
	})
	if len(got.Unhealthy) != 2 {
		t.Fatalf("unhealthy = %v, want the two distinct probe names", got.Unhealthy)
	}
	if got.Unhealthy[0] != ProbeClockUnderLoad || got.Unhealthy[1] != ProbeQueueCreate {
		t.Errorf("unhealthy = %v, want a sorted, deduplicated list", got.Unhealthy)
	}
}
