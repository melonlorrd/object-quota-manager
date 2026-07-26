package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	RegisterMetrics()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName = "node-local"
	}

	leaser := NewK8sLeaseManager(mgr.GetClient(), nodeName, 1000)
	fdSyncer := NewSyncer(mgr.GetClient(), NewStubManager(), NewCachedPodResolver(NewPodResolver()), setupLog.WithName("fd-syncer")).WithLeaser(leaser, nodeName)
	defer fdSyncer.Close()

	if err := fdSyncer.LoadBPF(); err != nil {
		setupLog.Info("eBPF load failed, running without enforcement", "error", err)
	}

	if err := fdSyncer.StartBreachWatcher(); err != nil {
		setupLog.Info("breach watcher not available", "error", err)
	}

	ctx := ctrl.SetupSignalHandler()
	fdSyncer.StartCgroupScanner(ctx)

	// Start the lease renewal background loop
	go leaser.StartLeaseRenewal(ctx)

	if err := (&ObjectQuotaReconciler{
		Client:   mgr.GetClient(),
		FDSyncer: fdSyncer,
	}).SetupWithManager(mgr, setupLog); err != nil {
		setupLog.Error(err, "unable to create ObjectQuota controller")
		os.Exit(1)
	}

	if err := (&PodReconciler{
		Client:   mgr.GetClient(),
		FDSyncer: fdSyncer,
		Recorder: mgr.GetEventRecorder("object-quota-manager"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create Pod controller")
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

	setupLog.Info("starting object-quota-manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
