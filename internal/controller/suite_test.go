// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/boothold"
	"github.com/baldwinSPC/halo-operator/internal/healthchecks"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
	"github.com/baldwinSPC/halo-operator/internal/stackpolicy"
)

// testEnv is the shared control plane. It is package-level because starting a
// real apiserver per test would dominate the runtime.
var (
	testEnv   *envtest.Environment
	testCfg   *rest.Config
	k8sClient client.Client
)

// TestMain brings up a real API server with the CRD installed, so the tests
// below exercise the CRD schema, defaulting, and the status subresource as the
// cluster would apply them rather than as Go structs alone.
//
// It skips rather than fails when the control-plane binaries are absent: a
// developer without them should not be blocked, and CI installs them so the
// skip cannot hide a regression there.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Nothing to run against. Skip the whole package rather than fail.
		os.Exit(runWithoutEnvtest(m))
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	testCfg, err = testEnv.Start()
	if err != nil {
		panic("starting the test control plane: " + err.Error())
	}

	if err := halov1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic("registering the API types: " + err.Error())
	}

	k8sClient, err = client.New(testCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("building the test client: " + err.Error())
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic("stopping the test control plane: " + err.Error())
	}
	os.Exit(code)
}

// runWithoutEnvtest runs the unit tests in this package and reports whether the
// envtest-dependent ones were skipped.
func runWithoutEnvtest(m *testing.M) int {
	return m.Run()
}

// requireEnvtest skips a test when no control plane is available.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make envtest-assets` or `make test-envtest`")
	}
}

// reconcileOnce runs a single reconcile against the live API server and returns
// the object as the server stored it.
func reconcileOnce(t *testing.T, r *HaloDeviceConfigReconciler, name string) *halov1alpha1.HaloDeviceConfig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got halov1alpha1.HaloDeviceConfig
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("re-read the config: %v", err)
	}
	return &got
}

// createConfig applies a config and registers its cleanup.
func createConfig(t *testing.T, name string, spec halov1alpha1.HaloDeviceConfigSpec) {
	t.Helper()
	ctx := context.Background()
	cfg := &halov1alpha1.HaloDeviceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	if err := k8sClient.Create(ctx, cfg); err != nil {
		t.Fatalf("create the config: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), cfg)
	})
}

func conditionOf(cfg *halov1alpha1.HaloDeviceConfig, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(cfg.Status.Conditions, conditionType)
}

func componentOf(cfg *halov1alpha1.HaloDeviceConfig, name string) *halov1alpha1.ComponentStatus {
	for i := range cfg.Status.Components {
		if cfg.Status.Components[i].Name == name {
			return &cfg.Status.Components[i]
		}
	}
	return nil
}

// TestCRDAcceptsTheSample proves the shipped sample is valid against the
// schema the cluster actually enforces, which no Go-level test can establish.
func TestCRDAcceptsTheSample(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "sample", halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{
				"amd_iommu":       "off",
				"amdgpu.gttsize":  "126976",
				"ttm.pages_limit": "32505856",
			},
			MinEffectiveGPUMemoryGiB: 100,
		},
		RuntimeProfile: halov1alpha1.RuntimeProfileSpec{Backends: []string{"vulkan", "rocm"}},
	})
}

// TestCRDRejectsAnUnknownBackend proves the enum marker reached the CRD. A
// validation marker that never made it into the schema is invisible to Go
// tests and rejects nothing at runtime.
func TestCRDRejectsAnUnknownBackend(t *testing.T) {
	requireEnvtest(t)

	cfg := &halov1alpha1.HaloDeviceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-backend"},
		Spec: halov1alpha1.HaloDeviceConfigSpec{
			RuntimeProfile: halov1alpha1.RuntimeProfileSpec{Backends: []string{"vulkan", "cuda"}},
		},
	}
	err := k8sClient.Create(context.Background(), cfg)
	if err == nil {
		_ = k8sClient.Delete(context.Background(), cfg)
		t.Fatal("the API server accepted an unsupported backend; the enum marker is not in the CRD")
	}
}

