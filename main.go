// Golden Image Controller — watches HetznerConfig CRDs for "golden:*" image convention,
// automatically builds Hetzner Cloud snapshots via Packer K8s Jobs, and patches the
// resolved snapshot ID back into the HetznerConfig.
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
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// goldenPrefix is the convention prefix in HetznerConfig.image field.
	// "golden:cis" → build CIS-hardened snapshot
	// "golden:base" → build base snapshot (no CIS)
	goldenPrefix = "golden:"

	// Annotations set on HetznerConfig by the controller.
	annotationStatus   = "hcloud-image.cattle.io/status"       // building | resolved | failed
	annotationJob      = "hcloud-image.cattle.io/job-name"     // K8s Job name
	annotationSnapshot = "hcloud-image.cattle.io/snapshot-id"  // resolved snapshot ID
	annotationSpec     = "hcloud-image.cattle.io/original-spec" // original golden:xxx value

	// Rancher namespaces.
	credentialNS = "cattle-global-data" // Cloud Credential secrets
	fleetNS      = "fleet-default"      // Cluster + HetznerConfig resources

	// Cloud Credential secret key for Hetzner token.
	hcloudTokenKey = "hetznercredentialConfig-apiToken"
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
//      Helm values → deployment env vars → this Config struct → builder Job env vars.
//      For standalone Packer builds, defaults live in rke2-base.pkr.hcl.
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

// ── main ─────────────────────────────────────────────────────────────────────

