package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// compositeClient merges an unstructured client (for CRDs) with a typed client (for core resources).
// The fake client builder for unstructured and typed objects needs to share a scheme.
func newCompositeReconciler(unstructuredObjects []*unstructured.Unstructured, typedObjects []runtime.Object, httpClient *http.Client) *HetznerConfigReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	// Register CRD GVKs.
	scheme.AddKnownTypeWithName(hetznerConfigGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		hetznerConfigGVK.GroupVersion().WithKind("HetznerConfigList"),
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		clusterGVK.GroupVersion().WithKind("ClusterList"),
		&unstructured.UnstructuredList{},
	)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range unstructuredObjects {
		builder = builder.WithObjects(obj.DeepCopy())
	}
	for _, obj := range typedObjects {
		builder = builder.WithRuntimeObjects(obj)
	}

	return &HetznerConfigReconciler{
		Client: builder.Build(),
		Config: Config{
			RKE2Version:  "v1.32.4+rke2r1",
			BuilderImage: "builder:test",
			JobNamespace: "hcloud-image-system",
		},
		HTTPClient: httpClient,
	}
}

var _ = Describe("Reconcile", func() {
	It("ignores HetznerConfigs outside fleet-default namespace", func() {
		hc := newHetznerConfig("test", "golden:cis", nil)
		hc.SetNamespace("other-ns")
		r := newCompositeReconciler([]*unstructured.Unstructured{hc}, nil, &http.Client{})

		result, err := r.Reconcile(ctx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test", Namespace: "other-ns"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("ignores HetznerConfigs without golden: prefix", func() {
		hc := newHetznerConfig("test", "ubuntu-24.04", nil)
		r := newCompositeReconciler([]*unstructured.Unstructured{hc}, nil, &http.Client{})

		result, err := r.Reconcile(ctx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test", Namespace: fleetNS},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("ignores resolved HetznerConfigs (non-golden image + resolved status)", func() {
		hc := newHetznerConfig("test", "12345", map[string]string{
			annotationStatus: "resolved",
		})
		r := newCompositeReconciler([]*unstructured.Unstructured{hc}, nil, &http.Client{})

		result, err := r.Reconcile(ctx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test", Namespace: fleetNS},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("ignores failed HetznerConfigs", func() {
		hc := newHetznerConfig("test", "golden:cis", map[string]string{
			annotationStatus: "failed",
		})
		r := newCompositeReconciler([]*unstructured.Unstructured{hc}, nil, &http.Client{})

		result, err := r.Reconcile(ctx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test", Namespace: fleetNS},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("returns not-found gracefully for missing resources", func() {
		r := newCompositeReconciler(nil, nil, &http.Client{})

		result, err := r.Reconcile(ctx(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: fleetNS},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
	})
})

var _ = Describe("handleNew", func() {
	It("resolves immediately on cache hit", func() {
		hc := newHetznerConfig("my-config", "golden:cis", nil)
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		secret := newCredentialSecret("cc-test", "test-hcloud-token")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{
				Images: []hetznerImage{{ID: 99999, Status: "available"}},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		httpClient := &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc, cluster},
			[]runtime.Object{secret},
			httpClient,
		)

		result, err := r.handleNew(ctx(), hc, "golden:cis")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		// Verify image was patched.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		imageField, _, _ := unstructured.NestedString(updated.Object, "image")
		Expect(imageField).To(Equal("99999"))
	})

	It("creates builder job on cache miss", func() {
		hc := newHetznerConfig("my-config", "golden:base", nil)
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		secret := newCredentialSecret("cc-test", "test-hcloud-token")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		httpClient := &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc, cluster},
			[]runtime.Object{secret},
			httpClient,
		)

		result, err := r.handleNew(ctx(), hc, "golden:base")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		// Verify status annotation was set to building.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, "building"))

		// Verify job was created.
		jobName := updated.GetAnnotations()[annotationJob]
		Expect(jobName).NotTo(BeEmpty())
		job := &batchv1.Job{}
		err = r.Get(ctx(), types.NamespacedName{Name: jobName, Namespace: "hcloud-image-system"}, job)
		Expect(err).NotTo(HaveOccurred())
	})

	It("requeues when cluster is not found", func() {
		hc := newHetznerConfig("orphan-config", "golden:cis", nil)
		// No cluster references this config.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(hetznerImageResponse{})
		}))
		defer server.Close()

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc},
			nil,
			&http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL}},
		)

		result, err := r.handleNew(ctx(), hc, "golden:cis")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
	})

	It("uses cluster kubernetesVersion instead of config fallback", func() {
		hc := newHetznerConfig("my-config", "golden:base", nil)
		cluster := newClusterWithVersion("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		}, "v1.34.4+rke2r1")
		secret := newCredentialSecret("cc-test", "test-hcloud-token")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		httpClient := &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc, cluster},
			[]runtime.Object{secret},
			httpClient,
		)

		result, err := r.handleNew(ctx(), hc, "golden:base")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		// Verify rke2-version annotation was set to cluster's version (not config fallback).
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationRKE2Version, "v1.34.4+rke2r1"))
	})

	It("falls back to config RKE2 version when cluster has none", func() {
		hc := newHetznerConfig("my-config", "golden:cis", nil)
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		secret := newCredentialSecret("cc-test", "test-hcloud-token")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{Images: []hetznerImage{}}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		httpClient := &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: server.URL},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc, cluster},
			[]runtime.Object{secret},
			httpClient,
		)

		result, err := r.handleNew(ctx(), hc, "golden:cis")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		// Falls back to the reconciler's Config.RKE2Version.
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationRKE2Version, "v1.32.4+rke2r1"))
	})
})

