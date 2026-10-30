IMAGE ?= docker.io/werton13/vcd-k8s-node-controller
TAG ?= 0.11.15
GO ?= go
CONTROLLER_GEN ?= controller-gen

.PHONY: all fmt vet test build run run-ui docker-build image image-push image-save generate manifests helm-lint clean

all: fmt vet test build

fmt: ; $(GO) fmt ./...

vet: ; $(GO) vet ./...

test: ; $(GO) test ./...

build: ; CGO_ENABLED=0 $(GO) build -o bin/manager ./cmd/manager && CGO_ENABLED=0 $(GO) build -o bin/ui ./cmd/ui

run: ; $(GO) run ./cmd/manager

# The management page needs a kubeconfig and, unless --auth-disabled is passed,
# the accounts Secret.
run-ui: ; $(GO) run ./cmd/ui

docker-build: ; docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

image: ; docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

image-push: ; docker push $(IMAGE):$(TAG) && docker push $(IMAGE):latest

image-save: ; mkdir -p dist && docker save -o dist/vcd-k8s-node-controller-$(TAG).tar $(IMAGE):$(TAG)

generate: ; $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

manifests: ; $(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd/bases

helm-lint: ; helm lint charts/vcd-nodescale

clean: ; $(GO) clean ./...
