package controller

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ── Test data builders ───────────────────────────────────────────────────────

func newHetznerConfig(name, image string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rke-machine-config.cattle.io/v1",
			"kind":       "HetznerConfig",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": fleetNS,
			},
			"image": image,
		},
	}
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	return obj
}

func newCluster(name, credentialSecret string, pools []interface{}) *unstructured.Unstructured {
	return newClusterWithVersion(name, credentialSecret, pools, "")
}

func newClusterWithVersion(name, credentialSecret string, pools []interface{}, kubernetesVersion string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"cloudCredentialSecretName": credentialSecret,
		"rkeConfig": map[string]interface{}{
			"machinePools": pools,
		},
	}
	if kubernetesVersion != "" {
		spec["kubernetesVersion"] = kubernetesVersion
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "provisioning.cattle.io/v1",
			"kind":       "Cluster",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": fleetNS,
			},
			"spec": spec,
		},
	}
}

func newMachinePool(name, configKind, configName string) map[string]interface{} {
	return map[string]interface{}{
		"name": name,
		"machineConfigRef": map[string]interface{}{
			"kind": configKind,
			"name": configName,
		},
	}
}

func newCredentialSecret(name, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: credentialNS,
		},
		Data: map[string][]byte{
			hcloudTokenKey: []byte(token),
		},
	}
}

func newReconciler(objects ...runtime.Object) *HetznerConfigReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithRuntimeObjects(obj)
	}

	return &HetznerConfigReconciler{
		Client:     builder.Build(),
		Config:     Config{RKE2Version: "v1.32.4+rke2r1", BuilderImage: "builder:test", JobNamespace: "hcloud-image-system"},
		HTTPClient: &http.Client{},
	}
}

// ── findClusterForConfig ─────────────────────────────────────────────────────

var _ = Describe("findClusterForConfig", func() {
	It("finds a cluster that references the config", func() {
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		r := newReconciler()
		// For unstructured, we need to pre-populate with unstructured objects
		// Use the reconciler's client directly since fake client doesn't support unstructured well out of box
		// Instead, test by creating a dedicated fake client for unstructured
		r.Client = newUnstructuredFakeClient(cluster)

		found, err := r.findClusterForConfig(ctx(), "my-config")
		Expect(err).NotTo(HaveOccurred())
		Expect(found.GetName()).To(Equal("my-cluster"))
	})

	It("returns error when no cluster references the config", func() {
		cluster := newCluster("my-cluster", "cred", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "other-config"),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster)

		_, err := r.findClusterForConfig(ctx(), "missing-config")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no cluster found"))
	})

	It("searches across multiple pools and clusters", func() {
		cluster1 := newCluster("cluster-a", "cred-a", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "config-a"),
		})
		cluster2 := newCluster("cluster-b", "cred-b", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "config-x"),
			newMachinePool("pool-1", "HetznerConfig", "config-b"),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster1, cluster2)

		found, err := r.findClusterForConfig(ctx(), "config-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(found.GetName()).To(Equal("cluster-b"))
	})
})

// ── getCloudCredentialToken ──────────────────────────────────────────────────

var _ = Describe("getCloudCredentialToken", func() {
	It("extracts token from secret referenced by cluster", func() {
		secret := newCredentialSecret("cc-test", "my-hcloud-token")
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", nil)
		r := newReconciler(secret)

		token, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(token).To(Equal("my-hcloud-token"))
	})

	It("handles credential name without namespace prefix", func() {
		secret := newCredentialSecret("cc-plain", "token-value")
		cluster := newCluster("my-cluster", "cc-plain", nil)
		r := newReconciler(secret)

		token, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(token).To(Equal("token-value"))
	})

	It("trims whitespace from token", func() {
		secret := newCredentialSecret("cc-ws", "  token-with-spaces  \n")
		cluster := newCluster("my-cluster", "cattle-global-data:cc-ws", nil)
		r := newReconciler(secret)

		token, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(token).To(Equal("token-with-spaces"))
	})

	It("returns error when cloudCredentialSecretName is missing", func() {
		cluster := newCluster("my-cluster", "", nil)
		r := newReconciler()

		_, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cloudCredentialSecretName"))
	})

	It("returns error when secret does not exist", func() {
		cluster := newCluster("my-cluster", "cattle-global-data:cc-missing", nil)
		r := newReconciler()

		_, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).To(HaveOccurred())
	})

	It("returns error when token key is missing from secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cc-nokey", Namespace: credentialNS},
			Data:       map[string][]byte{"other-key": []byte("value")},
		}
		cluster := newCluster("my-cluster", "cattle-global-data:cc-nokey", nil)
		r := newReconciler(secret)

		_, err := r.getCloudCredentialToken(ctx(), cluster)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(hcloudTokenKey))
	})
})

// ── getClusterRKE2Version ────────────────────────────────────────────────────

var _ = Describe("getClusterRKE2Version", func() {
	It("returns kubernetesVersion from cluster spec", func() {
		cluster := newClusterWithVersion("my-cluster", "cred", nil, "v1.34.4+rke2r1")
		Expect(getClusterRKE2Version(cluster)).To(Equal("v1.34.4+rke2r1"))
	})

	It("returns empty string when kubernetesVersion is not set", func() {
		cluster := newCluster("my-cluster", "cred", nil)
		Expect(getClusterRKE2Version(cluster)).To(BeEmpty())
	})
})

// ── createBuilderJob ─────────────────────────────────────────────────────────

