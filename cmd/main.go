// llmkube-kueue: external Kueue integration for LLMKube InferenceServices.
// Two entrypoints in one binary, mirroring konflux-ci/tekton-kueue:
//
//	llmkube-kueue controller  - leader-elected manager; hosts the
//	                            jobframework reconciler (issue #2).
//	llmkube-kueue webhook     - webhook server manager; hosts the suspend
//	                            defaulter (issue #3).
//
// Both are placeholder no-op managers in the scaffold slice (issue #1).
package main

import (
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/defilantech/llmkube-kueue/internal/scheme"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: llmkube-kueue <controller|webhook> [flags]")
		os.Exit(2)
	}
	sub, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	metricsAddr := fs.String("metrics-bind-address", ":8443", "metrics endpoint bind address")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "health probe bind address")
	certDir := fs.String("webhook-cert-dir", "", "directory holding tls.crt/tls.key for the webhook server")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(fs)
	_ = fs.Parse(args)

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup").WithValues("subcommand", sub)

	s, err := scheme.New()
	if err != nil {
		log.Error(err, "building scheme")
		os.Exit(1)
	}

	opts := manager.Options{
		Scheme:                 s,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
	}

	switch sub {
	case "controller":
		opts.LeaderElection = true
		opts.LeaderElectionID = "llmkube-kueue.defilantech.dev"
	case "webhook":
		opts.LeaderElection = false
		wh := webhook.NewServer(webhook.Options{Port: 9443, CertDir: *certDir})
		opts.WebhookServer = wh
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want controller or webhook)\n", sub)
		os.Exit(2)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "adding healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "adding readyz")
		os.Exit(1)
	}

	// Scaffold slice: no controllers or webhook handlers registered yet.
	// Issue #2 wires the jobframework reconciler under "controller";
	// issue #3 registers the suspend defaulter under "webhook".
	log.Info("starting no-op manager (scaffold)")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