// TestCRDRejectsAMalformedKernelFloor proves the stack policy's pattern
// reached the CRD: a floor the comparison cannot parse would otherwise be
// stored and then make every node's kernel check unobservable.
func TestCRDRejectsAMalformedKernelFloor(t *testing.T) {
	requireEnvtest(t)

	cfg := &halov1alpha1.HaloDeviceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-kernel-floor"},
		Spec: halov1alpha1.HaloDeviceConfigSpec{
			StackPolicy: halov1alpha1.StackPolicySpec{MinKernelVersion: "6.18.4-rc1"},
		},
	}
	err := k8sClient.Create(context.Background(), cfg)
	if err == nil {
		_ = k8sClient.Delete(context.Background(), cfg)
		t.Fatal("the API server accepted a malformed kernel floor; the pattern marker is not in the CRD")
	}
}

// TestCRDRejectsANegativeMemoryFloor proves the Minimum marker reached the CRD.
func TestCRDRejectsANegativeMemoryFloor(t *testing.T) {
	requireEnvtest(t)

	cfg := &halov1alpha1.HaloDeviceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "negative-floor"},
		Spec: halov1alpha1.HaloDeviceConfigSpec{
			MemoryPolicy: halov1alpha1.MemoryPolicySpec{MinEffectiveGPUMemoryGiB: -1},
		},
	}
	err := k8sClient.Create(context.Background(), cfg)
	if err == nil {
		_ = k8sClient.Delete(context.Background(), cfg)
		t.Fatal("the API server accepted a negative memory floor")
	}
}

// TestReconcileWithoutObserversReportsNotObserved is the end-to-end form of the
// invariant the unit tests hold: with nothing wired up, a real reconcile
// against a real API server must persist Unknown, not a claim of health.
func TestReconcileWithoutObserversReportsNotObserved(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "unobserved", halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{"amd_iommu": "off"},
		},
	})

	r := &HaloDeviceConfigReconciler{Client: k8sClient}
	got := reconcileOnce(t, r, "unobserved")

	for _, conditionType := range []string{
		halov1alpha1.ConditionMemoryPolicyCompliant,
		halov1alpha1.ConditionBackendsAvailable,
		halov1alpha1.ConditionNodesHealthy,
	} {
		c := conditionOf(got, conditionType)
		if c == nil {
			t.Fatalf("%s was never written to status", conditionType)
		}
		if c.Status != metav1.ConditionUnknown {
			t.Errorf("%s = %s with no observer, want Unknown", conditionType, c.Status)
		}
	}

	ready := conditionOf(got, halov1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("Ready was never written to status")
	}
	if ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready = True with nothing observed")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want the object generation %d",
			got.Status.ObservedGeneration, got.Generation)
	}
}

// TestReconcilePersistsDrift proves drift survives the round trip through the
// status subresource, with the offending parameter still named.
func TestReconcilePersistsDrift(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "drifted", halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{"amd_iommu": "off"},
		},
	})

	r := &HaloDeviceConfigReconciler{
		Client: k8sClient,
		MemoryObserver: stubObserver{observations: []memorypolicy.Observation{{
			Node:         "halo-1",
			KernelParams: map[string]string{"amd_iommu": "on"},
		}}},
	}
	got := reconcileOnce(t, r, "drifted")

	c := conditionOf(got, halov1alpha1.ConditionMemoryPolicyCompliant)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("MemoryPolicyCompliant = %v, want False", c)
	}
	if !strings.Contains(c.Message, "amd_iommu") {
		t.Errorf("the persisted condition does not name the drifted parameter: %q", c.Message)
	}

	component := componentOf(got, componentMemoryPolicy)
	if component == nil || component.State != halov1alpha1.ComponentDegraded {
		t.Errorf("memory-policy component = %v, want Degraded", component)
	}
	if component != nil && component.LastObserved.IsZero() {
		t.Errorf("the persisted component status carries no observation timestamp")
	}
}