var _ = Describe("createBuilderJob", func() {
	It("creates a job and credential secret", func() {
		r := newReconciler()

		jobName, err := r.createBuilderJob(ctx(), "my-config", "hcloud-token-123", true, "img-build-test-abcd1234", "v1.32.4+rke2r1")
		Expect(err).NotTo(HaveOccurred())
		Expect(jobName).To(Equal("img-build-test-abcd1234"))

		// Verify Job was created.
		job := &batchv1.Job{}
		err = r.Get(ctx(), types.NamespacedName{Name: "img-build-test-abcd1234", Namespace: "hcloud-image-system"}, job)
		Expect(err).NotTo(HaveOccurred())
		Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("builder:test"))
		Expect(job.Labels["hcloud-image.cattle.io/config"]).To(Equal("my-config"))

		// Verify credential Secret was created.
		secret := &corev1.Secret{}
		err = r.Get(ctx(), types.NamespacedName{Name: "img-build-test-abcd1234-cred", Namespace: "hcloud-image-system"}, secret)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(secret.Data["HCLOUD_TOKEN"])).To(Equal("hcloud-token-123"))
	})

	It("is idempotent — returns success if job already exists", func() {
		existingJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "img-build-existing", Namespace: "hcloud-image-system"},
			Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "builder", Image: "old"}}}}},
		}
		r := newReconciler(existingJob)

		jobName, err := r.createBuilderJob(ctx(), "my-config", "token", true, "img-build-existing", "v1.32.4+rke2r1")
		Expect(err).NotTo(HaveOccurred())
		Expect(jobName).To(Equal("img-build-existing"))
	})

	It("sets CIS flag in env vars", func() {
		r := newReconciler()

		_, err := r.createBuilderJob(ctx(), "cfg", "tok", true, "img-build-cis-test", "v1.32.4+rke2r1")
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		err = r.Get(ctx(), types.NamespacedName{Name: "img-build-cis-test", Namespace: "hcloud-image-system"}, job)
		Expect(err).NotTo(HaveOccurred())

		cisVal := envVarValue(job.Spec.Template.Spec.Containers[0].Env, "ENABLE_CIS")
		Expect(cisVal).To(Equal("true"))
	})

	It("sets resource limits on the container", func() {
		r := newReconciler()

		_, err := r.createBuilderJob(ctx(), "cfg", "tok", false, "img-build-res-test", "v1.32.4+rke2r1")
		Expect(err).NotTo(HaveOccurred())

		job := &batchv1.Job{}
		err = r.Get(ctx(), types.NamespacedName{Name: "img-build-res-test", Namespace: "hcloud-image-system"}, job)
		Expect(err).NotTo(HaveOccurred())

		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Requests).NotTo(BeEmpty())
		Expect(container.Resources.Limits).NotTo(BeEmpty())
	})
})

// ── setAnnotations ───────────────────────────────────────────────────────────

var _ = Describe("setAnnotations", func() {
	It("sets annotations on a HetznerConfig", func() {
		hc := newHetznerConfig("test-config", "golden:cis", nil)
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(hc)

		err := r.setAnnotations(ctx(), hc, map[string]string{
			annotationStatus: "building",
			annotationJob:    "img-build-test",
		})
		Expect(err).NotTo(HaveOccurred())

		// Re-fetch to verify.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "test-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, "building"))
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationJob, "img-build-test"))
	})
})

// ── setMachinePoolPaused ─────────────────────────────────────────────────────

var _ = Describe("setMachinePoolPaused", func() {
	It("pauses the matching machine pool", func() {
		cluster := newCluster("my-cluster", "cred", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster)

		err := r.setMachinePoolPaused(ctx(), cluster, "my-config", true)
		Expect(err).NotTo(HaveOccurred())

		// Re-fetch and check.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(clusterGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-cluster", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())

		pools, _, _ := unstructured.NestedSlice(updated.Object, "spec", "rkeConfig", "machinePools")
		Expect(pools).To(HaveLen(1))
		poolMap := pools[0].(map[string]interface{})
		Expect(poolMap["paused"]).To(BeTrue())
	})

	It("unpauses the matching machine pool", func() {
		cluster := newCluster("my-cluster", "cred", []interface{}{
			func() map[string]interface{} {
				p := newMachinePool("pool-0", "HetznerConfig", "my-config")
				p["paused"] = true
				return p
			}(),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster)

		err := r.setMachinePoolPaused(ctx(), cluster, "my-config", false)
		Expect(err).NotTo(HaveOccurred())

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(clusterGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-cluster", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())

		pools, _, _ := unstructured.NestedSlice(updated.Object, "spec", "rkeConfig", "machinePools")
		poolMap := pools[0].(map[string]interface{})
		Expect(poolMap["paused"]).To(BeFalse())
	})

	It("does nothing when no pool matches", func() {
		cluster := newCluster("my-cluster", "cred", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "other-config"),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster)

		err := r.setMachinePoolPaused(ctx(), cluster, "no-match", true)
		Expect(err).NotTo(HaveOccurred())
	})

	It("does nothing when cluster has no machinePools", func() {
		cluster := newCluster("my-cluster", "cred", nil)
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(cluster)

		err := r.setMachinePoolPaused(ctx(), cluster, "my-config", true)
		Expect(err).NotTo(HaveOccurred())
	})
})

// ── resolveImage ─────────────────────────────────────────────────────────────

var _ = Describe("resolveImage", func() {
	It("patches image field and sets resolved annotations", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
		})
		cluster := newCluster("my-cluster", "cred", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		r := newReconciler()
		r.Client = newUnstructuredFakeClient(hc, cluster)

		result, err := r.resolveImage(ctx(), hc, cluster, "99999", "golden:cis")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		// Re-fetch and verify.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())

		imageField, _, _ := unstructured.NestedString(updated.Object, "image")
		Expect(imageField).To(Equal("99999"))
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, "resolved"))
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationSnapshot, "99999"))
	})
})
