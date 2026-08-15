// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// The halo-operator manager: registers the HaloDeviceConfig controller
// and serves health probes and metrics.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/controller"
	"github.com/baldwinSPC/halo-operator/internal/observe"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(halov1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var maxObservationAge time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.DurationVar(&maxObservationAge, "max-observation-age", observe.DefaultMaxAge,
		"How old a node's published observation may be and still count as knowledge. Older readings are treated as no reading, and the affected conditions go Unknown.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "halo-operator.amd.halo.glimmer.ai",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// One observer serves both interfaces: the memory policy and the health
	// checks read the same published observations, differently.
	observer := &observe.Observer{Reader: mgr.GetClient(), MaxAge: maxObservationAge}
	if err := (&controller.HaloDeviceConfigReconciler{
		Client:         mgr.GetClient(),
		MemoryObserver: observer,
		HealthObserver: observer,
		// Recheck at half the freshness window so an observation can never
		// expire more than half a window before anyone notices.
		RecheckPeriod: maxObservationAge / 2,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HaloDeviceConfig")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
