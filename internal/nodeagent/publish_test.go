// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

// The publisher's whole job is preserving the difference between "measured"
// and "absent" through the API server's serialization and server-side apply.
// A fake client cannot prove that; these tests run against the real thing and
// skip, exactly like the controller suite, when the control-plane binaries are
// missing. CI asserts the suite ran.
var (
	testEnv   *envtest.Environment
	testCfg   *rest.Config
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		os.Exit(m.Run())
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

func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make envtest-assets` or `make test`")
	}
}

// createNode registers a minimal Node and its cleanup.
func createNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), node)
		_ = k8sClient.Delete(context.Background(),
			&halov1alpha1.HaloNodeObservation{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return node
}

// specFields returns the set of keys actually present in the stored spec,
// bypassing the Go types whose nil pointers cannot distinguish "absent in
// etcd" from "absent in this struct".
func specFields(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(halov1alpha1.GroupVersion.WithKind("HaloNodeObservation"))
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, u); err != nil {
		t.Fatalf("read back the observation: %v", err)
	}
	spec, _, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil {
		t.Fatalf("no spec in the stored observation: %v", err)
	}
	return spec
}

func i64(v int64) *int64   { return &v }
func str(s string) *string { return &s }

func TestPublishPreservesAbsenceThroughTheAPIServer(t *testing.T) {
	requireEnvtest(t)
	node := createNode(t, "halo-pub-1")

	p := Publisher{Client: k8sClient, NodeName: node.Name}
	reading := Reading{
		KernelCmdline: str("amd_iommu=off quiet"),
		GTTTotalBytes: i64(133143986176),
		// GPUBusyPercent, SCLKMHz, RatedSCLKMHz deliberately nil: not measured.
		Messages: []string{"busy: gpu_busy_percent unreadable in this test"},
	}
	if err := p.Publish(context.Background(), reading, metav1.Now()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	spec := specFields(t, node.Name)
	for _, want := range []string{"observedAt", "kernelCmdline", "gttTotalBytes", "messages"} {
		if _, ok := spec[want]; !ok {
			t.Errorf("stored spec is missing %q, which was measured", want)
		}
	}
	for _, absent := range []string{"gpuBusyPercent", "sclkMHz", "ratedSCLKMHz", "permissionFaults", "queueCreateFailures",
		"drmRenderNode", "gfxTargetVersion"} {
		if v, ok := spec[absent]; ok {
			t.Errorf("stored spec contains %q = %v for a reading that was never taken; "+
				"absence did not survive the round trip", absent, v)
		}
	}
}

// TestPublishKeepsAnEmptyRenderNodeAndAZeroTarget: both backend readings use
// their zero value as a reading ("the kernel registered none"), so both must
// reach the API server as present fields. Dropped, they would read as unread
// and a node that cannot run a backend would hold the condition at Unknown.
func TestPublishKeepsAnEmptyRenderNodeAndAZeroTarget(t *testing.T) {
	requireEnvtest(t)
	node := createNode(t, "halo-pub-zero")

	zero := int32(0)
	p := Publisher{Client: k8sClient, NodeName: node.Name}
	if err := p.Publish(context.Background(), Reading{DRMRenderNode: str(""), GFXTargetVersion: &zero}, metav1.Now()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	spec := specFields(t, node.Name)
	if v, ok := spec["drmRenderNode"]; !ok || v != "" {
		t.Errorf("drmRenderNode = %v (present %v), want a present empty string", v, ok)
	}
	if v, ok := spec["gfxTargetVersion"]; !ok || v != int64(0) {
		t.Errorf("gfxTargetVersion = %v (%T, present %v), want a present 0", v, v, ok)
	}
}

// TestPublishRemovesAReadingThatBecameUnreadable is the server-side-apply
// semantic the transport depends on: a counter present last cycle and
// unreadable this cycle must leave the stored object, not linger as a stale
// number that keeps a probe running on fiction.
func TestPublishRemovesAReadingThatBecameUnreadable(t *testing.T) {
	requireEnvtest(t)
	node := createNode(t, "halo-pub-2")
	p := Publisher{Client: k8sClient, NodeName: node.Name}

	full := Reading{
		KernelCmdline:  str("amd_iommu=off"),
		GTTTotalBytes:  i64(133143986176),
		GPUBusyPercent: i32(40),
	}
	if err := p.Publish(context.Background(), full, metav1.Now()); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, ok := specFields(t, node.Name)["gpuBusyPercent"]; !ok {
		t.Fatal("gpuBusyPercent was not stored by the first publish")
	}

	degraded := Reading{
		KernelCmdline: str("amd_iommu=off"),
		GTTTotalBytes: i64(133143986176),
		// GPUBusyPercent gone: the counter became unreadable.
		Messages: []string{"busy: became unreadable"},
	}
	if err := p.Publish(context.Background(), degraded, metav1.Now()); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	spec := specFields(t, node.Name)
	if v, ok := spec["gpuBusyPercent"]; ok {
		t.Errorf("gpuBusyPercent = %v is still stored after it became unreadable; "+
			"a stale reading is being presented as a current one", v)
	}
	if _, ok := spec["gttTotalBytes"]; !ok {
		t.Error("gttTotalBytes disappeared even though it was still measured")
	}
}

func TestPublishOwnsTheObservationWithTheNode(t *testing.T) {
	requireEnvtest(t)
	node := createNode(t, "halo-pub-3")
	p := Publisher{Client: k8sClient, NodeName: node.Name}

	if err := p.Publish(context.Background(), Reading{}, metav1.Now()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var obs halov1alpha1.HaloNodeObservation
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: node.Name}, &obs); err != nil {
		t.Fatalf("read back the observation: %v", err)
	}
	if len(obs.OwnerReferences) != 1 {
		t.Fatalf("observation has %d owner references, want exactly the Node", len(obs.OwnerReferences))
	}
	ref := obs.OwnerReferences[0]
	if ref.Kind != "Node" || ref.Name != node.Name || ref.UID != node.UID {
		t.Errorf("owner reference is %+v, want the Node %s (%s)", ref, node.Name, node.UID)
	}
}

func TestPublishRefusesToPublishForAMissingNode(t *testing.T) {
	requireEnvtest(t)

	p := Publisher{Client: k8sClient, NodeName: "no-such-node"}
	if err := p.Publish(context.Background(), Reading{}, metav1.Now()); err == nil {
		t.Fatal("publishing for a node that does not exist succeeded; " +
			"that observation could never be garbage-collected")
	}

	var obs halov1alpha1.HaloNodeObservation
	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "no-such-node"}, &obs)
	if err == nil {
		t.Error("an orphan observation was created for a missing node")
	}
}

// TestPublishedObservedAtIsCurrent pins the timestamp to the reading rather
// than to any server-side default, since staleness decisions hang off it.
func TestPublishedObservedAtIsCurrent(t *testing.T) {
	requireEnvtest(t)
	node := createNode(t, "halo-pub-4")
	p := Publisher{Client: k8sClient, NodeName: node.Name}

	at := metav1.NewTime(time.Now().Add(-42 * time.Second).Truncate(time.Second))
	if err := p.Publish(context.Background(), Reading{}, at); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var obs halov1alpha1.HaloNodeObservation
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: node.Name}, &obs); err != nil {
		t.Fatalf("read back the observation: %v", err)
	}
	if !obs.Spec.ObservedAt.Equal(&at) {
		t.Errorf("observedAt = %v, want the collection time %v", obs.Spec.ObservedAt, at)
	}
}
