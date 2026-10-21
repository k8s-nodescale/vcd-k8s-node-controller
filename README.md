# vcd-k8s-node-controller

A Go Kubernetes controller (operator) that scales **worker nodes** for Kubernetes
clusters running on **VMware Cloud Director (vCD)**.

This repository is intentionally **separate** from the Terraform module
`tf-vcloud-k8s-module`. Terraform performs the initial cluster bootstrap; this
controller owns worker scaling afterwards (see the ownership decision in the
design doc).

## Status

Working end to end on a dev tenant (vCD, NSX-V): adding a replica to a
`K8sWorkerPool` creates a VM, lays out its data disks, boots the guest, joins it
to the cluster and reports it Ready - about five minutes per worker.

Implemented:

- `K8sWorkerPool` CRD (`infra.example.com/v1alpha1`) + generated deepcopy/CRD.
- `ipam` allocator aware of Terraform's `ip_plan` (gateway, VIPs, masters, DVM,
  explicit reservations). Worker names are derived from the address ordinal and
  existing VMs are adopted into the ledger on every pass, so a lost status can
  never produce a duplicate name or IP.
- `cloud/vcd` provider on `govcd`: create VM from a catalog template, paravirtual
  SCSI data disks, IP-assigned NIC on the org network, guest customization with
  the rendered cloud-init payload. `DeleteVM` is still a stub.
- Guest bootstrap (`internal/cloudinit`) in two stages, because vCD runs the
  customisation script in a phase it can fail the VM in:
  - **preCustomization** writes only what vCD's own network step needs (netplan,
    NoCloud seed) and keeps the interface up, so the phase cannot stall;
  - **postCustomization** prepares the data disks (`/var`, `/var/lib`) and bumps
    the cloud-init instance epoch; vCD's reboot then pays for the move.
  Neither phase reboots or exits non-zero - a failing phase is terminal for the
  VM - and each stage records its result in `/etc/vcd-stage/state`.
- Boot work is done by two marker-guarded systemd units: `k8s-node-prereqs`
  (swap, modules, sysctl, containerd, pinned `pkgs.k8s.io` packages) and
  `k8s-node-join` (layout gate, wait for the API, bounded `kubeadm join`), both
  `Restart=on-failure`. A `vcd-ensure-layout` unit mounts the data disks where
  the spec says before either runs, and `vcd-verify-layout` refuses to run apt
  or `kubeadm join` on a guest whose `/var` move did not take effect.
- Every join prerequisite, so a created VM needs no manual cluster surgery: a
  bootstrap token (`bootstrap-token-<id>`), the matching `jws-kubeconfig-<id>`
  signature in `kube-public/cluster-info` (kubeadm 1.35 refuses token discovery
  without it) and the CA pin computed the way kubeadm does it - `sha256` of the
  CA **public key**, not of the certificate.
- Readiness: `Provisioning` -> `Joining` -> `Ready` from the real Node objects;
  allocatable CPU/memory, kubelet version, and `Ready=True/Converged` once every
  requested worker is Ready.
- `join.labels` and `join.taints` are patched onto the Node once it is Ready,
  including labels with an empty value such as
  `node-role.kubernetes.io/worker`, so nothing about labels is baked into
  user-data.
- Exactly one status write per pass, with conflicts treated as "retry next
  pass". Two writes per pass used to make the controller fight its own
  resourceVersion and never converge.

Not implemented yet:

- `govcd` VM delete and scale-down (cordon, PDB-aware drain, disk cleanup):
  `scaleDown` is a no-op, so lowering `spec.replicas` currently does nothing.
- Ingress LB pool membership (add only after the Node is Ready).
- Finalizers and a failed-provisioning watchdog: a VM that boots but never joins
  is counted in `observedReplicas` and is not replaced automatically.
- Token renewal for joins that outlive `join.tokenTTL`; the token of a finished
  join is left to expire on its own, and its JWS entry with it. A failed create
  now revokes its token (the earlier leak left one usable credential per failed
  attempt), and every published JWS entry is read back before the join command
  is handed to the guest.

## The K8sWorkerPool resource

One `K8sWorkerPool` describes the worker set of a single cluster. You own
`spec`; the controller owns `status`.

`spec`:

| Field | Required | Meaning |
| --- | --- | --- |
| `replicas` | yes | Desired number of workers. |
| `namePrefix` | yes | DNS-safe prefix. Nodes are named `<prefix>-wrk-NN`, where `NN` is the ordinal of the worker address in `ipPlan`. |
| `bootstrap.osUser` | no | Guest account created by vCD guest customization. |
| `bootstrap.osPasswordHash` | no | `/etc/shadow` hash, not a plain password. |
| `bootstrap.sshAuthorizedKey` | no | Public key installed for `osUser`. |
| `bootstrap.nicName` | no | Guest NIC name (`ens192` on the Ubuntu template). |
| `bootstrap.dns` | no | Resolvers written into the guest. |
| `bootstrap.k8sVersion` | no | Full apt version pinned in the guest, e.g. `1.35.9-1.1`. |
| `bootstrap.k8sMinorRepo` | no | `pkgs.k8s.io` minor repo, e.g. `v1.35`. |
| `flavor.cpus` / `flavor.memoryMB` | yes | VM shape. |
| `flavor.template` | no | vCD catalog template (must expose `disk.EnableUUID`). |
| `flavor.disks[]` | no | Data disks: `sizeGB`, `mount` (`/var`, `/var/lib`), `busType`, `busNumber`, `unitNumber`, `storageProfile`. Mirrors Terraform `diskw1` / `diskw2`. |
| `vcd.org` / `vcd.vdc` / `vcd.vapp` / `vcd.orgNetwork` | yes | Placement of the VM. |
| `vcd.catalog` / `vcd.catalogOrg` | no | Catalog holding the template (defaults to the controller org). |
| `ipPlan` | yes | Mirrors Terraform's `ip_plan`: `cidr`, `mNodeOffset`, `wNodeOffset`, `dvmOffset`, `kubeapiLB`, `ingressLB`, `masterCount`, `reserved[]`. Workers are allocated from `wNodeOffset` upwards. |
| `join.endpoint` | no | Control-plane address, `host:port` of the API VIP. |
| `join.tokenTTL` | no | Lifetime of the per-VM bootstrap token (default `1h`). |
| `join.labels` / `join.taints` | no | Pool-wide, applied to every Ready worker (`taints` in `key[=value]:Effect` form). |
| `ingressPool` | no | `type` (`nsxv`/`nsxt`), `name`, `port`. Declared, not applied yet. |
| `scaleDown` | no | `minReplicas`, `drainTimeout`. Declared, not applied yet. |

`status`:

| Field | Meaning |
| --- | --- |
| `observedReplicas` | Workers the controller knows about (VM exists). |
| `ledger.nextIndex` / `used[]` / `free[]` | Address book. `used` holds every allocated worker IP; addresses of deleted VMs are never released automatically. |
| `nodes[]` | Per worker: `name`, `ip`, `vmID`, `phase`, `cpu`, `memoryMB`, `diskGB`, `k8sReady`, `kubeletVersion`, `message`. |
| `conditions[]` | `Ready`, with reason `Converged` (`3/3 workers Ready`), `Progressing` while a worker is not Ready yet, `ScaleUpFailed` or `InvalidSpec` on errors. |

Phases: `Provisioning` (VM created, Node not registered yet) -> `Joining` (Node
registered, not Ready) -> `Ready`. `Draining`, `Deleting` and `Failed` are
reserved for the scale-in path and are not produced today.

Example (placeholder org, network and template; nothing cluster-specific):

```yaml
apiVersion: infra.example.com/v1alpha1
kind: K8sWorkerPool
metadata:
  name: demo-cluster
  namespace: default
spec:
  replicas: 2
  namePrefix: demo-cluster
  bootstrap:
    osUser: ubuntu
    osPasswordHash: "$6$REPLACE_WITH_HASH"
    sshAuthorizedKey: "ssh-ed25519 AAAA... ops@example.com"
    nicName: ens192
    dns: ["8.8.8.8", "1.1.1.1"]
    k8sVersion: "1.35.9-1.1"
    k8sMinorRepo: "v1.35"
  flavor:
    cpus: 4
    memoryMB: 8192
    template: ubuntu-24.04-k8s
    disks:
      - sizeGB: 10
        mount: /var
        busType: paravirtual
        busNumber: 1
        unitNumber: 0
      - sizeGB: 30
        mount: /var/lib
        busType: paravirtual
        busNumber: 1
        unitNumber: 1
  vcd:
    org: example-org
    vdc: example-vdc
    vapp: demo-cluster
    orgNetwork: demo-cluster-net
    catalog: Templates
  ipPlan:
    cidr: 10.10.0.0/24
    mNodeOffset: 4
    wNodeOffset: 7
    dvmOffset: -3
    kubeapiLB: 2
    ingressLB: 3
    masterCount: 3
    reserved:
      - 10.10.0.1
      - 10.10.0.2
      - 10.10.0.3
  join:
    endpoint: 10.10.0.2:6443
    tokenTTL: 1h
    labels:
      node-role.kubernetes.io/worker: ""
  ingressPool:
    type: nsxv
    name: demo-cluster-ingress-pool
    port: 443
  scaleDown:
    minReplicas: 2
    drainTimeout: 10m
```

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

The chart installs the CRD, the controller and the RBAC it needs, including
`configmaps` `get`/`update` in `kube-public` for the JWS publication.