var _ = Describe("handleBuilding", func() {
	It("detects completed job and resolves image", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
			annotationJob:    "img-build-test",
			annotationSpec:   "golden:cis",
		})
		cluster := newCluster("my-cluster", "cattle-global-data:cc-test", []interface{}{
			newMachinePool("pool-0", "HetznerConfig", "my-config"),
		})
		secret := newCredentialSecret("cc-test", "test-hcloud-token")

		completedJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "img-build-test", Namespace: "hcloud-image-system"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "b", Image: "x"}}},
				},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}

		apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			resp := hetznerImageResponse{
				Images: []hetznerImage{{ID: 77777, Status: "available"}},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer apiServer.Close()

		httpClient := &http.Client{
			Transport: &rewriteTransport{base: http.DefaultTransport, baseURL: apiServer.URL},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc, cluster},
			[]runtime.Object{secret, completedJob},
			httpClient,
		)

		result, err := r.handleBuilding(ctx(), hc)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		// Verify image was resolved.
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		imageField, _, _ := unstructured.NestedString(updated.Object, "image")
		Expect(imageField).To(Equal("77777"))
	})

	It("marks failed when job fails", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
			annotationJob:    "img-build-fail",
			annotationSpec:   "golden:cis",
		})

		failedJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "img-build-fail", Namespace: "hcloud-image-system"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "b", Image: "x"}}},
				},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				},
			},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc},
			[]runtime.Object{failedJob},
			&http.Client{},
		)

		_, err := r.handleBuilding(ctx(), hc)
		Expect(err).NotTo(HaveOccurred())

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, "failed"))
		// Phase 3: Verify error annotation contains actionable message.
		Expect(updated.GetAnnotations()).To(HaveKey(annotationError))
		Expect(updated.GetAnnotations()[annotationError]).To(ContainSubstring("img-build-fail"))
		Expect(updated.GetAnnotations()[annotationError]).To(ContainSubstring("BackoffLimitExceeded"))
	})

	It("requeues when job is still running", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
			annotationJob:    "img-build-running",
			annotationSpec:   "golden:cis",
		})

		runningJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "img-build-running", Namespace: "hcloud-image-system"},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "b", Image: "x"}}},
				},
			},
			Status: batchv1.JobStatus{
				Active: 1,
			},
		}

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc},
			[]runtime.Object{runningJob},
			&http.Client{},
		)

		result, err := r.handleBuilding(ctx(), hc)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
	})

	It("resets when job reference is lost and no original spec", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
			// No annotationJob, no annotationSpec
		})

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc},
			nil,
			&http.Client{},
		)

		result, err := r.handleBuilding(ctx(), hc)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, ""))
	})

	It("resets when job is not found in cluster", func() {
		hc := newHetznerConfig("my-config", "golden:cis", map[string]string{
			annotationStatus: "building",
			annotationJob:    "img-build-deleted",
			annotationSpec:   "golden:cis",
		})

		r := newCompositeReconciler(
			[]*unstructured.Unstructured{hc},
			nil,
			&http.Client{},
		)

		result, err := r.handleBuilding(ctx(), hc)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(hetznerConfigGVK)
		err = r.Get(ctx(), types.NamespacedName{Name: "my-config", Namespace: fleetNS}, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(updated.GetAnnotations()).To(HaveKeyWithValue(annotationStatus, ""))
	})
})
