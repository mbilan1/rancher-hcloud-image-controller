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
        clean all

# ── Go ────────────────────────────────────────────────────────────────────────

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) .

test:
	go test ./... -v -count=1

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

# ── Docker — Controller ──────────────────────────────────────────────────────

docker-build:
	docker build -t $(CONTROLLER_IMAGE) .

docker-push: docker-build
	docker push $(CONTROLLER_IMAGE)

# ── Docker — Builder ─────────────────────────────────────────────────────────
# Builder image lives in packer-hcloud-rke2/builder/ (tightly coupled to Packer template).
# This target builds it from the sibling repo for convenience.

docker-build-builder:
	docker build -t $(BUILDER_IMAGE) $(BUILDER_DIR)

docker-push-builder: docker-build-builder
	docker push $(BUILDER_IMAGE)

# ── Helm ──────────────────────────────────────────────────────────────────────

helm-lint:
	helm lint $(CHART_DIR)

helm-template:
	helm template test $(CHART_DIR) --namespace hcloud-image-system

helm-package:
	helm package $(CHART_DIR) -d bin/

# ── Convenience ───────────────────────────────────────────────────────────────

all: tidy fmt vet build helm-lint

clean:
	rm -rf bin/
