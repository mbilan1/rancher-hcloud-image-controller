# Hcloud Image Controller

Kubernetes controller that automatically builds Hetzner Cloud hcloud images
(Packer snapshots) when `HetznerConfig` CRDs use the `golden:*` image convention.

Part of the [RKE2-on-Hetzner platform](https://github.com/mbilan1/rke2-hetzner-architecture) — see [ADR-012](https://github.com/mbilan1/rke2-hetzner-architecture/blob/main/decisions/adr-012-hcloud-image-controller.md) and [DES-004](https://github.com/mbilan1/rke2-hetzner-architecture/blob/main/designs/des-004-hcloud-image-controller.md).

## How It Works

1. User creates a downstream cluster template with `image: "golden:cis"` in `HetznerConfig`
2. Controller detects the `golden:*` prefix and creates a Packer build Job
3. Job builds a Hetzner Cloud snapshot with RKE2 pre-installed (+ CIS hardening if `cis` profile)
4. Controller patches the `HetznerConfig` with the resolved snapshot ID
5. Rancher provisions the cluster using the hcloud image

## Prerequisites

- Rancher management cluster on Hetzner Cloud
- [zsys-studio Hetzner Node Driver](https://github.com/zsys-studio/rancher-hetzner-cluster-provider) installed
- Cloud Credentials configured in Rancher (provides `HCLOUD_TOKEN` to builder)
- Builder image (`ghcr.io/mbilan1/hcloud-image-builder`) available

## Quick Start

```bash
# Build controller binary
make build

# Lint Helm chart
make helm-lint

# Build Docker images (controller + builder)
make docker-build
make docker-build-builder

# Push to registry
make docker-push
make docker-push-builder
```

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
| `defaults.rke2Version` | `v1.34.4+rke2r1` | RKE2 version for hcloud images |
| `defaults.location` | `hel1` | Hetzner datacenter for builder |
| `defaults.serverType` | `cx23` | Server type for builder |
| `builder.image.tag` | `0.1.0` | Builder image version |

## Sibling Repositories

| Repo | Purpose |
|---|---|
| [packer-hcloud-rke2](https://github.com/mbilan1/packer-hcloud-rke2) | Packer template + builder image |
| [terraform-hcloud-rancher](https://github.com/mbilan1/terraform-hcloud-rancher) | Management cluster (deploys this controller) |
| [rancher-hetzner-cluster-templates](https://github.com/mbilan1/rancher-hetzner-cluster-templates) | Downstream cluster templates (uses `golden:*` images) |

## License

MIT
