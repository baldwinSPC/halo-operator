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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/nodeagent"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

// These tests walk the whole observation pipe against a real API server: a
// fixture filesystem shaped like a Halo node, the real collector reading it,
// the real publisher applying the observation, the real observer joining it to
// Nodes, and the real reconciler judging it. What they cannot do is make the
// fixture true of any physical machine — the sysfs surface of an actual
// gfx1151 unit is pending hardware, and nothing here claims otherwise. What
// they do prove is that when a node CAN be read, a condition CAN reach True,
// and that every failure to read degrades to Unknown rather than to a verdict.

// haloPolicy is the spec the fixture node satisfies.
func haloPolicy() halov1alpha1.HaloDeviceConfigSpec {
	return halov1alpha1.HaloDeviceConfigSpec{
		MemoryPolicy: halov1alpha1.MemoryPolicySpec{
			ExpectedKernelArgs: map[string]string{
				"amd_iommu":       "off",
				"amdgpu.gttsize":  "126976",
				"ttm.pages_limit": "32505856",
			},
			MinEffectiveGPUMemoryGiB: 100,
		},
		HealthChecks: halov1alpha1.HealthChecksSpec{
			// Only the sysfs-backed probe. The kernel-log probes have no
			// reader yet (their dmesg patterns are hardware facts pending a
			// physical unit), so including them holds NodesHealthy at Unknown
			// — which TestDefaultProbesStayUnknown pins as the default.
			Probes: []string{"clock-under-load"},
		},
	}
}

// haloNodeFixture is a fake /proc + /sys shaped like a healthy, compliant
// Halo node: recommended kernel args, one amdgpu display device, 124 GiB of
// GTT, busy at the top of its clock ladder, with a render node and a KFD GPU
// agent at gfx1151 so it can run both backends.
func haloNodeFixture(t *testing.T) nodeagent.Collector {
	t.Helper()
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(procRoot, "cmdline"),
		"BOOT_IMAGE=/vmlinuz amd_iommu=off amdgpu.gttsize=126976 ttm.pages_limit=32505856 quiet\n")
	dev := filepath.Join(sysRoot, "bus", "pci", "devices", "0000:c5:00.0")
	write(filepath.Join(dev, "vendor"), "0x1002\n")
	write(filepath.Join(dev, "class"), "0x030000\n")
	write(filepath.Join(dev, "mem_info_gtt_total"), "133143986176\n")
	write(filepath.Join(dev, "gpu_busy_percent"), "97\n")
	write(filepath.Join(dev, "pp_dpm_sclk"), "0: 200Mhz\n1: 1100Mhz\n2: 2900Mhz *\n")
	write(filepath.Join(dev, "drm", "renderD128", "dev"), "226:128\n")
	kfd := filepath.Join(sysRoot, "class", "kfd", "kfd", "topology", "nodes")
	write(filepath.Join(kfd, "0", "properties"), "cpu_cores_count 32\nsimd_count 0\ngfx_target_version 0\n")
	write(filepath.Join(kfd, "1", "properties"), "cpu_cores_count 0\nsimd_count 80\ngfx_target_version 110501\n")
	driver := filepath.Join(sysRoot, "bus", "pci", "drivers", "amdgpu")
	if err := os.MkdirAll(driver, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driver, filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	return nodeagent.Collector{ProcRoot: procRoot, SysRoot: sysRoot}
}

// createAMDNode registers a Node carrying the NFD label the default selector
// targets, plus cleanup of it and any observation published for it. The
// observation is deleted explicitly because envtest runs no garbage
// collector; in a real cluster the owner reference handles it.
func createAMDNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{observe.NodeFeatureLabelAMDGPU: "true"},
	}}
	if err := k8sClient.Create(context.Background(), node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(),
			&halov1alpha1.HaloNodeObservation{ObjectMeta: metav1.ObjectMeta{Name: name}})
		_ = k8sClient.Delete(context.Background(), node)
	})
	return node
}