func main() {
	opts := zap.Options{Development: true}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg := Config{
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

	reconciler := &HetznerConfigReconciler{
		Client:     mgr.GetClient(),
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Watch unstructured HetznerConfig CRDs in all namespaces.
	// The reconciler filters to fleet-default only.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(hetznerConfigGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(obj).
		Complete(reconciler); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	setupLog.Info("manager starting")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// ── Reconcile ────────────────────────────────────────────────────────────────

// Reconcile handles a single HetznerConfig resource.
//
// State machine:
//
//	(no annotation) → check golden: prefix → cache check → create Job → "building"
//	"building"      → check Job status → Job done → query snapshot → "resolved"
//	"resolved"      → no-op (image already patched)
//	"failed"        → no-op (operator must clear annotation to retry)
func (r *HetznerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Only process fleet-default namespace (where Rancher creates HetznerConfigs).
	if req.Namespace != fleetNS {
		return ctrl.Result{}, nil
	}

	// Fetch the HetznerConfig.
	hc := &unstructured.Unstructured{}
	hc.SetGroupVersionKind(hetznerConfigGVK)
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check image field for golden: prefix.
	image, found, _ := unstructured.NestedString(hc.Object, "image")
	if !found {
		return ctrl.Result{}, nil
	}

	// Read current status annotation.
	annotations := hc.GetAnnotations()
	status := ""
	if annotations != nil {
		status = annotations[annotationStatus]
	}

	// If image doesn't have golden: prefix AND status is resolved → done.
	if !strings.HasPrefix(image, goldenPrefix) {
		if status == "resolved" {
			return ctrl.Result{}, nil
		}
		// Image was changed externally while building — reset.
		if status == "building" {
			log.Info("image changed externally during build, resetting", "image", image)
			return ctrl.Result{}, r.setAnnotations(ctx, hc, map[string]string{annotationStatus: ""})
		}
		return ctrl.Result{}, nil
	}

	// Image starts with golden: — needs resolution.
	switch status {
	case "resolved":
		// Should not happen (image would have been patched), but handle gracefully.
		return ctrl.Result{}, nil
	case "failed":
		// Operator must clear annotation to retry.
		log.Info("image build failed, waiting for manual intervention", "config", req.Name)
		return ctrl.Result{}, nil
	case "building":
		return r.handleBuilding(ctx, hc)
	default:
		return r.handleNew(ctx, hc, image)
	}
}

// ── State Handlers ───────────────────────────────────────────────────────────

// handleNew processes a newly detected golden: image request.
func (r *HetznerConfigReconciler) handleNew(ctx context.Context, hc *unstructured.Unstructured, image string) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	configName := hc.GetName()

	// Parse golden spec.
	spec := strings.TrimPrefix(image, goldenPrefix)
	enableCIS := spec == "cis"
	log.Info("new image build request", "config", configName, "spec", spec, "enableCIS", enableCIS)

	// Find parent Cluster to get Cloud Credential.
	cluster, err := r.findClusterForConfig(ctx, configName)
	if err != nil {
		log.Error(err, "cannot find parent cluster")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Get HCLOUD_TOKEN from Cloud Credential secret.
	token, err := r.getCloudCredentialToken(ctx, cluster)
	if err != nil {
		log.Error(err, "cannot get cloud credential token")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check for cached snapshot via Hetzner API.
	snapshotID, err := r.findSnapshot(ctx, token, r.Config.RKE2Version, enableCIS)
	if err != nil {
		log.Error(err, "error checking snapshot cache")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if snapshotID != "" {
		log.Info("cache hit — using existing snapshot", "snapshotID", snapshotID)
		return r.resolveImage(ctx, hc, cluster, snapshotID, image)
	}

	// Cache miss — check if a builder Job already exists (idempotency guard).
	// DECISION: Use deterministic job name derived from config name + spec hash.
	// Why: Timestamp-based names cause collisions at second precision and
	//      create orphaned Jobs on annotation patch failure. A deterministic name
	//      means check-before-create is reliable: same config always produces same Job name.
	jobName := r.deterministicJobName(configName, spec)

	existingJob := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.Config.JobNamespace}, existingJob)
	if err == nil {
		// Job already exists — mark as building and wait for it.
		log.Info("builder job already exists, setting status to building", "job", jobName)
		if err := r.setAnnotations(ctx, hc, map[string]string{
			annotationStatus: "building",
			annotationJob:    jobName,
			annotationSpec:   image,
		}); err != nil {
			log.Error(err, "failed to annotate config")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Job doesn't exist — pause machine pool and create builder Job.
	log.Info("cache miss, no existing job — creating builder job", "config", configName)

	if err := r.setMachinePoolPaused(ctx, cluster, configName, true); err != nil {
		log.Error(err, "failed to pause machine pool")
		// Continue anyway — pausing is best-effort.
	}

	// DECISION: Set annotations BEFORE creating the Job.
	// Why: If Job creation succeeds but annotation patch fails, the next reconcile
	//      would see no status annotation and try to create a duplicate Job.
	//      By annotating first, the duplicate is prevented (handleBuilding handles it).
	//      If annotation succeeds but Job creation fails, handleBuilding resets
	//      when it can't find the Job (already handled).
	if err := r.setAnnotations(ctx, hc, map[string]string{
		annotationStatus: "building",
		annotationJob:    jobName,
		annotationSpec:   image,
	}); err != nil {
		log.Error(err, "failed to annotate config before job creation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, err = r.createBuilderJob(ctx, configName, spec, token, enableCIS, jobName)
	if err != nil {
		log.Error(err, "failed to create builder job")
		// Reset annotation so next reconcile can retry.
		_ = r.setAnnotations(ctx, hc, map[string]string{annotationStatus: ""})
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleBuilding checks the status of an in-progress builder Job.
func (r *HetznerConfigReconciler) handleBuilding(ctx context.Context, hc *unstructured.Unstructured) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	configName := hc.GetName()

	annotations := hc.GetAnnotations()
	jobName := ""
	originalSpec := ""
	if annotations != nil {
		jobName = annotations[annotationJob]
		originalSpec = annotations[annotationSpec]
	}

	if jobName == "" {
		// DECISION: Try to recover job reference from deterministic name before resetting.
		// Why: If annotation was corrupted, we can still find the Job by name.
		if originalSpec != "" {
			spec := strings.TrimPrefix(originalSpec, goldenPrefix)
			recoveredName := r.deterministicJobName(configName, spec)
			existingJob := &batchv1.Job{}
			if err := r.Get(ctx, types.NamespacedName{Name: recoveredName, Namespace: r.Config.JobNamespace}, existingJob); err == nil {
				log.Info("recovered lost job reference", "config", configName, "job", recoveredName)
				_ = r.setAnnotations(ctx, hc, map[string]string{
					annotationJob: recoveredName,
				})
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}
		log.Info("lost job reference, resetting", "config", configName)
		return ctrl.Result{RequeueAfter: 10 * time.Second},
			r.setAnnotations(ctx, hc, map[string]string{annotationStatus: ""})
	}

	// Check Job status.
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.Config.JobNamespace}, job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("builder job not found, resetting", "job", jobName)
			return ctrl.Result{RequeueAfter: 10 * time.Second},
				r.setAnnotations(ctx, hc, map[string]string{annotationStatus: ""})
		}
		return ctrl.Result{}, err
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			log.Info("builder job completed", "job", jobName)
			return r.handleJobCompleted(ctx, hc, originalSpec)
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			log.Info("builder job failed", "job", jobName, "reason", c.Reason)
			// WORKAROUND: Packer may have completed the build but Hetzner snapshot
			// creation outlasted the Job deadline. Check if snapshot appeared anyway.
			if c.Reason == "DeadlineExceeded" {
				result, err := r.handleJobCompleted(ctx, hc, originalSpec)
				if err == nil {
					return result, nil
				}
				log.Info("snapshot not found after deadline, marking failed")
			}
			return ctrl.Result{},
				r.setAnnotations(ctx, hc, map[string]string{annotationStatus: "failed"})
		}
	}

	// Job still running.
	log.V(1).Info("builder job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleJobCompleted finds the newly built snapshot and resolves the image.
func (r *HetznerConfigReconciler) handleJobCompleted(ctx context.Context, hc *unstructured.Unstructured, originalSpec string) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	enableCIS := strings.TrimPrefix(originalSpec, goldenPrefix) == "cis"

	// Find parent Cluster (needed for token + unpause).
	cluster, err := r.findClusterForConfig(ctx, hc.GetName())
	if err != nil {
		log.Error(err, "cannot find parent cluster for resolution")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	token, err := r.getCloudCredentialToken(ctx, cluster)
	if err != nil {
		log.Error(err, "cannot get token for snapshot lookup")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	snapshotID, err := r.findSnapshot(ctx, token, r.Config.RKE2Version, enableCIS)
	if err != nil || snapshotID == "" {
		log.Error(err, "job completed but snapshot not found")
		return ctrl.Result{},
			r.setAnnotations(ctx, hc, map[string]string{annotationStatus: "failed"})
	}

	return r.resolveImage(ctx, hc, cluster, snapshotID, originalSpec)
}

// resolveImage patches the HetznerConfig with the real snapshot ID and unpauses the machine pool.
func (r *HetznerConfigReconciler) resolveImage(
	ctx context.Context,
	hc *unstructured.Unstructured,
	cluster *unstructured.Unstructured,
	snapshotID string,
	originalSpec string,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Single atomic patch: update image field + annotations.
	patchData := map[string]interface{}{
		"image": snapshotID,
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				annotationStatus:   "resolved",
				annotationSnapshot: snapshotID,
				annotationSpec:     originalSpec,
			},
		},
	}
	patchBytes, _ := json.Marshal(patchData)
	if err := r.Patch(ctx, hc, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		log.Error(err, "failed to patch HetznerConfig image")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Unpause machine pool so Rancher starts provisioning.
	if err := r.setMachinePoolPaused(ctx, cluster, hc.GetName(), false); err != nil {
		log.Error(err, "failed to unpause machine pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log.Info("image resolved successfully",
		"config", hc.GetName(),
		"snapshotID", snapshotID,
		"originalSpec", originalSpec,
	)
	return ctrl.Result{}, nil
}

// ── Cluster Discovery ────────────────────────────────────────────────────────

// findClusterForConfig scans Clusters in fleet-default to find one that references
// the given HetznerConfig in its machinePools[].machineConfigRef.
func (r *HetznerConfigReconciler) findClusterForConfig(ctx context.Context, configName string) (*unstructured.Unstructured, error) {
	clusterList := &unstructured.UnstructuredList{}
	clusterList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   clusterGVK.Group,
		Version: clusterGVK.Version,
		Kind:    "ClusterList",
	})

	if err := r.List(ctx, clusterList, client.InNamespace(fleetNS)); err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	for i := range clusterList.Items {
		cluster := &clusterList.Items[i]
		pools, found, _ := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
		if !found {
			continue
		}
		for _, pool := range pools {
			poolMap, ok := pool.(map[string]interface{})
			if !ok {
				continue
			}
			ref, found, _ := unstructured.NestedMap(poolMap, "machineConfigRef")
			if !found {
				continue
			}
			kind, _, _ := unstructured.NestedString(ref, "kind")
			name, _, _ := unstructured.NestedString(ref, "name")
			if kind == "HetznerConfig" && name == configName {
				return cluster, nil
			}
		}
	}

	return nil, fmt.Errorf("no cluster found referencing HetznerConfig %q", configName)
}

// ── Cloud Credentials ────────────────────────────────────────────────────────

// getCloudCredentialToken extracts the HCLOUD_TOKEN from the Cluster's Cloud Credential secret.
func (r *HetznerConfigReconciler) getCloudCredentialToken(ctx context.Context, cluster *unstructured.Unstructured) (string, error) {
	credName, found, _ := unstructured.NestedString(cluster.Object, "spec", "cloudCredentialSecretName")
	if !found || credName == "" {
		return "", fmt.Errorf("cloudCredentialSecretName not found in cluster %s", cluster.GetName())
	}

	// Rancher stores credentialSecretName as "namespace:name" (e.g. "cattle-global-data:cc-j2zfj").
	// Strip the namespace prefix to get the actual K8s Secret name.
	if parts := strings.SplitN(credName, ":", 2); len(parts) == 2 {
		credName = parts[1]
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      credName,
		Namespace: credentialNS,
	}, secret); err != nil {
		return "", fmt.Errorf("get credential secret %s/%s: %w", credentialNS, credName, err)
	}

	token, ok := secret.Data[hcloudTokenKey]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", hcloudTokenKey, credentialNS, credName)
	}

	return strings.TrimSpace(string(token)), nil
}

// ── Hetzner API ──────────────────────────────────────────────────────────────

// hetznerImageResponse represents the Hetzner Cloud API response for image listing.
type hetznerImageResponse struct {
	Images []hetznerImage `json:"images"`
}

type hetznerImage struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Created string `json:"created"`
}

// findSnapshot queries the Hetzner Cloud API for a snapshot matching the build parameters.
// Returns the snapshot ID as a string, or empty string if no matching snapshot exists.
//
// NOTE: Label keys must match rke2-base.pkr.hcl → snapshot_labels.
// The Packer template writes labels; this function reads them.
// If you add/change labels in pkr.hcl, update this query to match.
func (r *HetznerConfigReconciler) findSnapshot(ctx context.Context, token string, rke2Version string, enableCIS bool) (string, error) {
	rke2Label := strings.ReplaceAll(rke2Version, "+", "-")
	cisLabel := "false"
	if enableCIS {
		cisLabel = "true"
	}

	labelSelector := fmt.Sprintf("managed-by=packer,rke2-version=%s,cis-hardened=%s", rke2Label, cisLabel)

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.hetzner.cloud/v1/images", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	q := req.URL.Query()
	q.Set("type", "snapshot")
	q.Set("sort", "created:desc")
	q.Set("label_selector", labelSelector)
	req.URL.RawQuery = q.Encode()

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("hetzner API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("hetzner API returned %d: %s", resp.StatusCode, string(body))
	}

	var result hetznerImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode hetzner response: %w", err)
	}

	// Return the most recent available snapshot.
	for _, img := range result.Images {
		if img.Status == "available" {
			return fmt.Sprintf("%d", img.ID), nil
		}
	}

	return "", nil
}

// ── Builder Job ──────────────────────────────────────────────────────────────

// createBuilderJob creates a K8s Job that runs the hcloud-image-builder container.
// The Job builds a Hetzner Cloud snapshot using Packer + Ansible.
//
// DECISION: Job name is deterministic (passed in by caller).
// Why: Prevents duplicate Jobs for the same config+spec. Caller checks if Job
//      already exists before calling this function.
func (r *HetznerConfigReconciler) createBuilderJob(ctx context.Context, configName string, spec string, token string, enableCIS bool, jobName string) (string, error) {
	cisFlag := "false"
	if enableCIS {
		cisFlag = "true"
	}

	// DECISION: Store HCLOUD_TOKEN in a temporary Secret, not in the Job spec.
	// Why: Prevents token from being visible in `kubectl describe job` output.
	secretName := jobName + "-cred"

	// Check if secret already exists (idempotency guard).
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: r.Config.JobNamespace}, existingSecret)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("check credential secret: %w", err)
	}

	if apierrors.IsNotFound(err) {
		credSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: r.Config.JobNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by":  "hcloud-image-controller",
					"hcloud-image.cattle.io/config": configName,
				},
			},
			Data: map[string][]byte{
				"HCLOUD_TOKEN": []byte(token),
			},
		}
		if err := r.Create(ctx, credSecret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("create credential secret: %w", err)
			}
		}
	}

	backoffLimit := int32(2)
	deadline := int64(3600) // 60 minutes — CIS builds take ~20 min + snapshot creation can take 10+ min
	ttl := int32(3600)      // Clean up 1 hour after completion

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.Config.JobNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":  "hcloud-image-controller",
				"hcloud-image.cattle.io/config": configName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by":  "hcloud-image-controller",
						"hcloud-image.cattle.io/config": configName,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "builder",
							Image: r.Config.BuilderImage,
							// DECISION: Only set non-empty env vars.
							// Why: Omitted vars → Packer uses rke2-base.pkr.hcl defaults.
							// This keeps rke2-base.pkr.hcl as the fallback source of truth.
							Env: r.buildJobEnv(secretName, cisFlag),
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Job already exists — idempotent success.
			return jobName, nil
		}
		// Clean up the credential secret if Job creation fails.
		_ = r.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: r.Config.JobNamespace},
		})
		return "", fmt.Errorf("create builder job: %w", err)
	}

	// DECISION: Set OwnerReference on Secret AFTER Job creation.
	// Why: OwnerReference needs the Job UID. When Job's TTL garbage-collects it,
	//      the credential Secret is automatically deleted too — no orphans.
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: r.Config.JobNamespace}, secret); err == nil {
		trueVal := true
		secret.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               job.Name,
				UID:                job.UID,
				BlockOwnerDeletion: &trueVal,
			},
		}
		_ = r.Update(ctx, secret)
	}

	return jobName, nil
}

