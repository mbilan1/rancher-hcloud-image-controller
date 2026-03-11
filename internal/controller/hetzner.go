package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// findSnapshot queries the Hetzner Cloud API for a snapshot matching the build parameters.
// Returns the snapshot ID as a string, or empty string if no matching snapshot exists.
//
// NOTE: Label keys must match rke2-base.pkr.hcl → snapshot_labels.
// The Packer template writes labels; this function reads them.
// If you add/change labels in pkr.hcl, update this query to match.
func (r *HetznerConfigReconciler) findSnapshot(ctx context.Context, token, rke2Version string, enableCIS bool) (string, error) {
	rke2Label := strings.ReplaceAll(rke2Version, "+", "-")
	cisLabel := "false"
	if enableCIS {
		cisLabel = "true"
	}

	labelSelector := fmt.Sprintf("managed-by=packer,rke2-version=%s,cis-hardened=%s", rke2Label, cisLabel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/images", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create hetzner API request: %w", err)
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
