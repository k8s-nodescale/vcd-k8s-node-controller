# vcd-k8s-node-controller

A Go Kubernetes controller (operator) that scales **worker nodes** for Kubernetes
clusters running on **VMware Cloud Director (vCD)**.

This repository is intentionally **separate** from the Terraform module
`tf-vcloud-k8s-module`. Terraform performs the initial cluster bootstrap; this
controller owns worker scaling afterwards (see the ownership decision in the
design doc).

## Status

Early skeleton — the structure, API, IP allocator, token/CA logic, cloud-init
renderer and packaging are in place. Several provider methods are deliberate
stubs (see Roadmap).

Implemented:

- `K8sWorkerPool` CRD (`infra.example.com/v1alpha1`) + generated deepcopy/CRD.
- `ipam` allocator aware of Terraform's `ip_plan` (gateway, VIPs, masters, DVM,
  explicit reservations) with unit tests.
- `kube` provider: bootstrap-token creation, CA hash from `kube-root-ca.crt`,
  node listing/readiness, node deletion (client-go).
- `cloudinit` renderer: vCD initscript + worker user-data templates.
- Reconciler skeleton with a working scale-up flow and a guarded scale-down TODO.
- Helm chart, kustomize manifests, RBAC, sample CR, CI workflow.

Not implemented yet:

- `govcd` VM create/delete, internal disks, guest customization, LB pool members.
- Scale-down (cordon/drain/reset/delete) and PDB-aware eviction.
- Option A disk + `/var` relocation details (deferred by design).
- LB pool membership added only after the Node reports `Ready`.
- Finalizers and full reconciliation of `status.nodes`.

## Layout

```
api/v1alpha1/            CRD Go types, group registration, generated deepcopy
cmd/manager/             controller-runtime entrypoint
internal/controller/     WorkerPool reconciler (+ Node watch)
internal/cloud/          cloud.Provider interface
internal/cloud/vcd/      govcd-backed implementation (stubs)
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
2. Install the chart:

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

1. Implement `govcd` `CreateVM` / `DeleteVM` and verify against a dev tenant.
2. Finish scale-down: cordon, drain, LB removal, delete, ledger free.
3. Implement Option A cloud-init (disks, `/var` relocation, reboot guard).
4. Add LB pool membership after `Node Ready`; remove on scale-in.
5. Finalizers, retries/backoff, e2e on a dev tenant (2 -> 4 -> 2 via CRD).
6. Publish image + chart; wire the Terraform `node_controller` variable.

## License

Apache-2.0 (add the full `LICENSE` text before publishing).
