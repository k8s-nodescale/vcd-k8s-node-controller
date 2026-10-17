IMG ?= ghcr.io/werton13/vcd-k8s-node-controller:latest
GO ?= go
CONTROLLER_GEN ?= controller-gen

.PHONY: all fmt vet test build run docker-build generate manifests helm-lint clean

all: fmt vet test build

fmt: ; $(GO) fmt ./...

vet: ; $(GO) vet ./...

test: ; $(GO) test ./...

build: ; CGO_ENABLED=0 $(GO) build -o bin/manager ./cmd/manager

run: ; $(GO) run ./cmd/manager

docker-build: ; docker build -t $(IMG) .

generate: ; $(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

manifests: ; $(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd/bases

helm-lint: ; helm lint charts/vcd-nodescale

clean: ; $(GO) clean ./...
