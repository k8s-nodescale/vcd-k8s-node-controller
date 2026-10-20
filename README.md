# vcd-k8s-node-controller

A Go Kubernetes controller (operator) that scales **worker nodes** for Kubernetes
clusters running on **VMware Cloud Director (vCD)**.

This repository is intentionally **separate** from the Terraform module
`tf-vcloud-k8s-module`. Terraform performs the initial cluster bootstrap; this
controller owns worker scaling afterwards (see the ownership decision in the
design doc).

## Status

Working end to end on the dev tenant (`KION-WEB-DEV`, NSX-V, vApp `K8s_cluster`):
adding replicas to a `K8sWorkerPool` creates a VM, lays out its data disks,
boots the guest, joins it to the cluster and reports it Ready.

Implemented:

- `K8sWorkerPool` CRD (`infra.example.com/v1alpha1`) + generated deepcopy/CRD.
- `ipam` allocator aware of Terraform's `ip_plan` (gateway, VIPs, masters, DVM,
  explicit reservations). Worker names are derived from the address ordinal and
  existing VMs are adopted into the ledger on every pass, so a lost status can
  never produce a duplicate name or IP.
- `cloud/vcd` provider on `govcd`: create VM from a catalog template, paravirtual
  SCSI data disks, IP-assigned NIC on the org network, guest customization with
  the rendered cloud-init payload. `DeleteVM` is still a stub.
- Guest bootstrap (`internal/cloudinit`): swap off, kernel modules/sysctl,
  containerd, pinned `pkgs.k8s.io` packages, `kubeadm join`, run by a systemd
  unit that retries and is re-armed on later boots. One deliberate reboot to
  relocate `/var` and `/var/lib` onto the data disks.
- Readiness: `Provisioning` -> `Joining` -> `Ready` from the real Node objects;
  allocatable CPU/memory, kubelet version, and `Ready=True/Converged` once every
  requested worker is Ready.
- `join.labels` applied to the Node after it is Ready (kubeadm 1.35 has no
  `--node-labels`), `join.taints` merge-patched onto Ready nodes.
- Exactly one status write per pass, with conflicts treated as "retry next
  pass". Two writes per pass used to make the controller fight its own
  resourceVersion and never converge.

Not implemented yet:

- `govcd` VM delete and scale-down (cordon, PDB-aware drain, disk cleanup).
- Ingress LB pool membership (add only after the Node is Ready).
- Finalizers and a failed-provisioning watchdog: a VM that boots but never joins
  is counted in `observedReplicas` and is not replaced automatically.
- Token renewal for joins that outlive `join.tokenTTL`.

## Layout

```
api/v1alpha1/            CRD Go types, group registration, generated deepcopy
cmd/manager/             controller-runtime entrypoint
internal/controller/     WorkerPool reconciler (+ Node watch)
internal/cloud/          cloud.Provider interface
internal/cloud/vcd/      govcd-backed implementation
internal/kube/           kube.Provider interface + client-go implementation
internal/ipam/           ip_plan-aware IP ledger/allocator (+ tests)
internal/cloudinit/      renderer + embedded templates
config/                  kustomize: crd, rbac, manager, samples
charts/vcd-nodescale/    published Helm chart (CRDs in crds/)
docs/                    design doc
test/                    test strategy / e2e notes
```

## Build & test

```
go build ./...
go test ./...
go vet ./...
helm lint charts/vcd-nodescale
```

Regenerate deepcopy/CRD (requires `controller-gen`):

```
make generate
make manifests
```

## Install

1. Create the vCD credentials secret in the target namespace (keys: `url`,
   `user`, `password`, `org`, `vdc`, optional `insecure`).
2. Install the chart from the published repository:

```
helm repo add vcd-nodescale https://werton13.github.io/vcd-k8s-node-controller
helm repo update
helm install vcd-nodescale vcd-nodescale/vcd-nodescale \
  --namespace vcd-nodescale-system --create-namespace
```

Or from a checkout:

```
helm install vcd-nodescale charts/vcd-nodescale \
  --namespace vcd-nodescale-system --create-namespace
```

3. Apply a `K8sWorkerPool` (see `config/samples/`). Change `spec.replicas` to
   scale out or in.

## Terraform integration contract

- The Terraform module installs this chart behind a `node_controller.enabled`
  toggle and pins `chart_version` / `image_tag` (future work).
- **Once the controller is enabled, do not use Terraform to manage worker count
  or worker LB pool membership.** Use the CRD instead. The Terraform module's
  README must state this explicitly.
- Initial cluster bootstrap remains Terraform + Ansible/DVM; the controller only
  handles post-bootstrap scaling.

## Design

See [`docs/conception.md`](docs/conception.md) for the full decision record,
architecture, algorithms and open questions.

## Roadmap

1. Implement `govcd` `DeleteVM` and finish scale-down: cordon, drain, LB
   removal, delete, ledger free, node delete.
2. Failed-provisioning watchdog (VM up but Node absent for N minutes).
3. Token renewal / re-issued join for slow installs.
4. LB pool membership after `Node Ready`; remove on scale-in.
5. Finalizers, e2e on a dev tenant (2 -> 4 -> 2 via the CRD).
6. Wire the Terraform `node_controller` variable.

## Container image

The image is published to Docker Hub on a `v*` tag:
`docker.io/werton13/vcd-k8s-node-controller` (`.github/workflows/docker-publish.yaml`).

Build locally (the Dockerfile uses `golang:1.27.1-alpine` and
`gcr.io/distroless/static:nonroot`):

```
docker build -t docker.io/werton13/vcd-k8s-node-controller:0.1.2 .
```

Public module defaults are used inside the build. On a restricted network,
override them:

```
docker build \
  --build-arg GOPROXY=https://nexus.example.com/repository/golang/ \
  --build-arg GOSUMDB=off \
  -t docker.io/werton13/vcd-k8s-node-controller:0.1.2 .
```

Push (or use `make image-push`):

```
docker login docker.io
docker push docker.io/werton13/vcd-k8s-node-controller:0.1.2
docker push docker.io/werton13/vcd-k8s-node-controller:latest
```

The image is `linux/amd64` and runs as distroless `nonroot` (UID 65532).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
