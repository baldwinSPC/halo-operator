// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/memorypolicy"
	"github.com/baldwinSPC/halo-operator/internal/runtimeprofile"
)

// fixedNow is the test clock; observations are aged relative to it.
var fixedNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := halov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// amdNode is a node carrying the NFD label the default selector targets.
func amdNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{NodeFeatureLabelAMDGPU: "true"},
	}}
}

// observation publishes a reading for a node, aged by the given amount
// relative to the test clock.
func observation(node string, age time.Duration, mutate func(*halov1alpha1.HaloNodeObservationSpec)) *halov1alpha1.HaloNodeObservation {
	obs := &halov1alpha1.HaloNodeObservation{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec: halov1alpha1.HaloNodeObservationSpec{
			ObservedAt: metav1.NewTime(fixedNow.Add(-age)),
		},
	}
	if mutate != nil {
		mutate(&obs.Spec)
	}
	return obs
}

func observer(t *testing.T, objs ...client.Object) *Observer {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &Observer{Reader: c, Now: func() time.Time { return fixedNow }}
}

func str(s string) *string { return &s }
func i64(v int64) *int64   { return &v }
func i32(v int32) *int32   { return &v }

func TestObserveJoinsAFreshObservation(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.KernelCmdline = str(`BOOT_IMAGE=/vmlinuz amd_iommu=off amdgpu.gttsize=126976 console="ttyS0 115200" quiet`)
			// One byte short of 124 GiB: the floor must say 123, because a
			// floor rounded up is a floor not met reported as met.
			s.GTTTotalBytes = i64(124<<30 - 1)
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	want := []memorypolicy.Observation{{
		Node: "halo-1",
		KernelParams: map[string]string{
			"BOOT_IMAGE":     "/vmlinuz",
			"amd_iommu":      "off",
			"amdgpu.gttsize": "126976",
			"console":        "ttyS0 115200",
			"quiet":          "",
		},
		EffectiveGPUMemoryGiB: i32(123),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Observe() = %+v, want %+v", got, want)
	}
}

// TestObserveKeepsAbsenceAndEmptinessApart is the transport-end of the
// distinction the whole chain preserves: an absent cmdline means "could not
// read" (nil map → NotObserved), while a present-but-empty cmdline means
// "read fine, nothing set" (empty map → drift for anything expected).
func TestObserveKeepsAbsenceAndEmptinessApart(t *testing.T) {
	o := observer(t,
		amdNode("unread"), amdNode("empty"),
		observation("unread", time.Minute, nil), // no cmdline field at all
		observation("empty", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.KernelCmdline = str("")
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	byNode := map[string]memorypolicy.Observation{}
	for _, obs := range got {
		byNode[obs.Node] = obs
	}

	if byNode["unread"].KernelParams != nil {
		t.Errorf("an absent cmdline produced params %v, want nil", byNode["unread"].KernelParams)
	}
	if byNode["empty"].KernelParams == nil {
		t.Error("an empty cmdline produced a nil map; \"read fine, nothing set\" has collapsed into \"could not read\"")
	}
	if len(byNode["empty"].KernelParams) != 0 {
		t.Errorf("an empty cmdline parsed to %v, want an empty map", byNode["empty"].KernelParams)
	}
}

func TestObserveTreatsAStaleObservationAsUnread(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", DefaultMaxAge+time.Second, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.KernelCmdline = str("amd_iommu=off")
			s.GTTTotalBytes = i64(124 << 30)
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	want := []memorypolicy.Observation{{Node: "halo-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a stale observation was still believed: %+v", got)
	}
}

func TestObserveListsANodeTheAgentNeverReached(t *testing.T) {
	o := observer(t, amdNode("halo-1"), amdNode("halo-2"),
		observation("halo-1", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.KernelCmdline = str("amd_iommu=off")
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Observe() returned %d observations for 2 targeted nodes; "+
			"an unreached node fell out of the compliance calculus", len(got))
	}
	// Sorted by name: halo-1 then halo-2.
	if got[1].Node != "halo-2" || got[1].KernelParams != nil || got[1].EffectiveGPUMemoryGiB != nil {
		t.Errorf("the unreached node is not an all-nil observation: %+v", got[1])
	}
}

func TestObserveAcceptsAFutureTimestampAsFresh(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", -30*time.Second, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.GTTTotalBytes = i64(124 << 30)
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got[0].EffectiveGPUMemoryGiB == nil {
		t.Error("an observation slightly in the future (clock skew) was discarded")
	}
}

func TestObserveDefaultSelectorIsTheAMDGPULabel(t *testing.T) {
	plain := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cpu-only"}}
	o := observer(t, amdNode("halo-1"), plain)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(got) != 1 || got[0].Node != "halo-1" {
		t.Errorf("the empty selector targeted %+v, want exactly the NFD-labelled node", got)
	}
}

func TestObserveHonoursAnExplicitSelector(t *testing.T) {
	labelled := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "special",
		Labels: map[string]string{"pool": "halo"},
	}}
	o := observer(t, amdNode("halo-1"), labelled)

	got, err := o.Observe(context.Background(), map[string]string{"pool": "halo"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(got) != 1 || got[0].Node != "special" {
		t.Errorf("an explicit selector targeted %+v, want exactly the matching node", got)
	}
}

func TestObserveIgnoresObservationsForUntargetedNodes(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		// An observation for a node that matches nothing — gone from the
		// cluster, or never in the target set. Its readings must not leak
		// into anyone's verdict.
		observation("retired-node", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.KernelCmdline = str("amd_iommu=on")
		}),
	)

	got, err := o.Observe(context.Background(), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(got) != 1 || got[0].Node != "halo-1" {
		t.Errorf("Observe() = %+v, want only the targeted node", got)
	}
}

func TestObserveHealthMapsEveryReading(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.GPUBusyPercent = i32(97)
			s.SCLKMHz = i32(2900)
			s.RatedSCLKMHz = i32(2900)
			// PermissionFaults and QueueCreateFailures stay nil: no validated
			// kernel-log reader exists, so the probes must see nil, not zero.
		}),
	)

	got, err := o.ObserveHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObserveHealth: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d observations, want 1", len(got))
	}
	h := got[0]
	if h.GPUBusyPercent == nil || *h.GPUBusyPercent != 97 ||
		h.SCLKMHz == nil || *h.SCLKMHz != 2900 ||
		h.RatedSCLKMHz == nil || *h.RatedSCLKMHz != 2900 {
		t.Errorf("sysfs readings did not survive the mapping: %+v", h)
	}
	if h.PermissionFaults != nil || h.QueueCreateFailures != nil {
		t.Errorf("kernel-log counters arrived as %v/%v with no reader in existence; "+
			"a nil reading was invented into a value", h.PermissionFaults, h.QueueCreateFailures)
	}
}

