package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// ── Cluster Metadata ─────────────────────────────────────────────────────────

// getClusterRKE2Version reads spec.kubernetesVersion from the Cluster CRD.
// Returns the version string (e.g. "v1.34.4+rke2r1") or empty string if not set.
//
// DECISION: Per-cluster version instead of global DEFAULT_RKE2_VERSION.
// Why: Different downstream clusters may use different K8s versions. Using the
//
//	cluster's own kubernetesVersion ensures the snapshot matches the RKE2 version
//	that will actually run on the nodes. Falls back to Config.RKE2Version if empty.
func getClusterRKE2Version(cluster *unstructured.Unstructured) string {
	version, found, _ := unstructured.NestedString(cluster.Object, "spec", "kubernetesVersion")
	if !found {
		return ""
	}
	return version
}

// ── Machine Pool Pause/Unpause ───────────────────────────────────────────────

// setMachinePoolPaused pauses or unpauses the machine pool that references the given HetznerConfig.
// This prevents Rancher from provisioning nodes before the snapshot is ready.
func (r *HetznerConfigReconciler) setMachinePoolPaused(
	ctx context.Context, cluster *unstructured.Unstructured, configName string, paused bool,
) error {
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