3. Apply a `K8sWorkerPool` (see `config/samples/` and the resource reference
   above), then use it as described in **How to use**.

## How to use

**Only scale-out is available today.** The controller creates workers; it does
not remove them yet (`scaleDown` is a no-op, see the roadmap).

### Scale out

Raise `spec.replicas` on the pool, one replica at a time:

```
kubectl -n default patch k8sworkerpool demo-cluster --type merge -p '{"spec":{"replicas":3}}'
```

`kubectl -n default edit k8sworkerpool demo-cluster` works as well.
`kubectl scale` does not: the CRD only exposes the `status` subresource.

The next reconcile then does the whole job in one pass:

1. takes the next address from `status.ledger` (workers start at
   `ipPlan.wNodeOffset`); the address ordinal names the node, e.g.
   `demo-cluster-wrk-03`;
2. creates a bootstrap token (`bootstrap-token-<id>`) and publishes its JWS
   signature in `kube-public/cluster-info`;
3. renders the cloud-init payload (join command, NIC, data disks) and clones the
   VM from `flavor.template`;
4. the guest bootstraps itself: swap off, containerd and kubelet from
   `bootstrap.k8sVersion`, `kubeadm join`, then one reboot to move `/var` and
   `/var/lib` onto the data disks;
5. as soon as the Node object exists the phase moves `Provisioning` ->
   `Joining` -> `Ready`, and `join.labels` / `join.taints` are applied.

Budget about five minutes per worker: ~2 min vCD clone and power-on, ~1 min
first boot with the disk move, ~1 min guest bootstrap and join, the rest waiting
for kubelet to report Ready.

Watch it:

```
kubectl -n default get k8sworkerpool demo-cluster -w

kubectl -n default get k8sworkerpool demo-cluster \
  -o jsonpath='{range .status.nodes[*]}{.name}{"\t"}{.phase}{"\t"}{.ip}{"\n"}{end}'

kubectl get nodes -l node-role.kubernetes.io/worker
```

The pool is converged when the `Ready` condition says so:

```
kubectl -n default get k8sworkerpool demo-cluster \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
# {"type":"Ready","status":"True","reason":"Converged","message":"3/3 workers Ready",...}
```

Add **one replica at a time**: `scaleUp` creates every missing VM in a single
pass and writes `status` once at the end, so a larger jump keeps reporting the
previous `Converged` condition for several minutes and hides where the pass
actually is.

### What not to do

- Don't edit `status`: the controller rewrites it on every pass. The only
  exception is pruning `status.ledger.used` after you deleted a VM by hand,
  because addresses of vanished VMs are not released automatically.
- Don't patch a VM or its user-data after creation. The join command is baked
  into user-data and vCD cannot change it, so a broken or expired token means
  deleting and recreating the VM.
- Don't manage worker count in Terraform once the controller is enabled.
- Remember that `join.labels` and `join.taints` are pool-wide: they apply to
  every Ready worker, not to one node.

### Scale in / removing a worker

Not implemented yet. Lowering `spec.replicas` is accepted but has no effect: no
VM is deleted and no node is drained. Until `DeleteVM` and the drain path land,
removal is a manual procedure (undeploy and delete the VM, free its address in
`status.ledger`, delete the Node and its bootstrap token).

## Terraform integration contract

- The Terraform module installs this chart behind a `node_controller.enabled`
  toggle and pins `chart_version` / `image_tag` (currently `0.2.0`).
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
3. Token renewal / re-issued join for slow installs; delete the token and its
   JWS entry once the node is Ready (a failed create already revokes its token).
4. LB pool membership after `Node Ready`; remove on scale-in.
5. Finalizers, e2e on a dev tenant (2 -> 4 -> 2 via the CRD).
6. Wire the Terraform `node_controller` variable.

## Container image

The image is published to Docker Hub on a `v*` tag:
`docker.io/werton13/vcd-k8s-node-controller` (`.github/workflows/docker-publish.yaml`).

Build locally (the Dockerfile uses `golang:1.27.1-alpine` and
`gcr.io/distroless/static:nonroot`):

```
docker build -t docker.io/werton13/vcd-k8s-node-controller:0.2.0 .
```

Public module defaults are used inside the build. On a restricted network,
override them:

```
docker build \
  --build-arg GOPROXY=https://nexus.example.com/repository/golang/ \
  --build-arg GOSUMDB=off \
  -t docker.io/werton13/vcd-k8s-node-controller:0.2.0 .
```

Push (or use `make image-push`):

```
docker login docker.io
docker push docker.io/werton13/vcd-k8s-node-controller:0.2.0
docker push docker.io/werton13/vcd-k8s-node-controller:latest
```

The image is `linux/amd64` and runs as distroless `nonroot` (UID 65532).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
