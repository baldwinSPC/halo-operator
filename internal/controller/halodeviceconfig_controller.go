// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
)

// HaloDeviceConfigReconciler reconciles a HaloDeviceConfig.
type HaloDeviceConfigReconciler struct {
	client.Client
}

// component pairs a stanza name with its resolved enablement.
type component struct {
	name    string
	enabled bool
}

// components lists every stanza of a defaulted spec, in status order.
func components(spec halov1alpha1.HaloDeviceConfigSpec) []component {
	return []component{
		{"memory-policy", *spec.MemoryPolicy.Enabled},
		{"runtime-profile", true},
		{"health-checks", *spec.HealthChecks.Enabled},
	}
}

// componentStatuses reports each component honestly: Disabled when the
// spec disables it, NotImplemented while its controller does not exist
// yet. No component is ever reported Ready without an observation.
func componentStatuses(spec halov1alpha1.HaloDeviceConfigSpec, now metav1.Time) []halov1alpha1.ComponentStatus {
	var out []halov1alpha1.ComponentStatus
	for _, c := range components(spec) {
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

// +kubebuilder:rbac:groups=amd.halo.glimmer.ai,resources=halodeviceconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=amd.halo.glimmer.ai,resources=halodeviceconfigs/status,verbs=get;update;patch

// Reconcile records the observed state of every component on the CR.
func (r *HaloDeviceConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cfg halov1alpha1.HaloDeviceConfig
	if err := r.Get(ctx, req.NamespacedName, &cfg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	spec := cfg.Spec.WithDefaults()
	now := metav1.Now()
	cfg.Status.ObservedGeneration = cfg.Generation
	cfg.Status.Components = componentStatuses(spec, now)
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               halov1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "ComponentsNotImplemented",
		Message:            "no component controllers are implemented yet",
		ObservedGeneration: cfg.Generation,
	})

	if err := r.Status().Update(ctx, &cfg); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("recorded component states", "components", len(cfg.Status.Components))
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller.
func (r *HaloDeviceConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&halov1alpha1.HaloDeviceConfig{}).
		Named("halodeviceconfig").
		Complete(r)
}
