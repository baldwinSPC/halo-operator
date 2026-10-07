// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
)

type stubBootHold struct {
	results []boothold.NodeResult
	err     error
}

func (s stubBootHold) ObserveBootHold(context.Context, map[string]string) ([]boothold.NodeResult, error) {
	return s.results, s.err
}

func evalBootHold(observer BootHoldObserver) metav1.Condition {
	cfg := &halov1alpha1.HaloDeviceConfig{}
	_, cond := evaluateBootHold(context.Background(), observer, cfg, cfg.Spec.WithDefaults(), metav1.Now())
	return cond
}

// A reconciler with nothing wired must not report a cleared hold. This is the
// state the operator ships in until the agent grows the reads, so it is the
// state most likely to be seen.
func TestEvaluateBootHold_NoObserverIsUnknownNotTrue(t *testing.T) {
	got := evalBootHold(nil)
	if got.Status != metav1.ConditionUnknown {
		t.Fatalf("Status = %v, want Unknown — an operator with no source has not established anything", got.Status)
	}
	if got.Reason != "NoObservationSource" {
		t.Errorf("Reason = %q, want NoObservationSource", got.Reason)
	}
}

func TestEvaluateBootHold_AFailedObservationIsUnknown(t *testing.T) {
	got := evalBootHold(stubBootHold{err: errors.New("listing the nodes failed")})
	if got.Status != metav1.ConditionUnknown || got.Reason != "ObservationFailed" {
		t.Fatalf("condition = %+v, want Unknown/ObservationFailed", got)
	}
}

func TestEvaluateBootHold_AHeldNodeIsFalseAndSaysWhich(t *testing.T) {
	got := evalBootHold(stubBootHold{results: []boothold.NodeResult{
		{Node: "amd-halo", Result: boothold.Result{Verdict: boothold.Held, Reason: "BootHoldPresent"}},
	}})
	if got.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False", got.Status)
	}
	if got.Reason != "BootHoldPresent" {
		t.Errorf("Reason = %q, want BootHoldPresent", got.Reason)
	}
}

// Both not-cleared verdicts are False, and the REASON is the only thing that
// separates them. One needs a file edited, the other only a restart.
func TestEvaluateBootHold_RebootPendingIsFalseWithItsOwnReason(t *testing.T) {
	got := evalBootHold(stubBootHold{results: []boothold.NodeResult{
		{Node: "amd-halo", Result: boothold.Result{Verdict: boothold.RebootPending, Reason: "RebootPending"}},
	}})
	if got.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False", got.Status)
	}
	if got.Reason != "RebootPending" {
		t.Errorf("Reason = %q, want RebootPending — a reader told only False will edit files that are already clean", got.Reason)
	}
}

func TestEvaluateBootHold_AllClearedIsTrue(t *testing.T) {
	got := evalBootHold(stubBootHold{results: []boothold.NodeResult{
		{Node: "amd-halo", Result: boothold.Result{Verdict: boothold.Cleared}},
	}})
	if got.Status != metav1.ConditionTrue {
		t.Fatalf("Status = %v, want True", got.Status)
	}
}

// THE WIRING. Every test above calls evaluateBootHold directly, so all of them
// would still pass if the reconciler never called it — the condition would be
// built and unwired, which is a shape this project has shipped before. This one
// reconciles for real and asserts the condition reached the status.
func TestBootHoldConditionReachesTheStatus(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "boot-hold-wiring-cfg", haloPolicy())
	got := reconcileOnce(t, observingReconciler(), "boot-hold-wiring-cfg")

	cond := conditionOf(got, halov1alpha1.ConditionBootHoldCleared)
	if cond == nil {
		t.Fatalf("BootHoldCleared is absent from status after a reconcile — the condition is "+
			"declared and evaluated but never set, so nothing a user reads would ever show it.\nconditions: %+v",
			got.Status.Conditions)
	}
	// observingReconciler wires no BootHoldObserver yet, so Unknown is the
	// correct answer here and asserting True would be asserting a reading
	// nobody took.
	if cond.Status != metav1.ConditionUnknown {
		t.Errorf("BootHoldCleared = %v, want Unknown until an observer is wired", cond.Status)
	}
}
