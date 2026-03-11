package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// deterministicJobName generates a stable, collision-free Job name from config + spec + version.
// DECISION: Use SHA-256 hash of (configName + spec + rke2Version) truncated to 8 hex chars.
// Why: Timestamp-based names caused collisions at second precision and made
//
//	check-before-create impossible (new name every time = always creates new Job).
//	A deterministic name means: same input → same Job name → idempotent creation.
//
// To rebuild after a failed attempt, operator clears status annotation AND deletes
// the old Job — next reconcile creates a new Job with the same deterministic name.
func deterministicJobName(configName, spec, rke2Version string) string {
	hash := sha256.Sum256([]byte(configName + "/" + spec + "/" + rke2Version))
	suffix := fmt.Sprintf("%x", hash[:4]) // 8 hex chars
	name := fmt.Sprintf("img-build-%s-%s", sanitizeName(configName), suffix)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}
