# Hcloud Image Controller — Makefile
#
# Targets for build, test, lint, Docker, and Helm operations.
# Container images are built manually (no CI yet).

BINARY       := hcloud-image-controller
MODULE       := github.com/mbilan1/rancher-hcloud-image-controller
VERSION      ?= 0.1.0

# Container registries
CONTROLLER_IMAGE ?= ghcr.io/mbilan1/hcloud-image-controller:$(VERSION)
BUILDER_IMAGE    ?= ghcr.io/mbilan1/hcloud-image-builder:$(VERSION)
BUILDER_DIR      ?= ../packer-hcloud-rke2/builder

# Helm
CHART_DIR := chart/hcloud-image-controller

.PHONY: build test lint fmt vet tidy \
        docker-build docker-push docker-build-builder docker-push-builder \
        helm-lint helm-template helm-package \
        lint-go lint-docker lint-helm lint-all \
        quality-gate clean all

# ── Go ────────────────────────────────────────────────────────────────────────

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/controller

test:
	go test ./... -v -count=1

test-coverage:
	go test ./... -v -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

# ── Go Linters ────────────────────────────────────────────────────────────────

lint:
	golangci-lint run ./...

lint-go: lint vet
	@echo "Go linters passed"

# ── Docker Linters ────────────────────────────────────────────────────────────
# PARANOID: All tools are required. No SKIP — install them or fail.

lint-docker:
	hadolint Dockerfile
	trivy config Dockerfile --severity HIGH,CRITICAL --exit-code 1
	@echo "Docker linters passed (dockle requires built image — run after docker-build)"

# ── Helm Linters ──────────────────────────────────────────────────────────────

helm-lint:
	helm lint $(CHART_DIR) --strict

helm-template:
	helm template test $(CHART_DIR) --namespace hcloud-image-system

helm-package:
	helm package $(CHART_DIR) -d bin/

lint-helm: helm-lint
	helm template test $(CHART_DIR) --namespace hcloud-image-system | kubeconform -strict -ignore-missing-schemas
	kube-linter lint $(CHART_DIR)
	helm template test $(CHART_DIR) --namespace hcloud-image-system | pluto detect -

# ── All Linters ───────────────────────────────────────────────────────────────

lint-all: lint-go lint-docker lint-helm
	@echo "All linters passed"

# ── Quality Gate (pre-commit) ─────────────────────────────────────────────────
# Run this before every commit. Catches issues across all 3 layers:
# Go (tidy, fmt, vet, test, golangci-lint), Docker (hadolint, trivy), Helm (lint, kubeconform, kube-linter, pluto).

quality-gate: tidy fmt vet test lint-all
	@echo ""
	@echo "✓ Quality gate passed — safe to commit"

# ── Docker — Controller ──────────────────────────────────────────────────────

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(CONTROLLER_IMAGE) .

docker-push: docker-build
	docker push $(CONTROLLER_IMAGE)

# ── Docker — Builder ─────────────────────────────────────────────────────────
# Builder image lives in packer-hcloud-rke2/builder/ (tightly coupled to Packer template).
# This target builds it from the sibling repo for convenience.

docker-build-builder:
	docker build -t $(BUILDER_IMAGE) $(BUILDER_DIR)

docker-push-builder: docker-build-builder
	docker push $(BUILDER_IMAGE)

# ── Convenience ───────────────────────────────────────────────────────────────

all: tidy fmt vet build helm-lint

clean:
	rm -rf bin/ coverage.out
