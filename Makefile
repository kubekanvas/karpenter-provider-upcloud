SHELL := /usr/bin/env bash

GOFLAGS ?=
KO_DOCKER_REPO ?= ko.local
KARPENTER_NAMESPACE ?= karpenter
CLUSTER_NAME ?=
CLUSTER_ZONE ?=

# Karpenter core owns the NodePool and NodeClaim CRDs. They are vendored into pkg/apis/crds so the
# controller can install exactly the versions it was built against, rather than whatever happens to
# be in the cluster.
KARPENTER_CORE_VERSION := $(shell go list -m -f '{{.Version}}' sigs.k8s.io/karpenter)
KARPENTER_CORE_DIR := $(shell go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the controller
	go build $(GOFLAGS) ./...

.PHONY: test
test: ## Run unit tests
	go test $(GOFLAGS) -race ./...

.PHONY: coverage
coverage: ## Run unit tests and write a coverage profile
	go test $(GOFLAGS) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy go.mod
	go mod tidy

.PHONY: generate
generate: codegen vendor-crds ## Regenerate deepcopy funcs and CRDs

.PHONY: codegen
codegen: ## Regenerate deepcopy functions and the UpCloudNodeClass CRD
	cd pkg/apis && go tool -modfile=../../go.tools.mod controller-gen \
		crd object:headerFile="../../hack/boilerplate.go.txt" \
		paths="./..." output:crd:artifacts:config=crds

.PHONY: vendor-crds
vendor-crds: ## Copy the Karpenter core CRDs matching the pinned karpenter version
	@echo "vendoring karpenter core CRDs from $(KARPENTER_CORE_VERSION)"
	cp $(KARPENTER_CORE_DIR)/pkg/apis/crds/karpenter.sh_nodepools.yaml pkg/apis/crds/
	cp $(KARPENTER_CORE_DIR)/pkg/apis/crds/karpenter.sh_nodeclaims.yaml pkg/apis/crds/
	cp $(KARPENTER_CORE_DIR)/pkg/apis/crds/karpenter.sh_nodeoverlays.yaml pkg/apis/crds/
	chmod +w pkg/apis/crds/*.yaml
	cp pkg/apis/crds/*.yaml charts/karpenter-crd/templates/

.PHONY: verify
verify: generate tidy ## Regenerate everything and fail if anything changed
	@git diff --exit-code || (echo "generated files are out of date, run 'make generate tidy'" && exit 1)

.PHONY: image
image: ## Build and publish the controller image with ko
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --bare ./cmd/controller

.PHONY: install
install: ## Install the chart into the current cluster context
	@test -n "$(CLUSTER_NAME)" || (echo "CLUSTER_NAME is required" && exit 1)
	@test -n "$(CLUSTER_ZONE)" || (echo "CLUSTER_ZONE is required" && exit 1)
	helm upgrade --install karpenter charts/karpenter \
		--namespace $(KARPENTER_NAMESPACE) --create-namespace \
		--set settings.clusterName=$(CLUSTER_NAME) \
		--set settings.clusterZone=$(CLUSTER_ZONE) \
		--wait

.PHONY: uninstall
uninstall: ## Remove the chart from the current cluster context
	helm uninstall karpenter --namespace $(KARPENTER_NAMESPACE)

.PHONY: all
all: build test vet lint ## Build, test and lint