// collectAndPublish runs the real agent pipeline for one node.
func collectAndPublish(t *testing.T, collector nodeagent.Collector, node string, at metav1.Time) {
	t.Helper()
	publisher := nodeagent.Publisher{Client: k8sClient, NodeName: node}
	if err := publisher.Publish(context.Background(), collector.Collect(), at); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// observingReconciler is the reconciler as cmd/main.go wires it, reading
// through the given client with no periodic recheck (tests drive reconciles).
func observingReconciler() *HaloDeviceConfigReconciler {
	observer := &observe.Observer{Reader: k8sClient}
	return &HaloDeviceConfigReconciler{
		Client:          k8sClient,
		MemoryObserver:  observer,
		RuntimeObserver: observer,
		HealthObserver:  observer,
	}
}

// TestObservedCompliantNodeReachesTrue is the acceptance shape of issue #8:
// with a node that can actually be read, MemoryPolicyCompliant reaches True —
// and reaches it through the same collector, publisher, observer and
// reconciler the shipped binaries run, not through a stub.
func TestObservedCompliantNodeReachesTrue(t *testing.T) {
	requireEnvtest(t)

	node := createAMDNode(t, "flow-compliant")
	collectAndPublish(t, haloNodeFixture(t), node.Name, metav1.Now())
	createConfig(t, "flow-compliant-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-compliant-cfg")

	memory := conditionOf(got, halov1alpha1.ConditionMemoryPolicyCompliant)
	if memory == nil || memory.Status != metav1.ConditionTrue {
		t.Fatalf("MemoryPolicyCompliant = %+v, want True", memory)
	}
	health := conditionOf(got, halov1alpha1.ConditionNodesHealthy)
	if health == nil || health.Status != metav1.ConditionTrue {
		t.Fatalf("NodesHealthy = %+v, want True for the sysfs-backed probe", health)
	}
	backends := conditionOf(got, halov1alpha1.ConditionBackendsAvailable)
	if backends == nil || backends.Status != metav1.ConditionTrue {
		t.Fatalf("BackendsAvailable = %+v, want True for a node with a render node and a KFD GPU agent", backends)
	}
	component := componentOf(got, componentMemoryPolicy)
	if component == nil || component.State != halov1alpha1.ComponentReady {
		t.Errorf("memory-policy component = %+v, want Ready", component)
	}
	// Every component observed and healthy, through the shipped pipe: Ready
	// is reachable, which it never was while runtime-profile was a stub.
	ready := conditionOf(got, halov1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True", ready)
	}
}

// TestNodeWithoutKFDCannotRunROCm: the same node with no KFD in its kernel.
// It can still run Vulkan, so the verdict must be False, not Unknown — KFD's
// absence was observed — and must name the backend and the node.
func TestNodeWithoutKFDCannotRunROCm(t *testing.T) {
	requireEnvtest(t)

	collector := haloNodeFixture(t)
	if err := os.RemoveAll(filepath.Join(collector.SysRoot, "class", "kfd")); err != nil {
		t.Fatal(err)
	}

	node := createAMDNode(t, "flow-no-kfd")
	collectAndPublish(t, collector, node.Name, metav1.Now())
	createConfig(t, "flow-no-kfd-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-no-kfd-cfg")

	backends := conditionOf(got, halov1alpha1.ConditionBackendsAvailable)
	if backends == nil || backends.Status != metav1.ConditionFalse {
		t.Fatalf("BackendsAvailable = %+v, want False for a node without KFD", backends)
	}
	for _, want := range []string{"rocm", node.Name} {
		if !strings.Contains(backends.Message, want) {
			t.Errorf("the condition does not name %q: %q", want, backends.Message)
		}
	}
	if strings.Contains(backends.Message, "vulkan") {
		t.Errorf("the condition blames vulkan, which this node can run: %q", backends.Message)
	}
}

// TestObservedDriftReachesFalseWithTheParameterNamed: the same pipe, with the
// node booted without amd_iommu=off. Drift must be False — not Unknown — and
// must name what drifted.
func TestObservedDriftReachesFalseWithTheParameterNamed(t *testing.T) {
	requireEnvtest(t)

	collector := haloNodeFixture(t)
	cmdline := filepath.Join(collector.ProcRoot, "cmdline")
	if err := os.WriteFile(cmdline,
		[]byte("BOOT_IMAGE=/vmlinuz amdgpu.gttsize=126976 ttm.pages_limit=32505856 quiet\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	node := createAMDNode(t, "flow-drifted")
	collectAndPublish(t, collector, node.Name, metav1.Now())
	createConfig(t, "flow-drifted-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-drifted-cfg")

	memory := conditionOf(got, halov1alpha1.ConditionMemoryPolicyCompliant)
	if memory == nil || memory.Status != metav1.ConditionFalse {
		t.Fatalf("MemoryPolicyCompliant = %+v, want False for a drifted node", memory)
	}
	if !strings.Contains(memory.Message, "amd_iommu") {
		t.Errorf("the condition does not name the drifted parameter: %q", memory.Message)
	}
}

// TestUnreadableNodeStaysUnknown: a node whose agent could not read anything —
// no cmdline, no GPU — must hold the conditions at Unknown. It is the
// difference between "wrong" and "unproven", and it is the line this operator
// exists to hold.
func TestUnreadableNodeStaysUnknown(t *testing.T) {
	requireEnvtest(t)

	// An empty filesystem: everything the collector tries is absent.
	empty := nodeagent.Collector{
		ProcRoot: filepath.Join(t.TempDir(), "proc"),
		SysRoot:  filepath.Join(t.TempDir(), "sys"),
	}
	node := createAMDNode(t, "flow-unreadable")
	collectAndPublish(t, empty, node.Name, metav1.Now())
	createConfig(t, "flow-unreadable-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-unreadable-cfg")

	memory := conditionOf(got, halov1alpha1.ConditionMemoryPolicyCompliant)
	if memory == nil || memory.Status != metav1.ConditionUnknown {
		t.Fatalf("MemoryPolicyCompliant = %+v for an unreadable node, want Unknown — "+
			"an unread node must never resolve to a verdict", memory)
	}
	health := conditionOf(got, halov1alpha1.ConditionNodesHealthy)
	if health == nil || health.Status != metav1.ConditionUnknown {
		t.Fatalf("NodesHealthy = %+v for an unreadable node, want Unknown", health)
	}
	backends := conditionOf(got, halov1alpha1.ConditionBackendsAvailable)
	if backends == nil || backends.Status != metav1.ConditionUnknown {
		t.Fatalf("BackendsAvailable = %+v for an unreadable node, want Unknown", backends)
	}
}

// TestStaleObservationDegradesToUnknown: a reading that was once fresh ages
// out. Yesterday's compliance is not today's.
func TestStaleObservationDegradesToUnknown(t *testing.T) {
	requireEnvtest(t)

	node := createAMDNode(t, "flow-stale")
	staleAt := metav1.NewTime(time.Now().Add(-observe.DefaultMaxAge - time.Minute))
	collectAndPublish(t, haloNodeFixture(t), node.Name, staleAt)
	createConfig(t, "flow-stale-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-stale-cfg")

	memory := conditionOf(got, halov1alpha1.ConditionMemoryPolicyCompliant)
	if memory == nil || memory.Status != metav1.ConditionUnknown {
		t.Fatalf("MemoryPolicyCompliant = %+v on a stale observation, want Unknown", memory)
	}
}

// TestDefaultProbesStayUnknownWithoutKernelLogReaders pins the honest default:
// two of the three default probes need kernel-log counters nothing collects
// yet, so out of the box NodesHealthy is Unknown, not True. When the
// kernel-log reader lands (hardware-validated patterns), this test is the one
// that changes.
func TestDefaultProbesStayUnknownWithoutKernelLogReaders(t *testing.T) {
	requireEnvtest(t)

	node := createAMDNode(t, "flow-default-probes")
	collectAndPublish(t, haloNodeFixture(t), node.Name, metav1.Now())
	spec := haloPolicy()
	spec.HealthChecks.Probes = nil // defaulted to all three probes
	createConfig(t, "flow-default-probes-cfg", spec)

	got := reconcileOnce(t, observingReconciler(), "flow-default-probes-cfg")

	health := conditionOf(got, halov1alpha1.ConditionNodesHealthy)
	if health == nil || health.Status != metav1.ConditionUnknown {
		t.Fatalf("NodesHealthy = %+v with the default probe set, want Unknown while "+
			"the kernel-log probes have no reader", health)
	}
}

// TestClockLockObservedThroughThePipe: the health path can reach False too —
// a node busy at idle clocks is the documented idle-clock lock, and the
// verdict survives the whole pipe with the ROCm issue named.
func TestClockLockObservedThroughThePipe(t *testing.T) {
	requireEnvtest(t)

	collector := haloNodeFixture(t)
	dev := filepath.Join(collector.SysRoot, "bus", "pci", "devices", "0000:c5:00.0")
	if err := os.WriteFile(filepath.Join(dev, "pp_dpm_sclk"),
		[]byte("0: 200Mhz *\n1: 1100Mhz\n2: 2900Mhz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	node := createAMDNode(t, "flow-clocklock")
	collectAndPublish(t, collector, node.Name, metav1.Now())
	createConfig(t, "flow-clocklock-cfg", haloPolicy())

	got := reconcileOnce(t, observingReconciler(), "flow-clocklock-cfg")

	health := conditionOf(got, halov1alpha1.ConditionNodesHealthy)
	if health == nil || health.Status != metav1.ConditionFalse {
		t.Fatalf("NodesHealthy = %+v for a busy node at idle clocks, want False", health)
	}
	if !strings.Contains(health.Message, "5750") {
		t.Errorf("the condition does not cite the documented failure mode: %q", health.Message)
	}
}

// TestObservationWatchTriggersReconcile proves the event plumbing: with the
// controller running under a real manager — watches, cache, mapping functions,
// no periodic recheck to lean on — publishing an observation must move the
// condition from Unknown to True with nobody calling Reconcile by hand.
func TestObservationWatchTriggersReconcile(t *testing.T) {
	requireEnvtest(t)

	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	observer := &observe.Observer{Reader: mgr.GetClient()}
	reconciler := &HaloDeviceConfigReconciler{
		Client:         mgr.GetClient(),
		MemoryObserver: observer,
		HealthObserver: observer,
		// RecheckPeriod deliberately zero: if the watches do not fire, no
		// timer may rescue this test.
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup with manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager stopped with %v", err)
		}
	})

	node := createAMDNode(t, "flow-watch")
	createConfig(t, "flow-watch-cfg", haloPolicy())

	// First the controller must have recorded honest ignorance on its own.
	waitForCondition(t, "flow-watch-cfg", halov1alpha1.ConditionMemoryPolicyCompliant,
		metav1.ConditionUnknown)

	// The agent publishes; the condition must follow with no manual reconcile.
	collectAndPublish(t, haloNodeFixture(t), node.Name, metav1.Now())
	waitForCondition(t, "flow-watch-cfg", halov1alpha1.ConditionMemoryPolicyCompliant,
		metav1.ConditionTrue)
}

// waitForCondition polls until the named condition reaches the wanted status.
func waitForCondition(t *testing.T, config, conditionType string, want metav1.ConditionStatus) {
	t.Helper()
	deadline, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var last *metav1.Condition
	err := wait.PollUntilContextTimeout(deadline, 100*time.Millisecond, 15*time.Second, true,
		func(ctx context.Context) (bool, error) {
			var cfg halov1alpha1.HaloDeviceConfig
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: config}, &cfg); err != nil {
				return false, err
			}
			last = conditionOf(&cfg, conditionType)
			return last != nil && last.Status == want, nil
		})
	if err != nil {
		t.Fatalf("%s never reached %s; last seen %+v", conditionType, want, last)
	}
}
