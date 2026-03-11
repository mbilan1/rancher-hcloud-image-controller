package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// SetupWithManager registers the HetznerConfigReconciler with the controller manager.
func (r *HetznerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(hetznerConfigGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(obj).
		Complete(r)
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
func (r *HetznerConfigReconciler) handleNew(
	ctx context.Context, hc *unstructured.Unstructured, image string,
) (ctrl.Result, error) {
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

	rke2Version := r.resolveRKE2Version(ctx, cluster)

	// Get HCLOUD_TOKEN from Cloud Credential secret.
	token, err := r.getCloudCredentialToken(ctx, cluster)
	if err != nil {
		log.Error(err, "cannot get cloud credential token")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check for cached snapshot via Hetzner API.
	snapshotID, err := r.findSnapshot(ctx, token, rke2Version, enableCIS)
	if err != nil {
		log.Error(err, "error checking snapshot cache")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if snapshotID != "" {
		log.Info("cache hit — using existing snapshot", "snapshotID", snapshotID)
		return r.resolveImage(ctx, hc, cluster, snapshotID, image)
	}

	return r.createNewBuild(ctx, hc, cluster, configName, spec, image, rke2Version, token, enableCIS)
}

// resolveRKE2Version reads the cluster's kubernetesVersion, falling back to global config.
func (r *HetznerConfigReconciler) resolveRKE2Version(
	ctx context.Context, cluster *unstructured.Unstructured,
) string {
	log := log.FromContext(ctx)

	rke2Version := getClusterRKE2Version(cluster)
	source := "cluster"
	if rke2Version == "" {
		rke2Version = r.Config.RKE2Version
		source = "config-fallback"
	}
	log.Info("effective RKE2 version", "version", rke2Version, "source", source)
	return rke2Version
}

// createNewBuild handles the cache-miss path: pause pool, create Job, annotate.
func (r *HetznerConfigReconciler) createNewBuild(
	ctx context.Context,
	hc *unstructured.Unstructured,
	cluster *unstructured.Unstructured,
	configName, spec, image, rke2Version, token string,
	enableCIS bool,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// DECISION: Use deterministic job name derived from config name + spec + version hash.
	// Why: Timestamp-based names cause collisions at second precision and
	//      create orphaned Jobs on annotation patch failure.
	jobName := deterministicJobName(configName, spec, rke2Version)

	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: r.Config.JobNamespace}, existingJob)
	if err == nil {
		// Job already exists — mark as building and wait for it.
		log.Info("builder job already exists, setting status to building", "job", jobName)
		if err := r.setAnnotations(ctx, hc, map[string]string{
			annotationStatus:      "building",
			annotationJob:         jobName,
			annotationSpec:        image,
			annotationRKE2Version: rke2Version,
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

	// DECISION: Pause is a HARD GATE — do NOT proceed if pause fails.
	// Why: If Rancher provisions nodes with golden:cis (not a real image ID),
	//      those nodes will fail immediately. Pausing ensures Rancher waits for
	//      the snapshot to be built and the image field to be patched.
	if err := r.setMachinePoolPaused(ctx, cluster, configName, true); err != nil {
		errMsg := fmt.Sprintf("cannot pause machine pool for config %q: %v — refusing to build until pause succeeds", configName, err)
		log.Error(err, "HARD GATE: failed to pause machine pool, aborting build")
		_ = r.setAnnotations(ctx, hc, map[string]string{
			annotationError: errMsg,
		})
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// DECISION: Set annotations BEFORE creating the Job.
	// Why: If Job creation succeeds but annotation patch fails, the next reconcile
	//      would see no status annotation and try to create a duplicate Job.
	//      By annotating first, the duplicate is prevented (handleBuilding handles it).
	//      If annotation succeeds but Job creation fails, handleBuilding resets
	//      when it can't find the Job (already handled).
	if err := r.setAnnotations(ctx, hc, map[string]string{
		annotationStatus:      "building",
		annotationJob:         jobName,
		annotationSpec:        image,
		annotationRKE2Version: rke2Version,
	}); err != nil {
		log.Error(err, "failed to annotate config before job creation")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	_, err = r.createBuilderJob(ctx, configName, token, enableCIS, jobName, rke2Version)
	if err != nil {
		errMsg := fmt.Sprintf("failed to create builder job %q for config %q: %v", jobName, configName, err)
		log.Error(err, "failed to create builder job")
		_ = r.setAnnotations(ctx, hc, map[string]string{
			annotationStatus: "",
			annotationError:  errMsg,
		})
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Clear any previous error on successful Job creation.
	_ = r.setAnnotations(ctx, hc, map[string]string{annotationError: ""})

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleBuilding checks the status of an in-progress builder Job.
func (r *HetznerConfigReconciler) handleBuilding(ctx context.Context, hc *unstructured.Unstructured) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	configName := hc.GetName()

	annotations := hc.GetAnnotations()
	jobName := ""
	originalSpec := ""
	rke2Version := ""
	if annotations != nil {
		jobName = annotations[annotationJob]
		originalSpec = annotations[annotationSpec]
		rke2Version = annotations[annotationRKE2Version]
	}
	// Fallback to global config if version annotation is missing (pre-upgrade compat).
	if rke2Version == "" {
		rke2Version = r.Config.RKE2Version
	}

	if jobName == "" {
		// DECISION: Try to recover job reference from deterministic name before resetting.
		// Why: If annotation was corrupted, we can still find the Job by name.
		if originalSpec != "" {
			spec := strings.TrimPrefix(originalSpec, goldenPrefix)
			recoveredName := deterministicJobName(configName, spec, rke2Version)
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
		if c.Type == batchv1.JobComplete && c.Status == "True" {
			log.Info("builder job completed", "job", jobName)
			return r.handleJobCompleted(ctx, hc, originalSpec, rke2Version)
		}
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			log.Info("builder job failed", "job", jobName, "reason", c.Reason)
			// WORKAROUND: Packer may have completed the build but Hetzner snapshot
			// creation outlasted the Job deadline. Check if snapshot appeared anyway.
			if c.Reason == "DeadlineExceeded" {
				result, err := r.handleJobCompleted(ctx, hc, originalSpec, rke2Version)
				if err == nil {
					return result, nil
				}
				log.Info("snapshot not found after deadline, marking failed")
			}
			errMsg := fmt.Sprintf("builder job %q failed (reason: %s) for config %q — check job logs: kubectl logs -n %s job/%s",
				jobName, c.Reason, configName, r.Config.JobNamespace, jobName)
			return ctrl.Result{},
				r.setAnnotations(ctx, hc, map[string]string{
					annotationStatus: "failed",
					annotationError:  errMsg,
				})
		}
	}

	// Job still running.
	log.V(1).Info("builder job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleJobCompleted finds the newly built snapshot and resolves the image.
func (r *HetznerConfigReconciler) handleJobCompleted(
	ctx context.Context, hc *unstructured.Unstructured, originalSpec, rke2Version string,
) (ctrl.Result, error) {
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

	snapshotID, err := r.findSnapshot(ctx, token, rke2Version, enableCIS)
	if err != nil || snapshotID == "" {
		errMsg := fmt.Sprintf(
			"builder job completed but no snapshot found for config %q (rke2=%s, cis=%v)"+
				" — snapshot may still be creating on Hetzner, or builder failed silently",
			hc.GetName(), rke2Version, enableCIS)
		log.Error(err, "job completed but snapshot not found")
		return ctrl.Result{},
			r.setAnnotations(ctx, hc, map[string]string{
				annotationStatus: "failed",
				annotationError:  errMsg,
			})
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
