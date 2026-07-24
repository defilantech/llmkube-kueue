// llmkube-kueue: external Kueue integration for LLMKube InferenceServices.
// Two entrypoints in one binary, mirroring konflux-ci/tekton-kueue:
//
//	llmkube-kueue controller  - leader-elected manager; hosts the
//	                            jobframework reconciler (issue #2).
//	llmkube-kueue webhook     - webhook server manager; hosts the suspend
//	                            defaulter (issue #3).
//
// "webhook" remains a placeholder no-op manager until issue #3 registers the
// suspend defaulter.
package main

import (
	"flag"
	"fmt"
	"os"

	kubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"

	kueuecontroller "github.com/defilantech/llmkube-kueue/internal/controller"
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

	// ctrl.SetupSignalHandler may only be called once per process; hoist a
	// single ctx here and share it across both subcommand branches below.
	ctx := ctrl.SetupSignalHandler()

	switch sub {
	case "controller":
		// The jobframework reconciler owns the Workload lifecycle for
		// InferenceServices: it creates the Workload, translates Kueue
		// admission into clearing spec.suspend, and deactivates the
		// Workload at scale-to-zero (quota release). Kueue's own manager
		// never touches InferenceServices; it must list this integration
		// under integrations.externalFrameworks (hack/kueue-config.yaml).
		kubeClient, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
		if err != nil {
			log.Error(err, "building kube client for the event broadcaster")
			os.Exit(1)
		}
		broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: kubeClient.EventsV1()})
		broadcaster.StartRecordingToSink(ctx.Done())
		recorder := broadcaster.NewRecorder(mgr.GetScheme(), "llmkube-kueue")

		if err := jobframework.SetupWorkloadOwnerIndex(ctx, mgr.GetFieldIndexer(), kueuecontroller.InferenceServiceGVK()); err != nil {
			log.Error(err, "setting up the workload owner index")
			os.Exit(1)
		}
		factory := jobframework.NewGenericReconcilerFactory(kueuecontroller.NewInferenceServiceJob)
		rec, err := factory(ctx, mgr.GetClient(), mgr.GetFieldIndexer(), recorder)
		if err != nil {
			log.Error(err, "building the jobframework reconciler")
			os.Exit(1)
		}
		if err := rec.SetupWithManager(mgr); err != nil {
			log.Error(err, "registering the jobframework reconciler")
			os.Exit(1)
		}
		log.Info("starting controller with the InferenceService jobframework reconciler")
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "manager exited")
			os.Exit(1)
		}
	case "webhook":
		// Placeholder until issue #3 registers the suspend defaulter.
		log.Info("starting no-op webhook manager (defaulter lands with issue #3)")
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "manager exited")
			os.Exit(1)
		}
	}
}
