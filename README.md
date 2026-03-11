# Hcloud Image Controller

Kubernetes controller that automatically builds Hetzner Cloud hcloud images
(Packer snapshots) when `HetznerConfig` CRDs use the `golden:*` image convention.

Part of the [RKE2-on-Hetzner platform](https://github.com/mbilan1/rke2-hetzner-architecture) — see [ADR-012](https://github.com/mbilan1/rke2-hetzner-architecture/blob/main/decisions/adr-012-golden-image-controller.md) and [DES-004](https://github.com/mbilan1/rke2-hetzner-architecture/blob/main/designs/des-004-golden-image-controller.md).

## How It Works

1. User creates a downstream cluster template with `image: "golden:cis"` in `HetznerConfig`
2. Controller detects the `golden:*` prefix and pauses the machine pool (**hard gate** — no provisioning until resolved)
3. Controller reads the cluster's `spec.kubernetesVersion` to determine the RKE2 version
4. Queries Hetzner API for a cached snapshot matching version + CIS profile
5. Cache miss → creates a Packer builder Job
6. On Job completion → resolves snapshot ID, patches `HetznerConfig`, unpauses machine pool
7. All failure paths set `hcloud-image.cattle.io/error` annotation with a self-explaining message

## Key Features

- **Per-cluster RKE2 version**: reads `spec.kubernetesVersion` from the Cluster CRD (fallback to global config)
- **Hard pause gate**: machine pool is paused before build starts — Rancher cannot provision with unresolved `golden:*`
- **Self-explaining errors**: every failure path writes a human-readable annotation visible via `kubectl get hetznerconfig -o yaml`
- **Deterministic Job names**: hash-based naming prevents duplicate Jobs and orphaned resources
- **Snapshot cache**: reuses existing snapshots when version + CIS profile match

## Project Structure

```
cmd/controller/main.go              Entry point (config, manager, leader election)
internal/controller/
  types.go                          Constants, GVKs, Config struct
  reconciler.go                     Reconcile loop + state machine
  cluster.go                        Cluster discovery, credentials, pause/unpause
  builder.go                        K8s Job + credential Secret creation
  hetzner.go                        Hetzner Cloud API (snapshot lookup)
  helpers.go                        Annotations, name sanitization
  *_test.go                         68 Ginkgo specs (BDD tests)
chart/hcloud-image-controller/      Helm chart (Deployment, RBAC, ServiceAccount)
```

## Prerequisites

- Rancher management cluster on Hetzner Cloud
- [zsys-studio Hetzner Node Driver](https://github.com/zsys-studio/rancher-hetzner-cluster-provider) installed
- Cloud Credentials configured in Rancher (provides `HCLOUD_TOKEN` to builder)
- Builder image (`ghcr.io/mbilan1/hcloud-image-builder`) available

## Quick Start

```bash
# Build controller binary
make build

# Run quality gate (tidy + fmt + vet + test + lint + docker lint + helm lint)
make quality-gate

# Build Docker image
make docker-build

# Lint only
make lint-all
```

## Quality Gate

The quality gate (`make quality-gate`) runs all checks before every commit:

| Layer | Tool | What it checks |
|-------|------|----------------|
| Go | `go mod tidy` | Dependency hygiene |
| Go | `gofmt` | Formatting |
| Go | `go vet` | Correctness |
| Go | `go test` (68 specs) | Unit tests (Ginkgo/Gomega) |
| Go | `golangci-lint` (~30 linters, paranoid) | Style, security, complexity, errors |
| Docker | `hadolint` | Dockerfile best practices |
| Docker | `trivy` (HIGH+CRITICAL) | Misconfigurations |
| Helm | `helm lint --strict` | Chart validity |
| Helm | `kubeconform -strict` | K8s schema validation |
| Helm | `kube-linter` | Security + best practices |
| Helm | `pluto` | Deprecated API detection |

## Deployment

The controller is deployed via the Helm chart in `chart/hcloud-image-controller/`:

```bash
helm install hcloud-image-controller chart/hcloud-image-controller \
  --namespace hcloud-image-system \
  --create-namespace
```

For automated deployment via `terraform-hcloud-rancher`, set `install_hcloud_image_controller = true`.

## Configuration

See [chart/hcloud-image-controller/values.yaml](chart/hcloud-image-controller/values.yaml) for all configurable values.

Key defaults:

| Parameter | Default | Description |
|---|---|---|
| `defaults.rke2Version` | `v1.34.4+rke2r1` | Fallback RKE2 version (cluster version takes priority) |
| `defaults.location` | `hel1` | Hetzner datacenter for builder |
| `defaults.serverType` | `cx23` | Server type for builder |
| `builder.image.tag` | `0.1.0` | Builder image version |

## Annotations

The controller sets these annotations on `HetznerConfig` resources:

| Annotation | Values | Description |
|---|---|---|
| `hcloud-image.cattle.io/status` | `building` / `resolved` / `failed` | Current state |
| `hcloud-image.cattle.io/job-name` | Job name | Builder Job reference |
| `hcloud-image.cattle.io/snapshot-id` | Numeric ID | Resolved snapshot |
| `hcloud-image.cattle.io/rke2-version` | e.g. `v1.34.4+rke2r1` | Effective version used |
| `hcloud-image.cattle.io/error` | Human-readable message | Self-explaining error |
| `hcloud-image.cattle.io/original-spec` | e.g. `golden:cis` | Original image value |

## Sibling Repositories

| Repo | Purpose |
|---|---|
| [packer-hcloud-rke2](https://github.com/mbilan1/packer-hcloud-rke2) | Packer template + builder image |
| [terraform-hcloud-rancher](https://github.com/mbilan1/terraform-hcloud-rancher) | Management cluster (deploys this controller) |
| [rancher-hetzner-cluster-templates](https://github.com/mbilan1/rancher-hetzner-cluster-templates) | Downstream cluster templates (uses `golden:*` images) |
| [rke2-hetzner-architecture](https://github.com/mbilan1/rke2-hetzner-architecture) | Architecture decisions (ADR-012, DES-004) |

## License

MIT
