// Package controller implements the HetznerConfig image reconciler.
//
// Architecture (DES-004):
//
//	HetznerConfig (image: "golden:cis")
//	  → Controller detects golden: prefix
//	  → Finds parent Cluster → extracts Cloud Credential → gets HCLOUD_TOKEN
//	  → Checks Hetzner API for cached snapshot (label query)
//	  → Cache miss → creates K8s Job (Packer builder image)
//	  → Job builds snapshot on Hetzner Cloud
//	  → Controller detects Job completion → queries Hetzner API → gets snapshot ID
//	  → Patches HetznerConfig (image: "12345") → unpauses machine pool
//
// Idempotency guarantees:
//   - Job names are deterministic (hash of config name + spec) — no duplicates
//   - Credential Secrets have OwnerReference → garbage collected with Job
//   - Leader election prevents concurrent controller instances
//   - Check-before-create prevents orphaned resources
//   - Exponential backoff prevents API hammering on failures
package controller

import (
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// goldenPrefix is the convention prefix in HetznerConfig.image field.
	// "golden:cis" → build CIS-hardened snapshot.
	// "golden:base" → build base snapshot (no CIS).
	goldenPrefix = "golden:"

	// Annotations set on HetznerConfig by the controller.
	annotationStatus      = "hcloud-image.cattle.io/status"        // building | resolved | failed
	annotationJob         = "hcloud-image.cattle.io/job-name"      // K8s Job name
	annotationSnapshot    = "hcloud-image.cattle.io/snapshot-id"   // resolved snapshot ID
	annotationSpec        = "hcloud-image.cattle.io/original-spec" // original golden:xxx value
	annotationError       = "hcloud-image.cattle.io/error"         // human-readable error message (set on failed)
	annotationRKE2Version = "hcloud-image.cattle.io/rke2-version"  // effective RKE2 version used for build/lookup

	// Rancher namespaces.
	credentialNS = "cattle-global-data" // Cloud Credential secrets
	fleetNS      = "fleet-default"      // Cluster + HetznerConfig resources

	// Cloud Credential secret key for Hetzner token.
	hcloudTokenKey = "hetznercredentialConfig-apiToken" //nolint:gosec // Not a credential, just the key name in Rancher's secret schema.
)

// Kubernetes GVKs for Rancher CRDs.
var (
	hetznerConfigGVK = schema.GroupVersionKind{
		Group:   "rke-machine-config.cattle.io",
		Version: "v1",
		Kind:    "HetznerConfig",
	}
	clusterGVK = schema.GroupVersionKind{
		Group:   "provisioning.cattle.io",
		Version: "v1",
		Kind:    "Cluster",
	}
)

// ── Configuration ────────────────────────────────────────────────────────────

// Config holds controller runtime configuration from environment variables.
//
// DECISION: No hardcoded version/location defaults in this file.
// Why: Single source of truth for the controller path is chart/hcloud-image-controller/values.yaml.
//
//	Helm values → deployment env vars → this Config struct → builder Job env vars.
//	For standalone Packer builds, defaults live in rke2-base.pkr.hcl.
type Config struct {
	BuilderImage string // Docker image for builder Jobs
	JobNamespace string // Namespace where builder Jobs are created
	RKE2Version  string // RKE2 version for builds (required — used for cache lookup)
	Location     string // Hetzner location for build servers (optional — Packer default if empty)
	ServerType   string // Hetzner server type for build servers (optional — Packer default if empty)
	BaseImage    string // OS base image (optional — Packer default if empty)
}

// ── Reconciler ───────────────────────────────────────────────────────────────

// HetznerConfigReconciler watches HetznerConfig resources and resolves golden: images.
type HetznerConfigReconciler struct {
	client.Client
	Config     Config
	HTTPClient *http.Client
}

// ── Hetzner API Types ────────────────────────────────────────────────────────

// hetznerImageResponse represents the Hetzner Cloud API response for image listing.
type hetznerImageResponse struct {
	Images []hetznerImage `json:"images"`
}

type hetznerImage struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Created string `json:"created"`
}
