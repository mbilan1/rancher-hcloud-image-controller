# Claude Instructions — rancher-hcloud-image-controller

> Single source of truth for AI agents working on this repository.
> AGENTS.md redirects here. Read this file in full before any task.

---

## What This Repository Is

A **Kubernetes controller** that automatically builds Hetzner Cloud hcloud images (Packer snapshots) when `HetznerConfig` CRDs use the `golden:*` image convention.

- **Language**: Go 1.24
- **Framework**: controller-runtime v0.20.0
- **Layout**: Kubebuilder (`cmd/controller/`, `internal/controller/`)
- **Testing**: Ginkgo v2 + Gomega (68 specs)
- **Linting**: golangci-lint (~30 linters, paranoid mode)
- **Deployment**: Helm chart (deployed via cloud-init HelmChart CRD in terraform-hcloud-rancher)
- **Status**: Active development

### What This Controller Does

1. Watches `rke-machine-config.cattle.io/v1 HetznerConfig` CRDs for `golden:*` prefix in `.image` field
2. Reads per-cluster RKE2 version from `Cluster.spec.kubernetesVersion` (fallback to global config)
3. Pauses machine pool (**hard gate** — refuses to build if pause fails)
4. Creates a Kubernetes Job running the builder image (Packer + Ansible)
5. Builder creates a Hetzner Cloud snapshot with RKE2 pre-installed
6. Controller patches HetznerConfig with resolved snapshot ID
7. Unpauses machine pool — Rancher provisions cluster nodes using the hcloud image
8. All failure paths set `hcloud-image.cattle.io/error` annotation with self-explaining message

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
1. **Run `make quality-gate`** after any change (Go + Docker + Helm linters)
2. **Read the relevant file before editing**

---

## Repository Structure

| Path | Purpose |
|------|---------|
| `cmd/controller/main.go` | Entry point — logger, config, manager setup (81 LOC) |
| `internal/controller/types.go` | Constants, GVKs, Config, reconciler struct (105 LOC) |
| `internal/controller/reconciler.go` | Reconcile loop + state machine (400 LOC) |
| `internal/controller/cluster.go` | Cluster discovery, cloud credentials, pause/unpause (161 LOC) |
| `internal/controller/builder.go` | Builder Job + credential Secret creation (199 LOC) |
| `internal/controller/hetzner.go` | Hetzner Cloud API — snapshot lookup (63 LOC) |
| `internal/controller/helpers.go` | Annotations, name sanitization, deterministic Job naming (64 LOC) |
| `internal/controller/*_test.go` | Ginkgo tests — 68 specs |
| `go.mod` | Go module definition |
| `Dockerfile` | Multi-stage build (golang:1.24.0-alpine → distroless) |
| `Makefile` | Build, test, lint-all, quality-gate, Docker, Helm targets |
| `.golangci.yml` | Paranoid linter config (~30 linters) |
| `.github/workflows/` | CI: lint-go, lint-helm, sast-checkov, sast-kics, unit-test |
| `chart/hcloud-image-controller/` | Helm chart for deployment |

---

## Quality Gate

Run `make quality-gate` before every commit. It validates all 3 layers:

| Layer | Tools |
|-------|-------|
| Go | tidy, fmt, vet, test (68 specs), golangci-lint (~30 linters) |
| Docker | hadolint, trivy (HIGH+CRITICAL) |
| Helm | helm lint --strict, kubeconform, kube-linter, pluto |

---

## Code Style

- **Go**: `gofmt` canonical style
- **Helm**: Standard chart conventions
- **Naming**: camelCase for Go, snake_case for Helm values
- **Comments**: end with a period (godot linter enforced)

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
