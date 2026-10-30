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

- `K8sWorkerPool` CRD (`k8s-nodescale.github.io/v1alpha1`) + generated deepcopy/CRD.
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
  `spec.scaleDown.deleteWorkers` and the `k8s-nodescale.github.io/delete-worker`
  annotation remove a worker **by name**. Every removal is cordon -> ingress-pool
  removal -> PDB-aware drain (eviction API, escalating past a stuck budget once
  `drainTimeout` is spent) -> volume-detach wait -> Node delete
  -> VM delete -> address release, bounded by `scaleDown.drainTimeout` and
  guarded by `minReplicas` (a floor on workers able to serve, so a `Missing`
  slot cannot make the pool look bigger than it is) and a node-local volume
  check. See
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
- Drift and reconciliation policy: a worker whose VM disappears keeps its slot
  (same name, address reserved) as `Missing` and is reported; `AutoHeal` instead
  recreates it in place with a bounded retry budget. See
  [Drift and the reconciliation policy](#drift-missing-workers-and-the-reconciliation-policy).
- `scaleDown.allowLastWorker` with `scaleDown.minReplicas: 0` empties the pool
  (refused on the controller's own node and on node-local volumes), and the chart
  exposes `replicaCount`, `nodeSelector`, `tolerations`, `affinity`,
  `priorityClassName` and `dnsPolicy` so the controller can live on the control
  plane.
- Capacity pre-flight: before a create or a resize the controller reads the
  vDC's free CPU, memory and storage and parks work that cannot fit as `Queued`
  with reason `NoResourceAvailable`, retrying as capacity appears instead of
  letting vCD fail a create halfway. See
  [When the vDC is full](#when-the-vdc-is-full).
- Rolling compute-spec changes: `spec.flavor` is the desired shape and
  `status.nodes[].flavor` the observed one; a mismatch drives the worker through
  cordon -> pool out -> drain -> power off -> resize -> power on -> Ready ->
  uncordon, resumable from `status`, bounded by `rollout.maxUnavailable`. A
  change vCD cannot apply in place - a shrink, or another `flavor.template` -
  replaces the worker instead: the same drain, then delete and create again under
  the same name and address. See
  [Rolling compute-spec changes](#rolling-compute-spec-changes).

Not implemented yet:

- NSX-T ingress pools (`ingressPool.type: nsxt` is rejected with a clear event;
  the node is still drained and deleted, but its pool member must be removed by
  hand).
- Automatic repair of a worker that is `Failed`: it is reported and kept, and
  a `Failed` drain is not retried for you. A VM that boots but never joins is
  parked by `spec.provisioning.timeout` with the VM kept.
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
| `flavor.osDiskGB` | no | Grows the cloned template's system disk (bus 0/unit 0) before the first power-on. Terraform nodes use `disks_config.osdisk.sizegb`; a clone without this keeps the template size (10 GB on the current Ubuntu template). A smaller value replaces the worker (see [Rolling compute-spec changes](#rolling-compute-spec-changes)). |
| `flavor.template` | no | vCD catalog template (must expose `disk.EnableUUID`). Another value replaces the worker: a different image is a different VM. |
| `flavor.disks[]` | no | Data disks: `sizeGB`, `mount` (`/var`, `/var/lib`), `busType`, `busNumber`, `unitNumber`, `storageProfile`. Mirrors Terraform `diskw1` / `diskw2`. |
| `provider` | no | Platform that runs the workers: `vcd` (default) or `vsphere`. The block for the selected platform is required. `vsphere` is a stub: a pool that names it is rejected as `InvalidSpec` instead of half-working. See [Platform providers](#platform-providers). |
| `vcd.org` / `vcd.vdc` / `vcd.vapp` / `vcd.orgNetwork` | yes* | Placement of the VM. Required when `provider` is `vcd`. |
| `vcd.catalog` / `vcd.catalogOrg` | no | Catalog holding the template (defaults to the controller org). |
| `vcd.dvmOffset` | no | Address offset of the deployment VM in Terraform's `ip_plan`; the controller reserves it so no worker gets that address. Only the vCD bootstrap has a DVM. |
| `ipPlan` | yes | Mirrors Terraform's `ip_plan`: `cidr`, `mNodeOffset`, `wNodeOffset`, `kubeapiLB`, `ingressLB`, `masterCount`, `reserved[]`. Workers are allocated from `wNodeOffset` upwards. |
| `join.endpoint` | no | Control-plane address, `host:port` of the API VIP. |
| `join.tokenTTL` | no | Lifetime of the per-VM bootstrap token (default `1h`). |
| `join.labels` / `join.taints` | no | Pool-wide, applied to every Ready worker (`taints` in `key[=value]:Effect` form). |
| `ingressPool` | no | `type` (`nsxv`/`nsxt`), `name`, `port`, `edge`. `port` is the **node port** the pool forwards to, not the virtual-server port; `edge` is the NSX-V edge that owns the pool. Workers are added on Ready and removed before a drain. |
| `scaleDown.minReplicas` | no | Floor for the workers that can serve traffic (default `1`). A removal that would leave fewer is refused. Writing off a `Missing` slot takes nothing out of service, so the floor never blocks that cleanup. `0` disables the floor. |
| `scaleDown.drainTimeout` | no | Budget for cordon+drain of one worker (default `10m`), and for the drain a rollout performs. When it expires the removal escalates past a blocking PodDisruptionBudget: the pods left on the node are deleted, the budget is named in `status`, and the removal continues with a fresh budget. |
| `scaleDown.waitForPDB` | no | Refuse to bypass a PodDisruptionBudget (default `false`). With `true` a drain that cannot finish within `drainTimeout` parks the worker `Failed` and keeps its VM for a human instead of escalating. A rollout drain honours the same switch: the roll pauses with a `RolloutBlocked` warning and the worker untouched. |
| `scaleDown.deleteWorkers[]` | no | Worker names to remove regardless of their position. Each entry is drained and deleted, then removed from this list and `spec.replicas` is decremented so the worker is **not** recreated. |
| `scaleDown.force` | no | Skip the node-local volume guard. Per-node override: annotation `k8s-nodescale.github.io/force-delete=true`. Never bypasses a PodDisruptionBudget on its own and never skips the detach wait. |
| `scaleDown.allowLastWorker` | no | Permit a request that leaves the pool with zero workers. Needs `scaleDown.minReplicas: 0`; a higher floor is rejected as `InvalidSpec` instead of accepting a flag that could never be honoured. Refused anyway on the worker the controller runs on and on node-local volumes. |
| `reconciliation.policy` | no | `Notify` (default) or `AutoHeal`. What happens when a worker's VM disappears outside the controller. |
| `reconciliation.autoHealRetries` | no | Recreations AutoHeal attempts for one slot before parking it as `Failed` (default `3`). |
| `reconciliation.autoHealBackoff` | no | Minimum time between two AutoHeal attempts for the same slot (default `2m`). |
| `rollout.strategy` | no | `Rolling` (default) applies `spec.flavor` automatically; `Manual` only to workers annotated `k8s-nodescale.github.io/resize=allow`. |
| `rollout.maxUnavailable` | no | How many workers may be out of service at once during a roll (int or percentage, default `1`). PDBs are the real brake. |
| `rollout.order` | no | `Descending` (default, highest ordinal first) or `Ascending`. |
| `rollout.pauseOnFailure` | no | Stop the roll when a worker does not come back (default `true`). |
| `rollout.readyTimeout` | no | How long a resized worker may take to become Ready (default `10m`). A recreated worker bootstraps from scratch and is bounded by `provisioning.timeout` instead. |
| `provisioning.timeout` | no | How long a VM may run without its Kubernetes Node before the worker is parked as `Failed` (default `15m`). The VM is kept for inspection and the pool reports `Ready=False`/`WorkerFailed`; a registration that arrives later still promotes the worker to `Ready`. |
| `capacity.preflight` | no | Read the vDC's free capacity before a create or a resize and park work that cannot fit (default `true`). See [When the vDC is full](#when-the-vdc-is-full). |
| `capacity.cpuMhzPerCore` | no | MHz per vCPU used for the CPU estimate when the vDC does not report its own speed to the controller account (default `2000`). The admin view of the vDC is read once; a tenant account that cannot read it falls back to this number. |

#### Platform providers

The controller talks to a platform through a narrow contract (`internal/cloud`):
`WorkerCompute` (list, read, create, delete, power, resize),
`CapacityReader`, and the optional `PoolMembership`. A `Factory` binds a
provider to one pool's `Placement`, so no call carries a vApp or network name
and the platform is a property of the pool, not of the process.

`spec.provider` picks the implementation. `vcd` (the default) is the
complete one: `internal/cloud/vcd` authenticates once and hands out a provider
per pool, bound to `spec.vcd`. `vsphere` is a stub in
`internal/cloud/vsphere`: it satisfies the same contract and refuses every
call with `cloud.ErrNotImplemented`, so a pool that names it is rejected as
`InvalidSpec` before anything is created.

vSphere has no load-balancer pool, which is why membership is a separate
optional interface rather than part of the main contract: a provider that
cannot edit a pool simply does not implement it, and the controller leaves
membership to the operator. The vCD provider implements it for NSX-V and
reports `ErrPoolNotConfigured` for a pool it cannot address (NSX-T today).

`status`:

| Field | Meaning |
| --- | --- |
| `observedReplicas` | Worker **slots** the controller knows about: VMs that exist, slots whose VM disappeared and stay reserved (`Missing` / parked `Failed`), and slots waiting for capacity (`Queued`). |
| `ledger.nextIndex` / `used[]` / `free[]` | Address book. `used` holds every allocated worker IP; addresses of deleted VMs are never released automatically. |
| `nodes[]` | Per worker: `name` (the vCD VM name), `nodeName` (the Kubernetes Node object name, lower case - the only name the controller passes to the Kubernetes API), `ip`, `providerID` (the VM id in the platform, today `urn:vcloud:vm:...`), `phase`, `cpu`, `memoryMB`, `diskGB`, `k8sReady`, `kubeletVersion`, `message`, `deleteStartedAt`, `flavor` (what the VM actually runs with, template included, read back from the platform), `healAttempts` / `healLastAttempt` (the AutoHeal budget), `resize` (the per-worker rollout step: `phase`, `recreate`, `drainStartedAt`, `message`), `provisioningStartedAt` (when the watchdog clock started for a VM that has no Node yet), `bootstrapTokenID` (the token kept alive until the join, then revoked; never the secret half). `message` is where a blocked removal explains itself (the PDB, the attached volume, the local PV). |
| `rollout` | `phase` (`Idle`, `Progressing`, `Paused`, `Queued`), `message` (why a roll is waiting, e.g. the capacity it needs), `targetFlavor`, `workersTotal`, `workersDone`, `currentWorker`, `lastError`, `startedAt`. |
| `conditions[]` | `Ready`, with reason `Converged` (`3/3 workers Ready`), `Progressing` while a worker is not Ready yet, `WorkerMissing` when a slot lost its VM, `WorkerFailed` when a worker is parked (drain timeout, exhausted AutoHeal, or no Node within `provisioning.timeout`), `LastWorkerProtected` / `WorkerHostsController` on a refused emptying, `RolloutInProgress` while a roll holds the pool, `NoResourceAvailable` when workers are queued for vDC capacity, plus `ScaleUpFailed`, `ScaleDownFailed`, `RolloutFailed` and `InvalidSpec` on errors. |

Phases: `Provisioning` (VM created, Node not registered yet) -> `Joining` (Node
registered, not Ready) -> `Ready`. Removal uses `Draining` (cordon, pool
removal, pod eviction) -> `Deleting` (detach wait, Node and VM deletion).
`Resizing` marks a worker inside a rollout. `Queued` means the slot is reserved
but the vDC has no room for the VM yet: no VM, no Node, no token, retried every
pass. `Missing` means the VM disappeared outside the controller and its slot is
reserved; `Failed` means the drain hit `drainTimeout` (VM kept), AutoHeal
exhausted its attempts, or no Node appeared within `provisioning.timeout` (VM
kept).

Example (placeholder org, network and template; nothing cluster-specific):

```yaml
apiVersion: k8s-nodescale.github.io/v1alpha1
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
  provider: vcd
  vcd:
    org: example-org
    vdc: example-vdc
    vapp: demo-cluster
    orgNetwork: demo-cluster-net
    catalog: Templates
    dvmOffset: -3
  ipPlan:
    cidr: 10.10.0.0/24
    mNodeOffset: 4
    wNodeOffset: 7
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
internal/cloud/          platform contract: WorkerCompute, CapacityReader,
                         optional PoolMembership, Factory + Placement
internal/cloud/vcd/      vCloud Director factory + per-pool provider
internal/cloud/vsphere/  vSphere stub (same contract, ErrNotImplemented)
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
helm repo add vcd-nodescale https://k8s-nodescale.github.io/vcd-k8s-node-controller
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

**Upgrading.** Helm applies `crds/` on install only: `helm upgrade` never touches a
CRD. An older CRD prunes the newer `status` fields and drops new subresources
(such as `scale`), so the CRDs of the *new* chart have to be applied before the
release is upgraded.

The Ansible playbook does that for you: the `k8s_addons_node_controller` role pulls
the pinned chart, applies every file from its `crds/` directory and installs the
release from that directory, on every run. A deploy through the playbook needs
no manual CRD step.

Upgrading by hand:

```
kubectl apply -f charts/vcd-nodescale/crds/k8s-nodescale.github.io_k8sworkerpools.yaml
helm upgrade vcd-nodescale charts/vcd-nodescale -n vcd-nodescale-system \
  --reuse-values --set image.tag=<version>
```

These are also the manual steps for an environment whose release was installed
by an older playbook, or by `helm install` straight from the repository: the
playbook does not migrate such a release, it only applies the CRDs of the
version it pins. Apply the new CRDs first, then upgrade the release, otherwise
the API server prunes the newer status fields and the new subresources are
missing. `kubectl scale` works only after the CRD that carries the subresource
is applied.

3. Apply a `K8sWorkerPool` (see `config/samples/` and the resource reference
   above), then use it as described in **How to use**.

## How to use

`spec.replicas` is the intent; the controller converges reality to it and every step
shows up in `status.nodes[].phase`. Both directions are supported:

| Goal | Request | Effect |
| --- | --- | --- |
| more workers | raise `spec.replicas` | creates the missing workers |
| fewer workers | lower `spec.replicas` | removes the highest-ordinal worker, never below `scaleDown.minReplicas` |
| one specific worker gone | `spec.scaleDown.deleteWorkers: ["<worker>"]` | removes that worker and decrements `replicas`; no replacement |
| one specific worker replaced | `kubectl annotate node <worker> k8s-nodescale.github.io/delete-worker=true` | removes it and creates a replacement (`replicas` untouched) |

The scale-out path is described next, the removal path under **Scale in /
removing a worker**. What the pool counts as its own is in **Ownership**, and
what happens when a worker disappears without a request is in **Drift, missing
workers and the reconciliation policy**.

### Scale out

Raise `spec.replicas` on the pool, one replica at a time:

```
kubectl -n default patch k8sworkerpool demo-cluster --type merge -p '{"spec":{"replicas":3}}'
```

`kubectl -n default edit k8sworkerpool demo-cluster` works as well, and so does
`kubectl -n default scale k8sworkerpool demo-cluster --replicas=3`: the CRD
exposes the `scale` subresource (`spec.replicas` / `status.observedReplicas`),
which also keeps `replicas` out of the alphabetically ordered YAML an editor
shows.

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

The token is a live credential with a ttl (`join.tokenTTL`, `1h` by default).
While the worker is `Provisioning` or `Joining` the controller renews it
whenever less than half the ttl remains, so a guest that is slow to reach
`kubeadm join` does not find it expired. Once the Node is `Ready`, the token and
its `cluster-info` JWS entry are deleted: only the id is kept, in
`status.nodes[].bootstrapTokenID`, and never the secret half.

A new worker gets `flavor.osDiskGB` on its system disk (bus 0/unit 0), applied
to the clone before the first power-on. The knob does not resize anything: a
worker that already exists keeps the disk it was created with. A template whose
system disk is already larger than `osDiskGB` is an error rather than a silent
shrink.

Budget about five minutes per worker: ~2 min vCD clone and power-on, ~1 min
first boot with the disk move, ~1 min guest bootstrap and join, the rest waiting
for kubelet to report Ready.

#### When the vDC is full

`capacity.preflight` (default on) reads the vDC's free compute and storage
capacity before a create or a resize. vCD rates a vCPU at the speed configured on
the vDC (its admin view carries `VCpuInMhz2`; the controller reads it once and
falls back to `capacity.cpuMhzPerCore` when the account cannot), memory in MB,
and disks against the storage profile the request names - the default profile for
the system disk and for a data disk without `storageProfile`. A category the
vDC's allocation model does not cap is reported with no limit and is skipped.

When a request does not fit, the controller does not attempt it: the slot is
parked as `Queued`, its name and address stay reserved, and the pool reports

    Ready=False  reason=NoResourceAvailable
      "2 worker(s) queued for vDC capacity: demo-cluster-wrk-04 (10.10.0.10), ...
       (waiting for vDC capacity: memory: need 8192 MB, 4096 MB free)"

`status.observedReplicas` counts the queued slots, so a scale-out is accepted and
visible instead of being retried as a failing create. Every pass retries the
queue: as soon as the vDC has room, the VM is created under the same name and
address. That create takes minutes, so the slot reports
`capacity is available; creating the VM` and keeps the `Queued` phase until the
VM exists: a restart in the middle of it retries the create (which converges)
instead of parking the slot as missing. A `Queued` slot costs nothing to
remove - a scale-in or `deleteWorkers` drops it and releases the address
(`WorkerDropped`), and it never counts as a serving worker for
`scaleDown.minReplicas`.

A capacity read that fails never blocks the pool: the controller logs it at
verbosity 1 and attempts the create, which is the behaviour without the
pre-flight. The check is an estimate in vCD's own units - vCD remains the
authority, and a create it still refuses is reported as before.

The system disk of a new worker is `spec.flavor.osDiskGB`. When that is unset the
clone keeps the template size, which the controller cannot read here, so it uses
an existing worker's `status.nodes[].flavor.osDiskGB` as the estimate (0 until
the pool has one).

#### When a worker never registers

A VM that boots but never registers a Node (a broken cloud-init, an apt mirror
that does not answer, a full data disk) would otherwise sit in `Provisioning`
forever. `spec.provisioning.timeout` bounds that window:

```
$ kubectl -n default get k8sworkerpool demo-cluster -o jsonpath='{range .status.nodes[*]}{.name} {.phase} {.message}{"\n"}{end}'
demo-cluster-wrk-03 Failed VM has run for 16m0s without a Kubernetes Node (spec.provisioning.timeout=15m0s); the VM is kept for inspection
```

The watchdog reports, it does not repair: it emits `WorkerProvisioningTimeout`,
parks the worker as `Failed` so the pool condition becomes
`Ready=False`/`WorkerFailed` instead of a permanent `Progressing`, and leaves
the VM running as evidence. If kubelet registers after all, the next pass
promotes the worker to `Ready` and clears the clock. Raise the timeout for a
slow template; do not remove the check.

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

Each worker is published in `status.nodes[]` before its VM exists (`phase:
Provisioning`, `message: creating the VM`) and again as soon as the VM id is
known, so a create that takes minutes is visible while it runs instead of
appearing only when the pass ends. The pass itself still creates one VM after
another, so asking for five replicas on an empty pool takes about five times as
long as one.

A create that a controller restart interrupted is finished on the next pass. The
interruption leaves the VM powered off with no Node behind it, and nothing else
would ever start it: the controller re-runs the (idempotent) configuration with
a fresh join command and powers it on, revoking the token of the interrupted
attempt. A VM that is already powered on had reached the last step of its create
and is left alone, and a VM the controller did not create itself (a Terraform
baseline worker) is never touched by this path.

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
| `kubectl annotate node demo-cluster-wrk-03 k8s-nodescale.github.io/delete-worker=true` | exactly that worker, with `replicas` untouched | yes, a replacement is created (rolling replacement) |

The annotation is the **rolling replacement** request: it marks one Node object
instead of naming a worker in the spec. Because `spec.replicas` is untouched,
the pool creates a replacement in a fresh slot once the old worker is gone. Use
it to replace a worker whose guest is in a bad state, or to move it to a new
address, without changing the size of the pool.

```bash
kubectl annotate node demo-cluster-wrk-03 k8s-nodescale.github.io/delete-worker=true
```

Only the value `true` (case-insensitive) counts; any other value is ignored. The
annotation lives on the Node object, so it disappears with it - there is nothing
to clean up afterwards. The pool only creates the replacement while
`spec.replicas` still asks for it: if something lowered `replicas` to 2 in the
meantime, the pool stays at 2.

The request can be withdrawn while nothing has started, by removing the
annotation:

```bash
kubectl annotate node demo-cluster-wrk-03 k8s-nodescale.github.io/delete-worker-
```

Once the worker is `Draining` in `status.nodes[].phase` (with
`deleteStartedAt` set), the removal is in flight and withdrawing the
annotation no longer stops it; only a policy change does (see **The last worker
is a deliberate choice**). A request that a guard refuses stays in place and is
re-evaluated every minute - remove the annotation to stop the refusals.

A removal is one step per reconcile pass, visible in `status.nodes[].phase`:

1. **Guards.** Refuse to go below `scaleDown.minReplicas`, which counts workers
   able to serve: `Missing` and `Failed` slots do not count, so a pool with
   one `Ready` and one `Missing` worker is a one-worker pool. Refuse when Bound
   `hostPath` PVs used by the node or PVs pinned to it by `nodeAffinity`
   (local volumes) would lose their data - the message names the PVs and the
   override (`scaleDown.force: true` or the node annotation
   `k8s-nodescale.github.io/force-delete=true`).
2. **Cordon + ingress pool.** Patch `spec.unschedulable`, then remove the
   address from `ingressPool` so no new request is sent to a dying node. A pool
   removal that cannot be performed (NSX-T, missing `edge`, pool not found)
   raises a `IngressPoolMemberNotRemoved` event instead of silently pretending.
3. **Drain through the eviction API.** DaemonSet and mirror pods are skipped;
   every other pod is evicted with the eviction subresource, so a
   PodDisruptionBudget is honoured instead of being bypassed. While a budget has
   no disruptions left the worker stays `Draining` and the message names the
   budget. The step is bounded by `scaleDown.drainTimeout`; when it expires the
   removal **escalates**: the pods still on the node are deleted through the
   API (not the eviction API), which is the only way past a budget that can
   never allow a disruption (`minAvailable` equal to the replica count, one
   replica with nowhere to move). The status message and a `DrainEscalated`
   warning list every budget that was bypassed, the budget clock restarts and
   the removal continues. `scaleDown.waitForPDB: true` turns the escalation
   off: the phase becomes `Failed` and the VM is kept.
4. **Volume gate.** Wait until no `VolumeAttachment` references the node.
   Destroying a VM with an attached volume strands the volume in the storage
   backend and can leave the PV bound forever.
5. **Power off the guest.** A hard power-off, not a guest shutdown: the pods
   are already gone and a wedged guest must not be able to block its own
   removal. This is what makes the next step stick.
6. **Delete the Node object.** Only now: a Node deleted while kubelet still
   runs is re-registered within seconds and stays behind as a `NotReady` corpse
   that nothing owns and whose pods are never garbage collected.
7. **Remove the VM and free the address.** The VM leaves the vApp (data disks
   included), the address goes back to `status.ledger.free` and the worker
   leaves `status.nodes`. Pods of the deleted Node are reaped by the pod
   garbage collector.

#### What can hold a removal, and what the controller does about it

A drain has to get past everything that can keep a pod on a node. None of it is
allowed to strand a worker the pool was told to remove:

| Blocker | Handling |
| --- | --- |
| `PodDisruptionBudget` with no disruptions allowed | Honoured for `scaleDown.drainTimeout`. When the budget is spent the pods left on the node are deleted through the API - the only way past `minAvailable` equal to the replica count - and the status message and the `DrainEscalated` event name the budget. A rollout drain spends the same budget and escalates the same way, or pauses under `scaleDown.waitForPDB`. |
| Pod without a controller (bare pod) | Evicted like any other: removing the node is the point, and there is nowhere else for it to go. |
| DaemonSet and mirror (static) pods | Skipped by design: they belong to the node, and deleting the Node object is what reaps them. |
| Pod that tolerates `node.kubernetes.io/unschedulable` | Left to go with the node (`tigera-operator` is the classic case). Evicting it is a loop: it is rescheduled onto the cordoned node it was evicted from. |
| `VolumeAttachment` still pointing at the node | The removal waits for it and never skips it; destroying the VM first strands the volume in the storage backend. |
| `hostPath` / `local` PV on the node | Refused with `VolumeDataAtRisk` unless `scaleDown.force` or the per-node `force-delete` annotation accepts the data loss. |
| Pod with a long `terminationGracePeriodSeconds` | The escalation deletes it with its own grace period; the next pass moves on once it is gone. |

`scaleDown.waitForPDB: true` replaces the escalation in the first row with a
refusal: the worker is parked `Failed`, the VM is kept and the request is
retried with a fresh budget. Use it where a budget must always win.

#### What "deleting a Kubernetes node" means here

`DELETE /api/v1/nodes/<name>` removes one registry record. It does not stop
kubelet and it does not touch the VM, which is why the order above matters
twice: powering the guest off first stops kubelet from re-registering the Node,
and deleting the Node object is what lets the pod garbage collector reap the
DaemonSet and mirror pods a drained worker leaves behind - while the Node object
exists, they stay in the API forever, `Running` on a node that is not there.

The controller deletes a Node object only for a worker it is removing itself (or,
under `AutoHeal`, for the worker it is repairing). Masters, the DVM and VMs
outside `<namePrefix>-wrk-` are never touched: they are not part of the pool.

Notes:

- `PVs` are not deleted. A `Retain` PV survives its PVC and must be cleaned up
  by hand; a `Delete` PV is reclaimed only when its PVC is deleted.
- Pods using `emptyDir` are evicted and their data goes with them - that is the
  point of deleting a node.
- A `Failed` removal keeps retrying while the request stands: one pass reports
  `Failed` with the reason (a budget when `waitForPDB: true`, an attached
  volume, a refusal), the next one starts a fresh `drainTimeout` budget and goes
  back to `Draining`. A count-based scale-down refuses to start while any worker
  is `Failed`, so one stuck removal never cascades into a second one. Prune the
  request (`deleteWorkers` entry or the node annotation) to stop it; a request
  that is withdrawn is undone instead: the worker is uncordoned and serves again.
- Nothing is removed from the vApp except the VM: its disks are part of the VM,
  the data-disk VMDKs included.
- The removal is resumable. Every step is persisted in `status` before it is
  attempted, so a controller restart - or the controller's own pod being evicted
  by the drain it started - resumes where the previous pass stopped instead of
  starting over.

> **0.3.1 and earlier did not do this.** The removal path passed the vCD VM name
> (`KION-WEB-DEV-wrk-03`) to the Kubernetes API instead of the Node object name
> (`kion-web-dev-wrk-03`). `Cordon` and `DeleteNode` swallowed the 404 and `Drain`
> selected no pods, so a worker was removed with its guest still running: no
> cordon, no drain, no PodDisruptionBudget, no Node deletion, and a `NotReady`
> Node object left in the cluster forever. From 0.3.2 the Node name is kept in
> `status.nodes[].nodeName` and every Kubernetes call uses it, and the guest is
> powered off before the Node object is deleted.

### Rolling compute-spec changes

`spec.flavor` is what a worker should be; `status.nodes[].flavor` is what its VM
actually runs with, read back from vCD on every pass. A difference between them
is a roll-out candidate, so the loop also notices a VM somebody resized by hand.

    kubectl -n vcd-nodescale-system patch k8sworkerpool default --type merge -p '{"spec":{"flavor":{"cpus":8,"memoryMB":16384}}}'
    kubectl -n vcd-nodescale-system get k8sworkerpool default -o custom-columns=PHASE:.status.rollout.phase,DONE:.status.rollout.workersDone,TOTAL:.status.rollout.workersTotal,NOW:.status.rollout.currentWorker

Each worker goes through the removal pipeline with a resize in the middle
instead of a delete:

    cordon (nodeName) -> ingress pool out -> drain (PDB) -> VolumeAttachment wait
      -> read the VM -> power off -> grow CPU/memory/system disk -> verify the shape
      -> power on -> wait Node Ready and the flavor to match -> uncordon
      -> ingress pool in

A change that cannot be applied in place takes the recreate branch after the
drain instead of powering off:

    cordon (nodeName) -> ingress pool out -> drain (PDB) -> VolumeAttachment wait
      -> power off -> delete the Node object -> delete the VM
      -> create the VM again in the same slot (new shape, new image)
      -> wait Node Ready and the flavor to match -> uncordon -> ingress pool in

The VM is read before anything is powered off, so a step that a status conflict
re-drove does not power-cycle a worker whose shape already matches, and a VM that
something started again is stopped rather than failing the slot. CPU, memory and
the system disk change in that one power-off window, and the worker is only
powered on again once vCD reports the target shape: a resize that only half
applied parks the slot `Failed` with the VM stopped, instead of booting a worker
that reports Ready with the wrong disk.

One worker at a time by default. `rollout.maxUnavailable` (int or percentage)
lets a batch be out of service together; PDBs remain the real brake, and a batch
whose drain cannot finish keeps retrying with the reason in
`status.nodes[].resize.message`. `rollout.order: Ascending` rolls `wrk-01`
first instead of last.

The drain in that pipeline spends the same `scaleDown.drainTimeout` budget a
removal does, counted from `status.nodes[].resize.drainStartedAt`. When it is
spent the roll escalates exactly like a scale-down: the pods left on the worker
are deleted through the API, the status message and a `DrainEscalated` warning
name the budgets that were bypassed, and the worker keeps draining with a fresh
budget before it is powered off. `scaleDown.waitForPDB: true` is the opt-out:
the roll pauses instead - `status.rollout.phase: Paused`, the worker's
`resize.phase: Failed` with the reason, and a `RolloutBlocked` warning - and
nothing on the worker is touched, so the operator decides when it may go.

A roll checks capacity too, before it takes anyone out of service: the delta the
resize adds (CPU, memory, system disk) has to fit, not the whole target flavor.
When it does not, `status.rollout.phase` is `Queued` with the shortfall in
`status.rollout.message`, the condition is `Ready=False`/`NoResourceAvailable`,
and no worker is cordoned; workers already in flight finish their step. See
[When the vDC is full](#when-the-vdc-is-full).

Every step is persisted before it is attempted, so a controller restart in the
middle of a roll resumes at the same worker instead of starting over - the same
mechanism that makes a named removal resumable.

While a roll runs, scale-in and `deleteWorkers` are refused with
`RolloutInProgress`; scale-up is allowed and only adds capacity. A worker that
does not come back within `rollout.readyTimeout` (default `10m`) is parked as
`Failed` with its VM intact, and with `rollout.pauseOnFailure: true` (the
default) the whole roll pauses there: `status.rollout.phase: Paused` and
`lastError` name the worker. Resolve it (usually `deleteWorkers`, then let the
pool create a fresh worker at the current spec) and the roll resumes.

vCD cannot shrink a VM - CPU and memory have no smaller setting, and a smaller
disk would destroy data - and another `spec.flavor.template` is another operating
system. Both are a **recreate**, not a resize: the worker goes through the same
cordon, pool-out and drain, and then its VM is deleted and created again in the
same slot with the new shape and image, under the same name and address, so the
ledger and `status.nodes` do not move. `status.nodes[].resize.recreate` is `true`
for the whole operation, the phases are `Cordoned`, `Deleting`, `Creating` and
`Booting`, and a `WorkerRecreate` event names the reason before anything is
drained.

A recreate is a fresh provision, so it is bounded by `spec.provisioning.timeout`
rather than `rollout.readyTimeout`, the join token is renewed while the guest is
still installing, and it consumes no free vDC capacity: the old VM is deleted
before the new one is created. Data on the worker's disks does not survive it -
that is exactly what makes a shrink possible.

A worker the controller adopted carries no record of the image it was cloned
from, so `status.nodes[].flavor.template` stays empty, and an unknown image is no
opinion: changing `spec.flavor.template` does not replace an adopted worker.
Replace it once (`deleteWorkers`) and the worker that replaces it carries the
record.

`strategy: Manual` disables automatic rolls: only workers annotated
`k8s-nodescale.github.io/resize=allow` are resized, which suits environments where
the maintenance window matters more than the API.

## Ownership

The pool owns every VM in its placement's vApp whose name starts with
`<spec.namePrefix>-wrk-`, together with the Node object of each of those
workers. That is a name-prefix rule, not a metadata rule: a VM created by
Terraform during the initial bootstrap carries no controller metadata and is
adopted on the first pass exactly like one the controller created itself.
Adoption puts the worker into `status.nodes`, reserves its address in
`status.ledger` and makes it a candidate for every deletion path.

Consequences worth knowing:

- Everything in `status.nodes` is deletable by the pool. There is no read-only
  class of worker, and `DeleteVM` has no ownership check.
- A VM created outside the pool that happens to match the prefix (a manual
  clone, a `terraform apply` that re-creates a removed worker) is adopted on the
  next pass and counted towards `spec.replicas`.
- Adoption is visible: a VM that enters `status.nodes` for the first time
  raises a `WorkerAdopted` event, and a worker adopted in the current pass is
  skipped by the scale-down of that pass, so an outside `apply` cannot turn into
  "the controller deleted a different worker".
- Because ownership is by prefix, the Terraform workers are the pool's workers
  too; see the Terraform integration contract below for how to remove one.

## Drift, missing workers and the reconciliation policy

0.3.x behaviour, for the record: a worker that disappeared without a request
was replaced immediately and silently. `status.observedReplicas` simply drops, the pool sees
`spec.replicas > observed` and provisions a new worker - under a new address and
therefore a new name, because the vanished worker's address stays reserved in
`status.ledger.used`. A `deleteWorkers` entry that names an already-absent
worker is pruned without touching `replicas`, which then triggers that
replacement. Nothing reports the hole: the pool just converges to a smaller,
differently named cluster.

The behaviour is decided by `spec.reconciliation.policy`.

| Value | Meaning |
| --- | --- |
| `Notify` (default) | never create a VM for a worker that disappeared outside the loop; report it |
| `AutoHeal` | recreate the disappeared worker in its own slot: same name, same address |

`Notify` is the default because creating a VM is the expensive, state-creating
decision - healing has to be opted into. Both values act only on slots the pool
owns (see **Ownership**); neither touches masters, the DVM or VMs outside the
prefix, and a worker with a pending removal is never healed.

The unit of reconciliation is the **slot**: one address in `status.ledger.used`
plus the worker name derived from its ordinal. A slot whose VM is gone keeps its
address reserved and is reported in `status.nodes` with phase `Missing`, so
`status` shows a hole (`Ready=False`, reason `WorkerMissing`, message naming the
worker and its IP) instead of quietly shrinking to the new reality.

| Situation | `Notify` | `AutoHeal` |
| --- | --- | --- |
| VM gone, Node object gone or stale | report, keep the slot reserved, create nothing | recreate the worker in its slot; the new kubelet re-registers the same Node object |
| Node object gone, VM alive | report; the message carries the manual command | reboot the guest once and let kubelet re-register |
| `deleteWorkers` names a worker that is already gone | prune the entry and set `spec.replicas` to the observed count, so the CR is lined up with reality and nothing is created | same: an explicit removal always wins over healing |
| `spec.replicas` lowered while a slot is already missing | write the missing slot off, no running worker is deleted | same |

The reserved address belongs to that slot: writing the slot off (a scale-in, or
a `deleteWorkers` entry naming it) frees the address, and an `AutoHeal` refills
it with the same name. A plain scale-out never steals a reserved address, so a
missing worker cannot be silently replaced by a differently named successor.
`AutoHeal` is bounded - a fixed back-off (`reconciliation.autoHealBackoff`,
default `2m`) between attempts and a retry limit
(`reconciliation.autoHealRetries`, default `3`), after which the slot is parked
as `Failed` with the reason and nothing else is retried - so a broken template
cannot spin.

### Controller placement, the last worker, and scale from zero

The controller has to outlive the pool for two operations: removing the last
worker and going from zero workers back to N. Both need a pod that does not run
on a pool worker, because a pass that deletes its own last host cannot finish:
the process dies with the VM, and nothing is left to write `status` or to create
the next worker.

**Recommended: run the controller on the control plane.** It asks for 100m CPU
and 512Mi; the masters in this platform have 2 vCPU and ~3.8GiB allocatable, so
the footprint is negligible next to etcd, apiserver and CoreDNS. Masters are
never pool members - the pool only owns `<namePrefix>-wrk-*` - so they are not
deletable through the CRD and the controller cannot delete its own host.

```yaml
# values-control-plane.yaml
nodeSelector:
  node-role.kubernetes.io/control-plane: ""
tolerations:
  - key: node-role.kubernetes.io/control-plane
    effect: NoSchedule
replicaCount: 2
affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            app.kubernetes.io/name: vcd-nodescale
        topologyKey: kubernetes.io/hostname
```

```
helm upgrade vcd-nodescale vcd-nodescale -n vcd-nodescale-system \
  --repo https://k8s-nodescale.github.io/vcd-k8s-node-controller \
  -f values-control-plane.yaml
```

- Two replicas with `leaderElection: true` (the chart default) make a master
  rebuild or a restart a non-event; the required anti-affinity keeps them on
  different masters, and an unsatisfiable anti-affinity (single master) leaves
  one replica `Pending`, which is the right failure mode.
- With the controller off the pool, `scaleDown.minReplicas: 0` is supported and
  `spec.replicas` may go to 0 and back up. The pool at 0 reports `0/0 workers
  Ready` and `Converged`.
- Emptying the pool is possible, but it is a deliberate owner decision, not
  something the controller does because a number in the spec says so - see
  [The last worker is a deliberate choice](#the-last-worker-is-a-deliberate-choice).
- While the pool is at zero the cluster has no workers, so ingress, workload
  pods and the CSI node plugins are gone by definition. The control plane,
  CoreDNS, the controller and the vCD API remain, which is exactly what is
  needed to bring workers back.
- Check that CoreDNS can run on a master before emptying the pool: the
  controller resolves the vCD API through cluster DNS. kubeadm's CoreDNS
  tolerates the control-plane taint and normally lands there; if it does not,
  set `dnsPolicy: Default` on the controller (chart value from 0.4.0).

Caveats of the control-plane placement: the pod holds vCD credentials with
VM-delete rights, and now runs next to etcd - keep the vCD account scoped to the
cluster's VDC and vApp, and do not run the controller with an admin account. A
master that is destroyed takes the pod with it, though the CR `status` is the
source of truth and the remaining replica resumes immediately.

#### The last worker is a deliberate choice

Emptying the pool is never an accident and never a side effect of a count: it
needs `scaleDown.minReplicas: 0` and, from 0.4.0,
`scaleDown.allowLastWorker: true`. Without the flag a request that would take
the pool to zero is refused with `LastWorkerProtected` and the worker is left
alone. **Deleting the last worker is a critical event for everything that runs on
the cluster** - set the flag only when you mean it, and read the following first:

- Every pod on that worker is evicted: ingress-nginx, any CoreDNS replica that
  landed there, metrics-server, the CSI node plugins, the calico-node DaemonSet.
  A PodDisruptionBudget whose `minAvailable` equals its current replica count
  (the classic `minAvailable: 1` on a single-replica workload) is honoured
  until `drainTimeout` and then bypassed, with the budget named in `status`
  and in a `DrainEscalated` warning: on the last worker the workload has
  nowhere else to go, so waiting can never end differently. The pod is deleted
  and comes back when the pool does. Set `scaleDown.waitForPDB: true` to stop
  at `Failed` with the VM kept instead.
- What remains is the control plane, CoreDNS when it can run on a master, the
  controller and the vCD API. Nothing else can be scheduled, so every workload
  with a PDB, a `hostPort` or a `local`/`hostPath` PV is down until the pool is
  refilled. On a cluster with one worker this is a full outage of the payload, by
  design, on request.
- Scaling back up from 0 is a cold start: new VMs, cloud-init, kubelet join,
  calico and CSI - minutes per worker, not seconds. Do not empty the pool
  "overnight to save resources" and expect a fast morning.

Two cases are refused regardless of the flag, because they are physics rather
than policy: the node the controller itself runs on (a pass that deletes its own
host cannot write `status` or create the replacement - move the controller off
the pool instead), and a worker whose `hostPath`/`local` PVs would be destroyed
(`scaleDown.force` overrides only the PV check; a PDB is bypassed only by
the escalation after `drainTimeout`).

The floor and the flag are evaluated against what the pool can actually serve:

- `minReplicas` counts workers able to serve, not rows in `status`. A `Missing`
  slot (its VM is gone) and a `Failed` slot (a drain timed out) are not
  capacity, so a pool with one `Ready` and one `Missing` worker is treated as a
  one-worker pool.
- Writing off a `Missing` slot is the one removal that takes nothing out of
  service, so the floor never blocks it. It still may not empty the pool: the
  write-off of the last slot needs `allowLastWorker: true` like any other
  removal.
- `minReplicas` defaults to `1` in the CRD. A manifest that omits it is still
  protected; emptying the pool always needs `minReplicas: 0` written out.
- Setting `allowLastWorker: true` with a higher floor is contradictory and the
  pool reports `InvalidSpec` rather than quietly refusing every removal.
- If the policy changes while a drain is already under way, the removal is
  refused and the cordon is reverted: the worker is uncordoned, clears
  `deleteStartedAt` and goes back to `Ready` instead of staying `Draining`
  under a refusal. Once the phase is `Deleting` the VM is already being
  stopped, so the removal finishes rather than leaving a half-deleted worker.

| Situation | Result |
| --- | --- |
| `replicas: 3`, `minReplicas: 1`, remove one | allowed, 2 serving workers remain |
| 1 `Ready` + 1 `Missing`, `minReplicas: 1`, remove the `Ready` one | refused (`MinReplicasProtected`) |
| 1 `Missing` slot, `minReplicas: 1`, `deleteWorkers: [<that slot>]` | refused (`LastWorkerProtected`), address stays reserved |
| same, with `minReplicas: 0` + `allowLastWorker: true` | written off, pool becomes empty |
| `allowLastWorker: true`, `minReplicas: 1` | `InvalidSpec`, nothing reconciled |

### Deleting the pool object

`kubectl delete k8sworkerpool <name>` removes the pool **and its workers**. The
object carries the finalizer `k8s-nodescale.github.io/workerpool-cleanup`, so it
does not disappear until every worker has been drained, removed from the ingress
pool, stopped, deleted in vCD and had its address released. Only then is the
finalizer dropped and the CR garbage-collected:

    kubectl -n default delete k8sworkerpool kion-web-dev
    kubectl -n default get k8sworkerpool kion-web-dev -w   # Terminating until done

What this means in practice:

- **It is not a scale-down.** `minReplicas` and `allowLastWorker` do not apply:
  the owner asked for the whole pool, so every worker goes, including the last
  one. `status` reports `PoolDeleting` with the number of workers left.
- The two physics refusals still hold: a worker the controller itself runs on
  (`WorkerHostsController`) and a worker whose `hostPath`/`local` volumes would
  be lost (`VolumeDataAtRisk`). The object stays `Terminating` with the reason in
  `status.conditions` until the obstacle is removed - move the controller off
  the pool, or set `scaleDown.force` / annotate the node with
  `k8s-nodescale.github.io/force-delete=true` if the data really is disposable.
- One worker is advanced per pass, like a scale-down, so a drain that waits for
  a PDB does not block the work queue. The same `drainTimeout` budget and the
  same post-budget escalation apply; `waitForPDB` is ignored here, because a
  budget that never allows a disruption would otherwise pin the finalizer
  forever.
- Deleting the namespace or the vApp does not bypass this: the CR stays until
  the workers are gone. A pool created before the finalizer existed picks it up
  on the first reconcile, so an upgrade needs no migration.
- Do not strip the finalizer by hand (`kubectl patch ... -p
  '{"metadata":{"finalizers":null}}'`) unless the workers are already gone:
  that orphans the VMs, the Node objects and the ingress pool members with
  nothing left to own them.

## Management UI

The chart can run an optional second container in the same Pod, `nodescale-ui`.
It serves one page and a small JSON API over the same `K8sWorkerPool` objects.
It never talks to vCD: every action it offers is an edit to the CRD, so the
controller decides what happens exactly as it would for `kubectl`.

```yaml
ui:
  enabled: true
```

With `ui.enabled: true` the chart also creates a Service and an Ingress, both
named `nodescale-ui`, and the page answers under `/nodescale-ui`:

```
http://<any-node-ip>:<ingress-http-node-port>/nodescale-ui/
https://<any-node-ip>:<ingress-https-node-port>/nodescale-ui/
```

The ingress has no `host` by default (`ui.ingress.host`), so it answers on any
name; set one where a real hostname exists.

HTTPS works on the ingress controller's HTTPS port with its own self-signed
certificate, and the page is also served over plain HTTP. No redirect is set:
behind NodePorts a redirect drops the port and points at `:443`, which is not
open, and a certificate is matched by name, which a catch-all rule does not
have. With a real hostname, set `ui.ingress.tls.secretName` to a TLS Secret and
`ui.ingress.tls.redirect: true` to move everyone onto HTTPS.

### What the page does

| Action | What it changes |
| --- | --- |
| scale-out `+1`, scale-in `-1`, set replicas | `spec.replicas` |
| apply flavor | `spec.flavor.cpus`, `.memoryMB`, `.osDiskGB` (only the fields you fill in) |
| delete a worker | appends the name to `spec.scaleDown.deleteWorkers` |

Every gate still applies: `scaleDown.minReplicas`, `allowLastWorker`, the drain,
the last-worker guard and the node-local-volume refusal. A request the pool
refuses shows up as the pool's `Ready` condition message, which the page
displays under the summary.

The table is one row per slot: CPU and memory, each in three columns - requests,
limits and real utilisation. Each column is coloured on its own: green below
80%, orange from 80% to 90%, red at 90% and above. A node that is green on
requests and red on real utilisation is exactly the case worth looking at.

Requests and limits come from the pods on the node against its allocatable
capacity. Real utilisation is scraped from node-exporter pods
(`ui.nodeExporter`, default label
`app.kubernetes.io/name=prometheus-node-exporter`, port 9100); when those are
absent the sidecar falls back to `metrics.k8s.io`, and when neither answers the
column shows a dash instead of a number.

### Authentication

Authentication is on by default. Two groups live in one Secret
(`ui.auth.secretName`, default `nodescale-ui-auth`, key `users.yaml`):

```yaml
admins:
  - username: admin
    passwordHash: "$2a$10$..."
viewers:
  - username: viewer
    passwordHash: "$2a$10$..."
```

- `admins` may read and change, `viewers` may only read. The page hides the
  controls for a viewer and the server refuses the request with `403`, so the
  hiding is cosmetic and the refusal is the control.
- A name listed in both groups is an admin.
- Passwords are bcrypt hashes: `htpasswd -nB <user>` and copy the hash. The
  Secret is re-read every `ui.auth.refreshInterval` (60s), so a rotation needs
  no restart.
- A missing or unparsable Secret closes the page: everything except
  `/nodescale-ui/healthz` answers `503` instead of opening.

The chart creates the Secret on install when it does not exist yet
(`ui.auth.createSecret`), with the accounts `admin` (admins) and `viewer`
(viewers) and 24-character generated passwords. The plaintext is written once,
next to the hashes, under the key `initial-passwords`; `helm install` prints the
same command in its notes:

```
kubectl -n vcd-nodescale-system get secret nodescale-ui-auth \
  -o jsonpath='{.data.initial-passwords}' | base64 -d
```

`users.yaml` holds only bcrypt hashes, so a password that was never read from
that key cannot be recovered, only reset (see above). Once both are stored
somewhere safe, remove the plaintext so it stops existing:

```
kubectl -n vcd-nodescale-system patch secret nodescale-ui-auth \
  --type=json -p '[{"op":"remove","path":"/data/initial-passwords"}]'
```

`helm upgrade` never rotates the accounts and `helm uninstall` keeps the Secret
(`helm.sh/resource-policy: keep`). Where Ansible installs the chart it generates
the passwords itself, keeps them on the control host and creates the Secret
once, in which case the values set `ui.auth.createSecret: false`. They are then
in `<playbook_dir>/.credentials/<inventory_hostname>/nodescale-ui-admin` and
`nodescale-ui-viewer` (mode `0600`), not in the cluster at all.

Signing in happens on a form at `/nodescale-ui/login`, not in the browser's
Basic dialog: the page sets a signed cookie (`HttpOnly`, `SameSite=Strict`,
`Secure` when the request is HTTPS, lifetime `ui.auth.sessionTTL`, 12h by
default) and `sign out` in the header clears it. An unauthenticated visitor is
redirected to the form; only the API answers `401`.

The cookie is signed with a key derived from `users.yaml`, so any change to the
accounts - a rotated password, an added or removed user - ends every session at
once, without a restart and without waiting for the cookie to expire.

HTTP Basic is still accepted, so a script or `curl` does not have to keep a
cookie. Writes additionally require the header `X-Requested-With:
nodescale-ui`, which a cross-site form cannot set:

```
curl -u admin:'<password>' -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Requested-With: nodescale-ui' \
  -d '{"replicas":3}' \
  http://<node>:<port>/nodescale-ui/api/pools/default/kion-web-dev/replicas
```

Every accepted change is logged with the account and recorded on the pool:

```
kubectl -n default get k8sworkerpool kion-web-dev \
  -o jsonpath='{.metadata.annotations.k8s-nodescale\.github\.io/last-action-by}'
```

### Turning it off

`ui.enabled: false` is the default and deploys nothing: no container, no
Service, no Ingress, no Secret.

## Terraform integration contract

- The Terraform module installs this chart behind a `node_controller.enabled`
  toggle and pins `chart_version` / `image_tag` (currently `0.11.9`).
- **Once the controller is enabled, do not use Terraform to manage worker count
  or worker LB pool membership.** Use the CRD instead. The Terraform module's
  README must state this explicitly.
- Initial cluster bootstrap remains Terraform + Ansible/DVM; the controller only
  handles post-bootstrap scaling.
- The workers Terraform creates are adopted by the pool like any other
  (`<namePrefix>-wrk-*`), which makes them deletable by the CRD. Do not remove a
  bootstrap worker with `spec.scaleDown.deleteWorkers`: Terraform still owns
  that VM in state and will re-create it at the same address, while the pool has
  freed the address and may hand it to a different worker. Remove bootstrap
  workers through Terraform (`workers.vm_count`) and lower `spec.replicas` in the
  same change so the pool does not scale back up.
- `spec.reconciliation.policy` defaults to `Notify` from 0.4.0: a worker that
  disappears without a request is reported, not replaced. `AutoHeal` is an
  explicit opt-in.

## Design

See [`docs/conception.md`](docs/conception.md) for the full decision record,
architecture, algorithms and open questions.

## Container image

The image is published to Docker Hub on a `v*` tag:
`docker.io/werton13/vcd-k8s-node-controller` (`.github/workflows/docker-publish.yaml`).

Build locally (the Dockerfile uses `golang:1.27.1-alpine` and
`gcr.io/distroless/static:nonroot`):

```
docker build -t docker.io/werton13/vcd-k8s-node-controller:0.11.9 .
```

Public module defaults are used inside the build. On a restricted network,
override them:

```
docker build \
  --build-arg GOPROXY=https://nexus.example.com/repository/golang/ \
  --build-arg GOSUMDB=off \
  -t docker.io/werton13/vcd-k8s-node-controller:0.11.9 .
```

Push (or use `make image-push`):

```
docker login docker.io
docker push docker.io/werton13/vcd-k8s-node-controller:0.11.9
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
   https://k8s-nodescale.github.io/vcd-k8s-node-controller/`, commit and push. The
   Pages deploy job can sit `queued` for 10+ minutes; verify
   `https://k8s-nodescale.github.io/vcd-k8s-node-controller/vcd-nodescale-<tag>.tgz`
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