// ── Machine Pool Pause/Unpause ───────────────────────────────────────────────

// setMachinePoolPaused pauses or unpauses the machine pool that references the given HetznerConfig.
// This prevents Rancher from provisioning nodes before the snapshot is ready.
func (r *HetznerConfigReconciler) setMachinePoolPaused(ctx context.Context, cluster *unstructured.Unstructured, configName string, paused bool) error {
	pools, found, _ := unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	if !found {
		return nil
	}

	updated := false
	for i, pool := range pools {
		poolMap, ok := pool.(map[string]interface{})
		if !ok {
			continue
		}
		ref, found, _ := unstructured.NestedMap(poolMap, "machineConfigRef")
		if !found {
			continue
		}
		kind, _, _ := unstructured.NestedString(ref, "kind")
		name, _, _ := unstructured.NestedString(ref, "name")
		if kind == "HetznerConfig" && name == configName {
			poolMap["paused"] = paused
			pools[i] = poolMap
			updated = true
		}
	}

	if !updated {
		return nil
	}

	if err := unstructured.SetNestedSlice(cluster.Object, pools, "spec", "rkeConfig", "machinePools"); err != nil {
		return fmt.Errorf("set machinePools: %w", err)
	}

	// DECISION: Merge patch instead of full Update to avoid conflicts.
	// Why: Other controllers (e.g., Rancher) may update cluster fields concurrently.
	//      A merge patch modifies only machinePools, reducing conflict surface.
	patchData := map[string]interface{}{
		"spec": map[string]interface{}{
			"rkeConfig": map[string]interface{}{
				"machinePools": pools,
			},
		},
	}
	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return fmt.Errorf("marshal machinePools patch: %w", err)
	}
	return r.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, patchBytes))
}