// TestReconcileReportsReadyOnlyWhenEverythingObservedIsHealthy walks the whole
// chain: every observer supplies healthy readings for a config that leaves the
// runtime profile and the health checks at their defaults, and Ready becomes
// True. Until #27 the runtime profile was always NotImplemented and could not
// be disabled, so Ready could never be True on any config.
func TestReconcileReportsReadyOnlyWhenEverythingObservedIsHealthy(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "healthy", halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{"amd_iommu": "off"},
		},
	})

	r := &HaloDeviceConfigReconciler{
		Client: k8sClient,
		MemoryObserver: stubObserver{observations: []memorypolicy.Observation{{
			Node:         "halo-1",
			KernelParams: map[string]string{"amd_iommu": "off"},
		}}},
		RuntimeObserver: stubRuntimeObserver{observations: []runtimeprofile.Observation{bothBackends("halo-1")}},
		StackObserver:   stubStackObserver{observations: []stackpolicy.Observation{inboxStack("halo-1")}},
		BootHoldObserver: stubBootHold{results: []boothold.NodeResult{{
			Node: "halo-1", Result: boothold.Result{Verdict: boothold.Cleared, Reason: "Cleared"}}}},
		HealthObserver: stubHealthObserver{observations: []healthchecks.Observation{{
			Node:                "halo-1",
			GPUBusyPercent:      i32(99),
			SCLKMHz:             i32(2900),
			RatedSCLKMHz:        i32(2900),
			PermissionFaults:    i32(0),
			QueueCreateFailures: i32(0),
		}}},
	}
	got := reconcileOnce(t, r, "healthy")

	for _, conditionType := range []string{
		halov1alpha1.ConditionMemoryPolicyCompliant,
		halov1alpha1.ConditionBackendsAvailable,
		halov1alpha1.ConditionNodesHealthy,
		halov1alpha1.ConditionStackCompliant,
		halov1alpha1.ConditionGPUFirmwareCompliant,
		halov1alpha1.ConditionBootHoldCleared,
		halov1alpha1.ConditionReady,
	} {
		c := conditionOf(got, conditionType)
		if c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("%s = %v, want True", conditionType, c)
		}
	}
	for _, name := range []string{componentMemoryPolicy, componentRuntimeProfile, componentHealthChecks,
		componentStackPolicy, componentBootHold} {
		component := componentOf(got, name)
		if component == nil || component.State != halov1alpha1.ComponentReady {
			t.Errorf("%s component = %v, want Ready", name, component)
		}
	}
}

// TestReconcileIsIdempotent proves a second reconcile over unchanged input does
// not churn the conditions, which would otherwise reset transition timestamps
// on every pass and make the status useless for spotting real changes.
func TestReconcileIsIdempotent(t *testing.T) {
	requireEnvtest(t)

	createConfig(t, "idempotent", halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{"amd_iommu": "off"},
		},
	})

	r := &HaloDeviceConfigReconciler{
		Client: k8sClient,
		MemoryObserver: stubObserver{observations: []memorypolicy.Observation{{
			Node:         "halo-1",
			KernelParams: map[string]string{"amd_iommu": "off"},
		}}},
	}

	first := reconcileOnce(t, r, "idempotent")
	firstTransition := conditionOf(first, halov1alpha1.ConditionMemoryPolicyCompliant).LastTransitionTime

	time.Sleep(1100 * time.Millisecond) // condition timestamps have second resolution
	second := reconcileOnce(t, r, "idempotent")
	secondTransition := conditionOf(second, halov1alpha1.ConditionMemoryPolicyCompliant).LastTransitionTime

	if !firstTransition.Equal(&secondTransition) {
		t.Errorf("the condition transition time moved on an unchanged reconcile: %v then %v",
			firstTransition, secondTransition)
	}
}

// TestReconcileOnAMissingConfigIsNotAnError proves a deleted config does not
// wedge the controller in an error loop.
func TestReconcileOnAMissingConfigIsNotAnError(t *testing.T) {
	requireEnvtest(t)

	r := &HaloDeviceConfigReconciler{Client: k8sClient}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "does-not-exist"},
	}); err != nil {
		t.Errorf("reconciling a missing config returned %v, want no error", err)
	}
}
