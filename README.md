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
- `cloud/vcd` provider on `govcd`: create VM from a catalog template, grow the
  system disk when `flavor.osDiskGB` asks for it, paravirtual SCSI data disks,
  IP-assigned NIC on the org network, guest customization with the rendered
  cloud-init payload. `DeleteVM` powers the VM off and removes it together with
  its disks; it is idempotent.
- Scale-in: lowering `spec.replicas` removes the highest-ordinal worker, while
  `spec.scaleDown.deleteWorkers` and the `infra.example.com/delete-worker`
  annotation remove a worker **by name**. Every removal is cordon -> ingress-pool
  removal -> PDB-aware drain (eviction API) -> volume-detach wait -> Node delete
  -> VM delete -> address release, bounded by `scaleDown.drainTimeout` and
  guarded by `minReplicas` and a node-local volume check. See
  [Scale in](#scale-in--removing-a-worker).
- Ingress pool membership: workers are added to the NSX-V edge pool once Ready
  and removed before the drain starts (`ingressPool.type/name/port/edge`).
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

- NSX-T ingress pools (`ingressPool.type: nsxt` is rejected with a clear event;
  the node is still drained and deleted, but its pool member must be removed by
  hand).
- Finalizers and a failed-provisioning watchdog: a VM that boots but never joins
  is counted in `observedReplicas` and is not replaced automatically. A drain
  that hits its timeout is marked `Failed` and kept, it is not retried for you.
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
| `flavor.osDiskGB` | no | Grows the cloned template's system disk (bus 0/unit 0) before the first power-on. Terraform nodes use `disks_config.osdisk.sizegb`; a clone without this keeps the template size (10 GB on the current Ubuntu template). Shrinking is refused. |
| `flavor.template` | no | vCD catalog template (must expose `disk.EnableUUID`). |
| `flavor.disks[]` | no | Data disks: `sizeGB`, `mount` (`/var`, `/var/lib`), `busType`, `busNumber`, `unitNumber`, `storageProfile`. Mirrors Terraform `diskw1` / `diskw2`. |
| `vcd.org` / `vcd.vdc` / `vcd.vapp` / `vcd.orgNetwork` | yes | Placement of the VM. |
| `vcd.catalog` / `vcd.catalogOrg` | no | Catalog holding the template (defaults to the controller org). |
| `ipPlan` | yes | Mirrors Terraform's `ip_plan`: `cidr`, `mNodeOffset`, `wNodeOffset`, `dvmOffset`, `kubeapiLB`, `ingressLB`, `masterCount`, `reserved[]`. Workers are allocated from `wNodeOffset` upwards. |
| `join.endpoint` | no | Control-plane address, `host:port` of the API VIP. |
| `join.tokenTTL` | no | Lifetime of the per-VM bootstrap token (default `1h`). |
| `join.labels` / `join.taints` | no | Pool-wide, applied to every Ready worker (`taints` in `key[=value]:Effect` form). |
| `ingressPool` | no | `type` (`nsxv`/`nsxt`), `name`, `port`, `edge`. `port` is the **node port** the pool forwards to, not the virtual-server port; `edge` is the NSX-V edge that owns the pool. Workers are added on Ready and removed before a drain. |
| `scaleDown.minReplicas` | no | Floor for both scale-down paths. `0` disables the floor. |
| `scaleDown.drainTimeout` | no | Budget for cordon+drain of one worker (default `10m`). When it expires the worker is `Failed` and its VM is kept. |
| `scaleDown.deleteWorkers[]` | no | Worker names to remove regardless of their position. Each entry is drained and deleted, then removed from this list and `spec.replicas` is decremented so the worker is **not** recreated. |
| `scaleDown.force` | no | Skip the node-local volume guard. Per-node override: annotation `infra.example.com/force-delete=true`. Never skips PDBs or the detach wait. |

`status`:

| Field | Meaning |
| --- | --- |
| `observedReplicas` | Workers the controller knows about (VM exists). |
| `ledger.nextIndex` / `used[]` / `free[]` | Address book. `used` holds every allocated worker IP; addresses of deleted VMs are never released automatically. |
| `nodes[]` | Per worker: `name`, `ip`, `vmID`, `phase`, `cpu`, `memoryMB`, `diskGB`, `k8sReady`, `kubeletVersion`, `message`, `deleteStartedAt`. `message` is where a blocked removal explains itself (the PDB, the attached volume, the local PV). |
| `conditions[]` | `Ready`, with reason `Converged` (`3/3 workers Ready`), `Progressing` while a worker is not Ready yet, `ScaleUpFailed` or `InvalidSpec` on errors. |

Phases: `Provisioning` (VM created, Node not registered yet) -> `Joining` (Node
registered, not Ready) -> `Ready`. Removal uses `Draining` (cordon, pool
removal, pod eviction) -> `Deleting` (detach wait, Node and VM deletion).
`Failed` means the drain hit `drainTimeout` and the VM was kept.

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
    osDiskGB: 20
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
    edge: demo-edge-gateway
    port: 30080        # ingress controller NodePort, not 443
  scaleDown:
    minReplicas: 2
    drainTimeout: 10m
    deleteWorkers: []  # e.g. [demo-cluster-wrk-02]
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

Three ways to ask, one implementation:

| Request | Removes | Recreates? |
| --- | --- | --- |
| `spec.replicas: 2` (was 3) | the highest-ordinal worker (the one at the end of the pool) | no |
| `spec.scaleDown.deleteWorkers: ["demo-cluster-wrk-02"]` | exactly that worker; the entry is pruned and `replicas` is decremented when it is gone | no |
| `kubectl annotate node demo-cluster-wrk-03 infra.example.com/delete-worker=true` | exactly that worker, with `replicas` untouched | yes, a replacement is created (rolling replacement) |

A removal is one step per reconcile pass, visible in `status.nodes[].phase`:

1. **Guards.** Refuse to go below `scaleDown.minReplicas`. Refuse when Bound
   `hostPath` PVs used by the node or PVs pinned to it by `nodeAffinity`
   (local volumes) would lose their data - the message names the PVs and the
   override (`scaleDown.force: true` or the node annotation
   `infra.example.com/force-delete=true`).
2. **Cordon + ingress pool.** Patch `spec.unschedulable`, then remove the
   address from `ingressPool` so no new request is sent to a dying node. A pool
   removal that cannot be performed (NSX-T, missing `edge`, pool not found)
   raises a `IngressPoolMemberNotRemoved` event instead of silently pretending.
3. **Drain through the eviction API.** DaemonSet and mirror pods are skipped;
   every other pod is evicted with the eviction subresource, so a
   PodDisruptionBudget blocks the eviction instead of being bypassed. While a
   budget has no disruptions left the worker stays `Draining` and the message
   names the budget. The whole step is bounded by `scaleDown.drainTimeout`; when
   it expires the phase becomes `Failed` and the VM is kept.
4. **Volume gate.** Wait until no `VolumeAttachment` references the node.
   Destroying a VM with an attached volume strands the volume in the storage
   backend and can leave the PV bound forever.
5. **Delete.** Node object first, then the VM (power off, remove, disks
   included), then release the address in `status.ledger` and drop the worker
   from `status.nodes`.

Notes:

- `PVs` are not deleted. A `Retain` PV survives its PVC and must be cleaned up
  by hand; a `Delete` PV is reclaimed only when its PVC is deleted.
- Pods using `emptyDir` are evicted and their data goes with them - that is the
  point of deleting a node.
- A `Failed` removal is not retried automatically: fix the blocker (raise the
  budget, relax the PDB) and remove the annotation, or re-request the worker,
  then delete the entry from `status` by recreating the pool request.
- Nothing is removed from the vApp except the VM: its disks are part of the VM,
  the data-disk VMDKs included.

## Terraform integration contract

- The Terraform module installs this chart behind a `node_controller.enabled`
  toggle and pins `chart_version` / `image_tag` (currently `0.3.1`).
- **Once the controller is enabled, do not use Terraform to manage worker count
  or worker LB pool membership.** Use the CRD instead. The Terraform module's
  README must state this explicitly.
- Initial cluster bootstrap remains Terraform + Ansible/DVM; the controller only
  handles post-bootstrap scaling.

## Design

See [`docs/conception.md`](docs/conception.md) for the full decision record,
architecture, algorithms and open questions.

## Roadmap

1. Failed-provisioning watchdog (VM up but Node absent for N minutes).
2. Token renewal / re-issued join for slow installs; delete the token and its
   JWS entry once the node is Ready (a failed create already revokes its token).
3. NSX-T ALB pool membership (the scale-in path already drains and deletes; it
   only cannot edit the pool).
4. Finalizers, e2e on a dev tenant (2 -> 4 -> 2 via the CRD, and a named
   deletion in the middle of the pool).

## Container image

The image is published to Docker Hub on a `v*` tag:
`docker.io/werton13/vcd-k8s-node-controller` (`.github/workflows/docker-publish.yaml`).

Build locally (the Dockerfile uses `golang:1.27.1-alpine` and
`gcr.io/distroless/static:nonroot`):

```
docker build -t docker.io/werton13/vcd-k8s-node-controller:0.3.1 .
```

Public module defaults are used inside the build. On a restricted network,
override them:

```
docker build \
  --build-arg GOPROXY=https://nexus.example.com/repository/golang/ \
  --build-arg GOSUMDB=off \
  -t docker.io/werton13/vcd-k8s-node-controller:0.3.1 .
```

Push (or use `make image-push`):

```
docker login docker.io
docker push docker.io/werton13/vcd-k8s-node-controller:0.3.1
docker push docker.io/werton13/vcd-k8s-node-controller:latest
```

The image is `linux/amd64` and runs as distroless `nonroot` (UID 65532).

## Releasing a new version

Every version of the controller ships to four places. Missing one is how the
chart, the image and the Terraform pin drift apart (`0.1.1` in tfvars while
`0.1.5` ran), so do all of them in one pass:

1. **Version bump in this repo** - `Makefile` (`TAG`), `charts/vcd-nodescale/Chart.yaml`
   (`version` + `appVersion`), `internal/cloudinit`'s `RenderVersion` when the guest
   scripts change, and the image examples in this README.
2. **Image** - `make image` then `make image-push` (pushes `:<tag>` and `:latest`).
   Built locally: the `docker-publish` workflow needs `DOCKERHUB_USERNAME` /
   `DOCKERHUB_TOKEN` and fails without them.
3. **Chart on GitHub Pages** - `helm package charts/vcd-nodescale`, copy the
   `.tgz` into a `gh-pages` checkout, `helm repo index . --url
   https://werton13.github.io/vcd-k8s-node-controller/`, commit and push. The
   Pages deploy job can sit `queued` for 10+ minutes; verify
   `https://werton13.github.io/vcd-k8s-node-controller/vcd-nodescale-<tag>.tgz`
   returns 200 before calling it done.
4. **Pins in the consumers** - the same version in all three:
   - `tf-vcloud-k8s` `variables.tf` (`node_controller.chart_version` /
     `image_tag` defaults) - commit and push branch `dev`;
   - test/production `terraform.tfvars` files (`node_controller` block) - the
     authoritative pin, it is what Ansible installs;
   - `k8s-kubeadm-r2` `roles/k8s_addons_node_controller/defaults/main.yml`
     (both vars) and the role README table - commit and push branch `dev`.

Finally tag `v<version>` and push: `ci` must be green, and `docker-publish`
runs even though it cannot log in.

## License

Apache-2.0, see [`LICENSE`](LICENSE).