// ── Job Environment ──────────────────────────────────────────────────────────

// buildJobEnv constructs env vars for the builder Job container.
// Only non-empty config values are included — omitted vars let Packer use its
// rke2-base.pkr.hcl defaults. HCLOUD_TOKEN and ENABLE_CIS are always set.
func (r *HetznerConfigReconciler) buildJobEnv(secretName string, cisFlag string) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		{
			Name: "HCLOUD_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "HCLOUD_TOKEN",
				},
			},
		},
		{Name: "ENABLE_CIS", Value: cisFlag},
	}
	if r.Config.RKE2Version != "" {
		envs = append(envs, corev1.EnvVar{Name: "RKE2_VERSION", Value: r.Config.RKE2Version})
	}
	if r.Config.Location != "" {
		envs = append(envs, corev1.EnvVar{Name: "LOCATION", Value: r.Config.Location})
	}
	if r.Config.ServerType != "" {
		envs = append(envs, corev1.EnvVar{Name: "SERVER_TYPE", Value: r.Config.ServerType})
	}
	if r.Config.BaseImage != "" {
		envs = append(envs, corev1.EnvVar{Name: "BASE_IMAGE", Value: r.Config.BaseImage})
	}
	return envs
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// setAnnotations applies a merge patch to update annotations on the resource.
func (r *HetznerConfigReconciler) setAnnotations(ctx context.Context, obj *unstructured.Unstructured, annotations map[string]string) error {
	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annotations,
		},
	}
	patchBytes, _ := json.Marshal(patchData)
	return r.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchBytes))
}

