# Hcloud Image Controller

[![Quality Gate](https://github.com/mbilan1/rancher-hcloud-image-controller/actions/workflows/quality-gate.yml/badge.svg)](https://github.com/mbilan1/rancher-hcloud-image-controller/actions/workflows/quality-gate.yml)
[![Tests](https://github.com/mbilan1/rancher-hcloud-image-controller/actions/workflows/unit-test.yml/badge.svg)](https://github.com/mbilan1/rancher-hcloud-image-controller/actions/workflows/unit-test.yml)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Go Report Card](https://goreportcard.com/badge/github.com/mbilan1/rancher-hcloud-image-controller)](https://goreportcard.com/report/github.com/mbilan1/rancher-hcloud-image-controller)
[![Go Reference](https://pkg.go.dev/badge/github.com/mbilan1/rancher-hcloud-image-controller.svg)](https://pkg.go.dev/github.com/mbilan1/rancher-hcloud-image-controller)
[![Rancher](https://img.shields.io/badge/Rancher-%E2%89%A5v2.10-0075A8?logo=rancher&logoColor=white)](https://ranchermanager.docs.rancher.com/)
[![Hetzner Driver](https://img.shields.io/badge/Hetzner_Driver-v0.9.0-D50C2D)](https://github.com/zsys-studio/rancher-hetzner-cluster-provider)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Problem

Rancher provisions downstream cluster nodes using Hetzner Cloud snapshots. These snapshots must contain a pre-installed RKE2 distribution matching the cluster's Kubernetes version.

Without this controller, operators must:
1. Manually build Packer snapshots for each RKE2 version
2. Look up snapshot IDs in the Hetzner Cloud console
3. Edit `HetznerConfig` CRDs to hardcode snapshot IDs
4. Repeat for every version upgrade across every cluster

This controller eliminates all manual steps. Operators write `image: "golden:cis"` once and the controller handles the rest — building, caching, and resolving snapshots automatically.

## How It Works

```
HetznerConfig (image: "golden:cis")
  → Controller detects golden: prefix
  → Pauses machine pool (hard gate — Rancher cannot provision)
  → Reads cluster's spec.kubernetesVersion → determines RKE2 version
  → Queries Hetzner API for cached snapshot (label match: version + CIS profile)
  → Cache hit  → patches HetznerConfig with snapshot ID, unpauses
  → Cache miss → creates K8s Job (Packer builder)
                → Job builds snapshot on Hetzner Cloud
                → Controller detects Job completion → resolves snapshot ID
                → Patches HetznerConfig, unpauses machine pool
```

All failure paths set `hcloud-image.cattle.io/error` annotation with a self-explaining message.

## Features

- **Automatic snapshot builds** — Packer Jobs are created as K8s Jobs inside the cluster
- **Snapshot caching** — reuses existing snapshots when RKE2 version + CIS profile match
- **Per-cluster RKE2 version** — reads `spec.kubernetesVersion` from the Cluster CRD (fallback to global config)
- **Hard pause gate** — machine pool is paused before build starts; Rancher cannot provision with unresolved `golden:*`
- **Self-explaining errors** — every failure writes a human-readable annotation visible via `kubectl get hetznerconfig -o yaml`
- **Deterministic Job names** — hash-based naming prevents duplicate builder Jobs
- **Leader election** — safe to run multiple replicas

## Prerequisites

- Rancher management cluster on Hetzner Cloud
- [zsys-studio Hetzner Node Driver](https://github.com/zsys-studio/rancher-hetzner-cluster-provider) (v0.9.0+) installed
- Cloud Credentials configured in Rancher (provides `HCLOUD_TOKEN` to builder Jobs)
- Builder image (`ghcr.io/mbilan1/hcloud-image-builder`) available

## Installation

Deploy via the Helm chart:

```bash
helm install hcloud-image-controller chart/hcloud-image-controller \
  --namespace hcloud-image-system \
  --create-namespace
```

## Configuration

All values in [`chart/hcloud-image-controller/values.yaml`](chart/hcloud-image-controller/values.yaml).

| Parameter | Default | Description |
|---|---|---|
| `controller.image.tag` | `0.1.0` | Controller image version |
| `builder.image.tag` | `0.1.0` | Builder (Packer) image version |
| `defaults.rke2Version` | `v1.34.4+rke2r1` | Fallback RKE2 version (cluster version takes priority) |
| `defaults.location` | `hel1` | Hetzner datacenter for builder server |
| `defaults.serverType` | `cx23` | Server type for builder server |

## Usage

Set `image: "golden:cis"` (or `golden:base`) in your `HetznerConfig`:

```yaml
apiVersion: rke-machine-config.cattle.io/v1
kind: HetznerConfig
metadata:
  name: pool-worker
  namespace: fleet-default
serverType: cx33
serverLocation: fsn1
image: "golden:cis"          # ← controller resolves this to a snapshot ID
usePrivateNetwork: true
networks:
  - "my-network"
```

The controller will:
1. Pause the machine pool
2. Check for a cached snapshot matching the cluster's RKE2 version + CIS profile
3. Build one if needed (Packer Job)
4. Patch `image` to the snapshot ID (e.g., `12345678`)
5. Unpause the machine pool — Rancher provisions nodes normally

## Annotations

The controller sets these annotations on `HetznerConfig` resources:

| Annotation | Values | Description |
|---|---|---|
| `hcloud-image.cattle.io/status` | `building` / `resolved` / `failed` | Current state |
| `hcloud-image.cattle.io/job-name` | Job name | Builder Job reference |
| `hcloud-image.cattle.io/snapshot-id` | Numeric ID | Resolved Hetzner snapshot |
| `hcloud-image.cattle.io/rke2-version` | e.g. `v1.34.4+rke2r1` | Effective RKE2 version used |
| `hcloud-image.cattle.io/error` | Human-readable message | Error details (on failure) |
| `hcloud-image.cattle.io/original-spec` | e.g. `golden:cis` | Original image value before resolution |

## Project Structure

```
cmd/controller/main.go                 Entry point (config, manager, leader election)
internal/controller/
  types.go                             Constants, GVKs, Config struct
  reconciler.go                        Reconcile loop — 6-phase state machine
  cluster.go                           Cluster discovery, cloud credentials, pause/unpause
  builder.go                           K8s Job + credential Secret creation
  hetzner.go                           Hetzner Cloud API (snapshot lookup)
  helpers.go                           Annotations, name sanitization, deterministic naming
  *_test.go                            68 Ginkgo/Gomega BDD specs
chart/hcloud-image-controller/         Helm chart (Deployment, RBAC, ServiceAccount)
Dockerfile                             Multi-stage build (golang:1.24 → distroless)
```

## Development

```bash
# Build
make build

# Run tests (68 specs, Ginkgo/Gomega)
make test

# Run full quality gate
make quality-gate

# Build Docker image
make docker-build
```

## Quality Gate

`make quality-gate` runs all checks:

| Layer | Tool | Check |
|-------|------|-------|
| Go | `go mod tidy` | Dependency hygiene |
| Go | `gofmt` | Formatting |
| Go | `go vet` | Correctness |
| Go | `go test` | 68 unit tests (Ginkgo/Gomega) |
| Go | `golangci-lint` | ~30 linters, paranoid mode |
| Docker | `hadolint` | Dockerfile best practices |
| Docker | `trivy` | HIGH+CRITICAL misconfigurations |
| Helm | `helm lint --strict` | Chart validity |
| Helm | `kubeconform -strict` | K8s schema validation |
| Helm | `kube-linter` | Security + best practices |
| Helm | `pluto` | Deprecated API detection |

## Related Repositories

| Repo | Purpose |
|---|---|
| [packer-hcloud-rke2](https://github.com/mbilan1/packer-hcloud-rke2) | Packer template + builder Docker image |
| [terraform-hcloud-rancher](https://github.com/mbilan1/terraform-hcloud-rancher) | Management cluster (deploys this controller) |
| [rancher-hetzner-cluster-templates](https://github.com/mbilan1/rancher-hetzner-cluster-templates) | Downstream cluster templates (uses `golden:*` convention) |

## License

MIT