func TestObserveRuntimeMapsEveryReading(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"), amdNode("no-kfd"), amdNode("unread"),
		observation("halo-1", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.DRMRenderNode = str("renderD128")
			s.GFXTargetVersion = i32(110501)
		}),
		// Present-and-empty and present-and-zero are readings, and must not
		// arrive as nil: they are what makes a node observed-unable rather
		// than unread.
		observation("no-kfd", time.Minute, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.DRMRenderNode = str("")
			s.GFXTargetVersion = i32(0)
		}),
		observation("unread", time.Minute, nil),
	)

	got, err := o.ObserveRuntime(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObserveRuntime: %v", err)
	}
	byNode := map[string]runtimeprofile.Observation{}
	for _, obs := range got {
		byNode[obs.Node] = obs
	}
	if len(byNode) != 3 {
		t.Fatalf("got %d observations, want 3", len(byNode))
	}

	if r := byNode["halo-1"]; r.RenderNode == nil || *r.RenderNode != "renderD128" ||
		r.GFXTargetVersion == nil || *r.GFXTargetVersion != 110501 {
		t.Errorf("readings did not survive the mapping: %+v", r)
	}
	if r := byNode["no-kfd"]; r.RenderNode == nil || *r.RenderNode != "" ||
		r.GFXTargetVersion == nil || *r.GFXTargetVersion != 0 {
		t.Errorf("observed absences collapsed into unread: %+v", r)
	}
	if r := byNode["unread"]; r.RenderNode != nil || r.GFXTargetVersion != nil {
		t.Errorf("an unread node arrived with readings: %+v", r)
	}
}

func TestObserveRuntimeTreatsAStaleObservationAsUnread(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", time.Hour, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.DRMRenderNode = str("renderD128")
			s.GFXTargetVersion = i32(110501)
		}),
	)

	got, err := o.ObserveRuntime(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObserveRuntime: %v", err)
	}
	if r := got[0]; r.RenderNode != nil || r.GFXTargetVersion != nil {
		t.Errorf("an hour-old reading is still being judged: %+v", r)
	}
}

func TestObserveHealthTreatsAStaleObservationAsUnread(t *testing.T) {
	o := observer(t,
		amdNode("halo-1"),
		observation("halo-1", time.Hour, func(s *halov1alpha1.HaloNodeObservationSpec) {
			s.GPUBusyPercent = i32(97)
			s.SCLKMHz = i32(200)
			s.RatedSCLKMHz = i32(2900)
		}),
	)

	got, err := o.ObserveHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("ObserveHealth: %v", err)
	}
	h := got[0]
	if h.GPUBusyPercent != nil || h.SCLKMHz != nil || h.RatedSCLKMHz != nil {
		t.Errorf("an hour-old reading is still being fed to the probes: %+v", h)
	}
}

// TestObserveErrorsPropagate: a listing failure must surface as an error, which
// the reconciler renders as ObservationFailed/Unknown. Swallowing it into an
// empty result would report "no node has been observed yet" — a different and
// wrong story about why nothing is known.
func TestObserveErrorsPropagate(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(amdNode("halo-1")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return boom
			},
		}).
		Build()
	o := &Observer{Reader: c, Now: func() time.Time { return fixedNow }}

	if _, err := o.Observe(context.Background(), nil); !errors.Is(err, boom) {
		t.Errorf("Observe() error = %v, want the listing failure", err)
	}
	if _, err := o.ObserveHealth(context.Background(), nil); !errors.Is(err, boom) {
		t.Errorf("ObserveHealth() error = %v, want the listing failure", err)
	}
}

func TestGiBFloor(t *testing.T) {
	testCases := []struct {
		name  string
		bytes *int64
		want  *int32
	}{
		{"nil stays nil", nil, nil},
		{"an exact GiB count", i64(124 << 30), i32(124)},
		{"one byte short rounds down", i64(124<<30 - 1), i32(123)},
		{"zero bytes is zero GiB, present", i64(0), i32(0)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := gibFloor(tc.bytes)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("gibFloor = %d, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("gibFloor = nil, want %d", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("gibFloor = %d, want %d", *got, *tc.want)
			}
		})
	}
}
