// Hcloud Image Controller — watches HetznerConfig CRDs for "golden:*" image convention,
// automatically builds Hetzner Cloud snapshots via Packer K8s Jobs, and patches the
// resolved snapshot ID back into the HetznerConfig.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/mbilan1/rancher-hcloud-image-controller/internal/controller"
)

// Set via -ldflags at build time.
var version = "dev"

func main() {
	opts := zap.Options{Development: true}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg := controller.Config{
		BuilderImage: getEnv("BUILDER_IMAGE", "ghcr.io/mbilan1/hcloud-image-builder:latest"),
		JobNamespace: getEnv("JOB_NAMESPACE", "hcloud-image-system"),
		RKE2Version:  os.Getenv("DEFAULT_RKE2_VERSION"),
		Location:     os.Getenv("DEFAULT_LOCATION"),
		ServerType:   os.Getenv("DEFAULT_SERVER_TYPE"),
		BaseImage:    os.Getenv("DEFAULT_BASE_IMAGE"),
	}

	// RKE2Version is required — the cache lookup (findSnapshot) uses it as a label selector.
	// Without it, we cannot determine if a matching snapshot already exists.
	if cfg.RKE2Version == "" {
		setupLog.Error(fmt.Errorf("DEFAULT_RKE2_VERSION is required"), "set via Helm values.defaults.rke2Version")
		os.Exit(1)
	}

	setupLog.Info("starting hcloud-image-controller",
		"version", version,
		"builderImage", cfg.BuilderImage,
		"jobNamespace", cfg.JobNamespace,
		"defaultRKE2Version", cfg.RKE2Version,
	)

	// DECISION: Enable leader election to prevent concurrent controller instances.
	// Why: Without it, two replicas (e.g., during rolling update) can both create
	//      Jobs for the same HetznerConfig, leading to duplicate builds and wasted resources.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		LeaderElection:   true,
		LeaderElectionID: "hcloud-image-controller.cattle.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	reconciler := &controller.HetznerConfigReconciler{
		Client:     mgr.GetClient(),
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	setupLog.Info("manager starting")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// getEnv returns the value of an environment variable, or a fallback default.
func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
