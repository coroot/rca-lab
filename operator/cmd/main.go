// The rca-lab operator binary. Subcommands:
//
//	manager (default) — run the FailureScenario controller manager
//	dbtool            — query-loop runner used by Workload scenario Jobs
package main

import (
	"fmt"
	"os"
	"strings"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
	"github.com/coroot/rca-lab/operator/internal/api"
	"github.com/coroot/rca-lab/operator/internal/controller"
	"github.com/coroot/rca-lab/operator/internal/dbtool"
)

func main() {
	cmd := "manager"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "manager":
		runManager(args)
	case "dbtool":
		if err := dbtool.Run(args); err != nil {
			fmt.Fprintln(os.Stderr, "dbtool:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want: manager | dbtool)\n", cmd)
		os.Exit(2)
	}
}

func runManager(_ []string) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	setupLog := ctrl.Log.WithName("setup")

	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		setupLog.Error(fmt.Errorf("OPERATOR_IMAGE environment variable is empty"),
			"OPERATOR_IMAGE must be set to the operator's own image (used as the default Workload image)")
		os.Exit(1)
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	scheme := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "unable to add rcalab.dev scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		HealthProbeBindAddress:  ":8081",
		Metrics:                 metricsserver.Options{BindAddress: ":8082"},
		LeaderElection:          true,
		LeaderElectionID:        "rca-lab-operator",
		LeaderElectionNamespace: namespace,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{namespace: {}},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.FailureScenarioReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor("rca-lab-operator"),
		OperatorImage: operatorImage,
		Namespace:     namespace,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up FailureScenario controller")
		os.Exit(1)
	}

	if err := mgr.Add(&controller.Sweeper{Client: mgr.GetClient(), Namespace: namespace}); err != nil {
		setupLog.Error(err, "unable to add startup sweeper")
		os.Exit(1)
	}

	if err := mgr.Add(api.NewServer(mgr.GetClient(), ":8080")); err != nil {
		setupLog.Error(err, "unable to add API/UI server")
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

	setupLog.Info("starting manager", "namespace", namespace, "operatorImage", operatorImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
