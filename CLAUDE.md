# Claude Instructions — rancher-hcloud-image-controller

> Single source of truth for AI agents working on this repository.
> AGENTS.md redirects here. Read this file in full before any task.

---

## What This Repository Is

A **Kubernetes controller** that automatically builds Hetzner Cloud hcloud images (Packer snapshots) when `HetznerConfig` CRDs use the `golden:*` image convention.

- **Language**: Go 1.23
- **Framework**: controller-runtime v0.20.0
- **Deployment**: Helm chart (deployed via cloud-init HelmChart CRD in terraform-hcloud-rancher)
- **Status**: Active development

### What This Controller Does

1. Watches `rke-machine-config.cattle.io/v1 HetznerConfig` CRDs for `golden:*` prefix in `.image` field
2. Creates a Kubernetes Job running the builder image (Packer + Ansible)
3. Builder creates a Hetzner Cloud snapshot with RKE2 pre-installed
4. Controller patches HetznerConfig with resolved snapshot ID
5. Rancher provisions cluster nodes using the hcloud image

### What This Controller Does NOT Do

| Out of scope | Where it lives |
|---|---|
| Packer template | `packer-hcloud-rke2` repo |
| Builder Docker image | `packer-hcloud-rke2/builder/` |
| Cluster provisioning | Rancher (via cluster templates) |
| DNS, LB, network | `terraform-hcloud-rke2-core` |

---

## Sibling Repositories

| Repo | Purpose |
|---|---|
| `packer-hcloud-rke2` | Packer template + builder image |
| `terraform-hcloud-rancher` | Management cluster (deploys this controller via cloud-init) |
| `rancher-hetzner-cluster-templates` | Downstream cluster templates (uses `golden:*` images) |
| `rke2-hetzner-architecture` | Architecture decisions (ADR-012, DES-004) |

---

## Critical Rules

### NEVER:
1. **Do NOT hardcode** HCLOUD_TOKEN — it comes from Rancher Cloud Credentials
2. **Do NOT start RKE2** — the builder only installs, never starts
3. **A question is NOT a request to change code**

### ALWAYS:
1. **Run `go vet ./...`** after Go file changes
2. **Run `helm lint chart/hcloud-image-controller`** after chart changes
3. **Read the relevant file before editing**

---

## Repository Structure

| Path | Purpose |
|------|---------|
| `main.go` | Controller source (single-file, ~774 LOC) |
| `go.mod` | Go module definition |
| `Dockerfile` | Multi-stage build (golang:1.23-alpine → distroless) |
| `Makefile` | Build, test, lint, Docker, Helm targets |
| `chart/hcloud-image-controller/` | Helm chart for deployment |

---

## Code Style

- **Go**: `gofmt` canonical style
- **Helm**: Standard chart conventions
- **Naming**: camelCase for Go, snake_case for Helm values

### Git Commit Convention

```
<type>(<scope>): <short summary>
```
Types: `feat`, `fix`, `docs`, `refactor`, `chore`
Scopes: `controller`, `chart`, `builder`, `docs`

---

## Language

- **Code & comments**: English
- **Commits**: English, Conventional Commits
- **User communication**: respond in the user's language
