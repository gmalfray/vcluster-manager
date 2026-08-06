.DEFAULT_GOAL := help

# No local Go toolchain on this workstation: everything runs in a container.
# Caches live outside the repo so a rebuild is cheap and `git status` stays clean.
REPO_ROOT   := $(shell cd ../.. && pwd)
GO_IMAGE    ?= golang:1.25
CACHE       ?= $(HOME)/.cache/poc-go
ENVTEST_K8S ?= 1.35.0
CTRL_GEN    ?= v0.20.0

DOCKER_RUN = docker run --rm \
	-v $(REPO_ROOT):/src \
	-v $(CACHE)/gopath:/gopath \
	-v $(CACHE)/build:/gocache \
	-e GOPATH=/gopath -e GOCACHE=/gocache -e HOME=/gopath -e GOFLAGS=-mod=mod \
	--user $(shell id -u):$(shell id -g) \
	-w /src/poc/operator $(GO_IMAGE) sh -c

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: cache
cache:
	@mkdir -p $(CACHE)/gopath $(CACHE)/build

.PHONY: generate
generate: cache ## Regenerate deepcopy + CRD manifests (controller-gen)
	$(DOCKER_RUN) 'go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CTRL_GEN) \
		&& /gopath/bin/controller-gen object paths=./api/... \
		&& /gopath/bin/controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd'

.PHONY: test
test: cache ## Run the envtest suite (real kube-apiserver in the container)
	$(DOCKER_RUN) 'go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23 \
		&& export KUBEBUILDER_ASSETS=$$(/gopath/bin/setup-envtest use $(ENVTEST_K8S) -p path) \
		&& go vet ./... && go test -count=1 -v ./...'

.PHONY: build
build: cache ## Compile everything, including the seam assertions
	$(DOCKER_RUN) 'go build ./...'

.PHONY: fmt
fmt: cache ## gofmt
	$(DOCKER_RUN) 'gofmt -w . && gofmt -l .'

.PHONY: clean-cache
clean-cache: ## Drop the container caches (~2.5 GB)
	rm -rf $(CACHE)
