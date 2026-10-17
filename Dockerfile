# syntax=docker/dockerfile:1

FROM golang:1.27.1-alpine AS build
WORKDIR /src

# Public defaults; override with --build-arg if your network uses a mirror.
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB} \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.title="vcd-k8s-node-controller" \
      org.opencontainers.image.description="Kubernetes worker-node scaling controller for VMware Cloud Director" \
      org.opencontainers.image.source="https://github.com/werton13/vcd-k8s-node-controller" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
