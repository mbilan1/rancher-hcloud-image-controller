package controller

import (
	"context"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batchv1 "k8s.io/api/batch/v1"
)

// envVar is a simplified type for test assertions on HCLOUD_TOKEN ValueFrom.
type envVar struct {
	Name      string
	ValueFrom *corev1.EnvVarSource
}

// envVarNames extracts env var names from a slice for ContainElement matchers.
func envVarNames(envs []corev1.EnvVar) []string {
	names := make([]string, len(envs))
	for i, e := range envs {
		names[i] = e.Name
	}
	return names
}

// envVarValue returns the plain .Value of the named env var, or empty string.
func envVarValue(envs []corev1.EnvVar, name string) string {
	for _, e := range envs {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// ctx returns a background context for tests.
func ctx() context.Context {
	return context.Background()
}

// rewriteTransport redirects all HTTP requests to a test server URL while preserving
// the original request path and query. Used to intercept findSnapshot's hardcoded
// Hetzner API URL and route it to httptest.Server.
type rewriteTransport struct {
	base    http.RoundTripper
	baseURL string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, _ := url.Parse(t.baseURL)
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	return t.base.RoundTrip(req)
}

// newUnstructuredFakeClient creates a fake client that handles unstructured objects.
// Registers Rancher CRD GVKs so the fake client can List/Get them.
func newUnstructuredFakeClient(objects ...*unstructured.Unstructured) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	// Register the Rancher CRD GVKs as unstructured.
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rke-machine-config.cattle.io", Version: "v1", Kind: "HetznerConfig"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rke-machine-config.cattle.io", Version: "v1", Kind: "HetznerConfigList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "provisioning.cattle.io", Version: "v1", Kind: "Cluster"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "provisioning.cattle.io", Version: "v1", Kind: "ClusterList"},
		&unstructured.UnstructuredList{},
	)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithObjects(obj.DeepCopy())
	}
	return builder.Build()
}