// sanitizeName makes a string safe for use in Kubernetes DNS-1123 resource names.
// Operates on runes (not bytes) to avoid splitting multi-byte characters,
// collapses consecutive hyphens, and strips leading/trailing hyphens.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "-")
	// Keep only alphanumeric ASCII and hyphens.
	var result []rune
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result = append(result, r)
		}
	}
	// Collapse consecutive hyphens and trim leading/trailing hyphens.
	out := strings.TrimRight(strings.TrimLeft(string(result), "-"), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

// deterministicJobName generates a stable, collision-free Job name from config + spec.
// DECISION: Use SHA-256 hash of (configName + spec + rke2Version) truncated to 8 hex chars.
// Why: Timestamp-based names caused collisions at second precision and made
//      check-before-create impossible (new name every time = always creates new Job).
//      A deterministic name means: same input → same Job name → idempotent creation.
//
// To rebuild after a failed attempt, operator clears status annotation AND deletes
// the old Job — next reconcile creates a new Job with the same deterministic name.
func (r *HetznerConfigReconciler) deterministicJobName(configName string, spec string) string {
	hash := sha256.Sum256([]byte(configName + "/" + spec + "/" + r.Config.RKE2Version))
	suffix := fmt.Sprintf("%x", hash[:4]) // 8 hex chars
	name := fmt.Sprintf("img-build-%s-%s", sanitizeName(configName), suffix)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// getEnv returns the value of an environment variable, or a fallback default.
func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
