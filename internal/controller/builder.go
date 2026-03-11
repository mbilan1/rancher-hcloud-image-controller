package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ── Builder Job ──────────────────────────────────────────────────────────────

// createBuilderJob creates a K8s Job that runs the hcloud-image-builder container.
// The Job builds a Hetzner Cloud snapshot using Packer + Ansible.
//
// DECISION: Job name is deterministic (passed in by caller).
// Why: Prevents duplicate Jobs for the same config+spec. Caller checks if Job
//
//	already exists before calling this function.
func (r *HetznerConfigReconciler) createBuilderJob(
	ctx context.Context, configName, token string, enableCIS bool, jobName, rke2Version string,
) (string, error) {
	cisFlag := "false"
	if enableCIS {
		cisFlag = "true"
	}

	secretName := jobName + "-cred"
	if err := r.ensureCredentialSecret(ctx, secretName, configName, token); err != nil {
		return "", err
	}

	// DECISION: backoffLimit=2 (diverges from DES-004 which recommends 0).
	// Why: Packer builds can fail due to transient Hetzner API errors (rate limits,
	//      temporary server creation failures, snapshot API timeouts). With backoffLimit=0,
	//      every transient failure requires manual intervention (clear annotation + delete Job).
	//      backoffLimit=2 gives 3 total attempts (1 initial + 2 retries), which handles
	//      common transient failures while still failing fast on persistent issues.
	//      Trade-off: slightly higher resource usage on genuine failures vs dramatically
	//      reduced operator toil for transient issues.
	backoffLimit := int32(2)
	deadline := int64(3600) // 60 minutes — CIS builds take ~20 min + snapshot creation can take 10+ min.
	ttl := int32(3600)      // Clean up 1 hour after completion.

	job := r.buildJobObject(configName, jobName, secretName, cisFlag, rke2Version, backoffLimit, deadline, ttl)

	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return jobName, nil
		}
		_ = r.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: r.Config.JobNamespace},
		})
		return "", fmt.Errorf("create builder job: %w", err)
	}

	r.setSecretOwnerRef(ctx, secretName, job)

	return jobName, nil
}

// ensureCredentialSecret creates the Secret holding HCLOUD_TOKEN if it doesn't already exist.
func (r *HetznerConfigReconciler) ensureCredentialSecret(ctx context.Context, secretName, configName, token string) error {
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: r.Config.JobNamespace}, existingSecret)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("check credential secret: %w", err)
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
				return fmt.Errorf("create credential secret: %w", err)
			}
		}
	}
	return nil
}

// buildJobObject constructs the batchv1.Job object for the builder.
func (r *HetznerConfigReconciler) buildJobObject(
	configName, jobName, secretName, cisFlag, rke2Version string,
	backoffLimit int32, deadline int64, ttl int32,
) *batchv1.Job {
	return &batchv1.Job{
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
							Env:   r.buildJobEnv(secretName, cisFlag, rke2Version),
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
}

// setSecretOwnerRef sets OwnerReference on the credential Secret so it's
// garbage-collected when the Job's TTL expires.
func (r *HetznerConfigReconciler) setSecretOwnerRef(ctx context.Context, secretName string, job *batchv1.Job) {
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
}

// ── Job Environment ──────────────────────────────────────────────────────────

// buildJobEnv constructs env vars for the builder Job container.
// Only non-empty config values are included — omitted vars let Packer use its
// rke2-base.pkr.hcl defaults. HCLOUD_TOKEN and ENABLE_CIS are always set.
// rke2Version is the effective version (from cluster or fallback config).
func (r *HetznerConfigReconciler) buildJobEnv(secretName, cisFlag, rke2Version string) []corev1.EnvVar {
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
	if rke2Version != "" {
		envs = append(envs, corev1.EnvVar{Name: "RKE2_VERSION", Value: rke2Version})
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
