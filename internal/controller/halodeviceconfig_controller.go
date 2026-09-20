// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

// Component names as they appear in status, in status order.
const (
	componentMemoryPolicy   = "memory-policy"
	componentRuntimeProfile = "runtime-profile"
	componentHealthChecks   = "health-checks"
	componentBootHold       = "boot-hold"
)

// HaloDeviceConfigReconciler reconciles a HaloDeviceConfig.
type HaloDeviceConfigReconciler struct {
	client.Client

	// MemoryObserver reads the memory configuration of the targeted nodes. A
	// nil observer means no source is wired up, which is reported as
	// NotObserved rather than quietly treated as compliance.
	MemoryObserver NodeObserver

	// BootHoldObserver reads the persisted boot configuration of the targeted
	// nodes. Nil means no source is wired up, reported as NotObserved rather
	// than quietly treated as a cleared hold.
	BootHoldObserver BootHoldObserver

	// HealthObserver reads the health signals of the targeted nodes. A nil
	// observer is reported as NotObserved, never as a clean bill of health.
	HealthObserver HealthObserver

	// RecheckPeriod re-runs the evaluation this often even with no cluster
	// events. Observations expire by aging past the freshness window, which
	// produces no event — a dead agent goes quiet, it does not announce
	// itself — so without a periodic recheck a condition could sit at True
	// on the strength of an observation nobody can refresh. Zero disables
	// the backstop, for tests that drive reconciles by hand.
	RecheckPeriod time.Duration
}

// component pairs a stanza name with its resolved enablement.
type component struct {
	name    string
	enabled bool
}

// notYetImplemented lists the stanzas whose controllers do not exist yet, in
// status order. memory-policy and health-checks are absent from it because
// they are implemented.
func notYetImplemented(spec halov1alpha1.HaloDeviceConfigSpec) []component {
	return []component{
		{componentRuntimeProfile, true},
	}
}

// componentStatuses reports each not-yet-implemented component honestly:
// Disabled when the spec disables it, NotImplemented while its controller does
// not exist yet. No component is ever reported Ready without an observation.
func componentStatuses(spec halov1alpha1.HaloDeviceConfigSpec, now metav1.Time) []halov1alpha1.ComponentStatus {
	var out []halov1alpha1.ComponentStatus
	for _, c := range notYetImplemented(spec) {
		st := halov1alpha1.ComponentStatus{Name: c.name, LastObserved: now}
		if !c.enabled {
			st.State = halov1alpha1.ComponentDisabled
		} else {
			st.State = halov1alpha1.ComponentNotImplemented
			st.Message = "this component's controller is not implemented yet; see the repository issue tracker"
		}
		out = append(out, st)
	}
	return out
}

// readyCondition summarises the components into the overall Ready condition.
// Ready is True only when at least one component is Ready and none is
// Degraded, NotObserved or NotImplemented. A Disabled component neither
// blocks readiness nor establishes it.
func readyCondition(components []halov1alpha1.ComponentStatus, generation int64) metav1.Condition {
	condition := metav1.Condition{
		Type:               halov1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
	}
	var ready, blocked int
	for _, c := range components {
		switch c.State {
		case halov1alpha1.ComponentReady:
			ready++
		case halov1alpha1.ComponentDisabled:
		default:
			blocked++
		}
	}
	switch {
	case blocked > 0:
		condition.Reason = "ComponentsNotReady"
		condition.Message = "not every enabled component is Ready"
	case ready == 0:
		condition.Reason = "NothingEnabled"
		condition.Message = "no component is enabled, so readiness is not established"
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "AllComponentsReady"
		condition.Message = "every enabled component is Ready"
	}
	return condition
}

// +kubebuilder:rbac:groups=amd.halo.glimmer.ai,resources=halodeviceconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=amd.halo.glimmer.ai,resources=halodeviceconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=amd.halo.glimmer.ai,resources=halonodeobservations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile records the observed state of every component on the CR.
func (r *HaloDeviceConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cfg halov1alpha1.HaloDeviceConfig
	if err := r.Get(ctx, req.NamespacedName, &cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	spec := cfg.Spec.WithDefaults()
	now := metav1.Now()

	memoryStatus, memoryCondition := evaluateMemoryPolicy(ctx, r.MemoryObserver, &cfg, spec, now)
	healthStatus, healthCondition := evaluateHealthChecks(ctx, r.HealthObserver, &cfg, spec, now)
	bootHoldStatus, bootHoldCondition := evaluateBootHold(ctx, r.BootHoldObserver, &cfg, spec, now)

	cfg.Status.ObservedGeneration = cfg.Generation
	cfg.Status.Components = append([]halov1alpha1.ComponentStatus{memoryStatus, healthStatus, bootHoldStatus},
		componentStatuses(spec, now)...)
	meta.SetStatusCondition(&cfg.Status.Conditions, memoryCondition)
	meta.SetStatusCondition(&cfg.Status.Conditions, healthCondition)
	meta.SetStatusCondition(&cfg.Status.Conditions, bootHoldCondition)
	meta.SetStatusCondition(&cfg.Status.Conditions,
		readyCondition(cfg.Status.Components, cfg.Generation))

	if err := r.Status().Update(ctx, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("recorded component states", "components", len(cfg.Status.Components))
	return ctrl.Result{RequeueAfter: r.RecheckPeriod}, nil
}

// SetupWithManager registers the controller.
//
// Beside its own CRD, the controller re-evaluates every config when a
// HaloNodeObservation changes — that is how a fresh reading becomes a fresh
// condition — and when the set of targeted nodes can have changed. Node
// updates are filtered to label changes: the target join is by label, and
// reacting to every heartbeat would reconcile the world on a timer that is
// not ours.
func (r *HaloDeviceConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&halov1alpha1.HaloDeviceConfig{}).
		Watches(&halov1alpha1.HaloNodeObservation{},
			handler.EnqueueRequestsFromMapFunc(r.allConfigs)).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.allConfigs),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Named("halodeviceconfig").
		Complete(r)
}

// allConfigs fans a node-side event out to every HaloDeviceConfig. Configs are
// cluster-scoped and few — typically one — and which nodes each one targets is
// a selector evaluated at observe time, so requeueing them all is both cheap
// and simpler than reverse-mapping selectors.
func (r *HaloDeviceConfigReconciler) allConfigs(ctx context.Context, _ client.Object) []reconcile.Request {
	var list halov1alpha1.HaloDeviceConfigList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "listing configs to requeue after a node event")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
		})
	}
	return requests
}
