// Copyright 2026 the AMD Halo Operator authors
// SPDX-License-Identifier: Apache-2.0

// The halo-node-agent: the one process in this operator that runs on a node.
// On an interval it reads the kernel command line and the amdgpu sysfs
// counters — read-only, unprivileged — and publishes what it found as a
// HaloNodeObservation for the controller to judge. It decides nothing itself.
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	halov1alpha1 "github.com/baldwinSPC/halo-operator/api/v1alpha1"
	"github.com/baldwinSPC/halo-operator/internal/nodeagent"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(halov1alpha1.AddToScheme(scheme))
}

func main() {
	var nodeName, procRoot, sysRoot, etcRoot, espRoot string
	var interval time.Duration
	var once bool
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"),
		"Name of the node this agent runs on. Defaults to $NODE_NAME, which the DaemonSet sets from the downward API.")
	flag.StringVar(&procRoot, "proc-root", "/proc",
		"Where the host's /proc is visible. Only <proc-root>/cmdline is read.")
	flag.StringVar(&etcRoot, "etc-root", "/etc",
		"where the host's /etc is visible; only <etc-root>/kernel/cmdline is read")
	flag.StringVar(&espRoot, "esp-root", "/efi",
		"where the host's EFI system partition is visible, mounted read-only; only <esp-root>/loader/entries is listed")
	flag.StringVar(&sysRoot, "sys-root", "/sys",
		"Where the host's /sys is visible.")
	flag.DurationVar(&interval, "interval", time.Minute,
		"How often to read the node and publish.")
	flag.BoolVar(&once, "once", false,
		"Collect and publish a single observation, then exit. For bring-up and debugging.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("halo-node-agent")

	if nodeName == "" {
		log.Info("no node name: pass --node-name or set NODE_NAME")
		os.Exit(1)
	}

	// A direct client, no cache: the agent makes one read and one write per
	// interval and should not hold watches open from every node in the fleet.
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to build a cluster client")
		os.Exit(1)
	}

	collector := nodeagent.Collector{ProcRoot: procRoot, SysRoot: sysRoot, EtcRoot: etcRoot, ESPRoot: espRoot}
	publisher := nodeagent.Publisher{Client: c, NodeName: nodeName}
	ctx := ctrl.SetupSignalHandler()

	log.Info("starting", "node", nodeName, "interval", interval, "procRoot", procRoot, "sysRoot", sysRoot, "etcRoot", etcRoot, "espRoot", espRoot)

	if once {
		if err := cycle(ctx, log, collector, publisher); err != nil {
			os.Exit(1)
		}
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// Errors are logged and retried on the next tick, not fatal: a
		// briefly unreachable API server should not crash-loop every agent
		// in the fleet. Staleness is the controller's to notice.
		_ = cycle(ctx, log, collector, publisher)
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-ticker.C:
		}
	}
}

// cycle is one read-and-publish pass.
func cycle(ctx context.Context, log logr.Logger, collector nodeagent.Collector, publisher nodeagent.Publisher) error {
	reading := collector.Collect()
	at := metav1.Now()
	if len(reading.Messages) > 0 {
		// Absences are visible on the published observation too; logging them
		// here keeps the story on the node when the API server is the problem.
		log.Info("some readings could not be taken", "messages", reading.Messages)
	}
	if err := publisher.Publish(ctx, reading, at); err != nil {
		log.Error(err, "publishing the observation failed")
		return err
	}
	log.V(1).Info("published", "observedAt", at)
	return nil
}
