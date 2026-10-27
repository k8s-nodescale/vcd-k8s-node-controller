# Conception: Go-based Node Scaling Controller for the vCloud Kubernetes Module

> **Document control**
> - **Type:** Working conception / design & action plan (not implementation).
> - **Location:** Local working file — excluded from git via `.git/info/exclude`.
> - **NOT to be pushed.** Do not `git add` / commit / sync this file.
> - **Baseline analysed:** `dev-r3` (most recent code).
> - **Target development branch:** `dev`.
> - **Controller code repository:** **separate git repo** (see §3.7). This Terraform repo will
>   only *consume* a pinned release of it.
> - **Status:** Draft — decisions locked in §3, open items in §8.

---

## 1. Executive summary

The current module deploys a Kubernetes cluster on VMware Cloud Director (vCD) exclusively
through **Terraform** (compute + network + VM bootstrap) and then **Ansible** (run from the DVM)
for OS preparation and cluster join. There is **no supported path to add/remove worker nodes**
after the initial deployment — `add_workers.yaml` in the Ansible repo is manual, and
`add_hosts_director.sh` is a brittle port-scanning shell helper.

The proposed solution is a **Go controller (Kubernetes operator)**, delivered from a **separate git
repository**, that:

1. Observes worker-node count, compute and disk characteristics and stores them in a **CRD status**.
2. Reconciles a **desired replica count** (config change) against the actual cluster and converges
   (scale-out / scale-in).
3. Creates/configures the required vCD resources **directly via the vCD API** (`govcd`), with **no
   Terraform and no Ansible** in the scaling path.
4. Injects OS preparation **and** `kubeadm join` into a **generated cloud-init** at VM creation
   time, using a **short-lived bootstrap token created on demand via the Kubernetes API**.

All four requirements are feasible. The main engineering risks are the **Terraform ↔ controller
ownership boundary**, the **disk / `/var` relocation strategy**, and **LB pool membership timing**.
This document captures the agreed decisions for each.

---

## 2. Current system — facts that shape the design

### 2.1 Terraform (`dev-r3`)
- One shared `vcd_vapp` (`k8s_vapp`) + bridged Org-network attachment (required by the vCloud CSI
  driver). No vApp network is created.
- Master VMs (`vcd_vapp_vm.k8s_masters_vms`) and Worker VMs (`vcd_vapp_vm.k8s_workers_vms`) from the
  same catalog template: **MANUAL** IP allocation, `disk.EnableUUID=TRUE`, 0–2 internal data disks.
- Per-VM bootstrap uses **both**:
  - `customization.initscript` → `templates/customization_node.sh` (writes netplan, builds a
    **NoCloud seed** from the base64-gzip user-data, runs `cloud-init init/modules`), and
  - `guest_properties` → `guestinfo.userdata` (+ metadata, base64).
- DVM (`vcd_vm.dvm`) cloud-init clones the Ansible repo and runs `main.yaml`.
- NSX-V **or** NSX-T load-balancer pools/virtual servers, with `dynamic "member"` blocks built from
  `local.master_nodes` / `local.worker_nodes`, plus SNAT/DNAT.
- **IP plan is index-derived** (`locals.tf`):
  - worker *i* → `cidrhost(vm_ip_cidr, i + ip_plan.w_node)`, name `...-wrk-NN`
  - master *i* → `cidrhost(vm_ip_cidr, i + ip_plan.m_node)`
  - DVM → `cidrhost(vm_ip_cidr, ip_plan.dvm)` (`-3` = third from end)
  - reserved: `kubeapi_lb`, `ingress_lb`, gateway (`.1`)

### 2.2 Ansible bootstrap (referenced repo `k8s-kubeadm-r2`, branch `dev-r3`)
- `prepare_k8s_nodes`: reboot → LVM on `sdb`/`sdc` → mount `/data1`,`/data2` → **move `/var` →
  `/data1`, `/var/lib` → `/data2`** → swapoff, kernel modules, sysctl, containerd
  (`SystemdCgroup=true`), disable unattended upgrades.
- `install_k8s_binaries`: `pkgs.k8s.io` repo; install + `hold` pinned `kubeadm/kubelet/kubectl`.
- `bootstrap_k8s_cluster` / `k8s_add_workers`: `kubeadm token create --print-join-command` on
  `masters[0]`, then `kubeadm join` on each worker; poll `get nodes` until `Ready`.
- `add_workers.yaml` exists but is **manual**; `add_hosts_director.sh` scans ports after the last
  worker to guess new IPs.

The controller will **re-express these steps as generated cloud-init + vCD API calls**, not invoke
Ansible.

---

## 3. Decisions locked (from review discussion)

### 3.1 Token & join strategy — **DECIDED: on-demand short-lived bootstrap token**
- The in-cluster controller creates a **bootstrap token on demand via the Kubernetes API**
  (Secret type `bootstrap.kubernetes.io/token`) at VM-create time, with a short TTL (e.g. `1h`).
- `--discovery-token-ca-cert-hash` is computed from `kube-public/cluster-info` (or
  `kube-root-ca.crt`).
- Workers need **no `--certificate-key`** (control-plane only).
- Rationale: no permanent token baked into templates, no `admin.conf` copying, least exposure.

### 3.2 Disk / `/var` relocation — **DECIDED: Option A (parity with current Ansible path)**
- cloud-init performs LVM on `sdb`/`sdc`, mounts `/data1`/`/data2`, moves `/var` → `/data1` and
  `/var/lib` → `/data2`, then reboots once (one-shot marker file guard via `bootcmd`/`power_state`).
- **Details to be discussed later** (bootcmd guard, idempotency, reboot detection, failure
  recovery). Do not finalize implementation until those are agreed.

### 3.3 IP allocation ledger — **AGREED**
- The controller maintains a persistent **IP ledger** (`status` of the CRD and/or a dedicated
  ConfigMap) with `nextIndex`, `used[]`, `free[]`.
- The ledger **must be aware of the Terraform `ip_plan` variable** and its semantics:
  - `m_node`, `w_node` — start offsets for master/worker allocation;
  - `dvm` — negative offset from subnet end;
  - `kubeapi_lb`, `ingress_lb` — reserved VIPs;
  - gateway `.1` — reserved.
- The CRD should carry the effective IP plan (cidr + offsets + reserved list) so the controller
  never collides with Terraform-managed infrastructure or reserved VIPs.
- Freed IPs may be reused only after confirming no VM in the vApp still uses them.

### 3.4 LB pool membership — **AGREED: add only after node is Ready**
- After a new worker VM is created and the Kubernetes `Node` reports **`Ready`** (and is able to
  accept traffic), the controller adds its IP to the **ingress LB pool**:
  - NSX-V: `vcd_lb_server_pool` member update (Edge API);
  - NSX-T: `vcd_nsxt_alb_pool` member update.
- On scale-in: remove from the pool **before** draining/deleting.
- This ordering prevents routing traffic to a node that is not yet serving.

### 3.5 Terraform ↔ controller ownership — **DECIDED: controller takes over after bootstrap**
- Today Terraform owns worker count **and** worker LB pool members
  (`dynamic "member" for_each = local.worker_nodes`). If the controller adds nodes/members, a later
  `terraform apply` would revert them.
- **Decision:** once the node controller is enabled, **Terraform must NOT be used to manage the
  cluster further** (scale-out/scale-in, node configuration, LB membership). Terraform is limited to
  initial bootstrap.
- **Required documentation:** the `README.md` must clearly warn:
  > If the node-scaling controller is enabled, do **not** use Terraform to manage the cluster
  > afterwards (no further `terraform apply` for node/scale changes). Use the controller/CRD instead.
- **Required variable:** add a toggle so the controller can be enabled/disabled while filling the
  initial `tfvars`, e.g.:
  ```hcl
  node_controller = {
    enabled       = true
    namespace     = "vcd-nodescale-system"
    chart_repo    = "https://charts.example.com"
    chart_version = "0.1.0"
    image_tag     = "v0.1.0"
    # ...vcd service account, flavor defaults, etc.
  }
  ```
  (A structured object is preferred over a bare bool for future extensibility; a bare
  `enable_node_controller = true` is acceptable as a minimum.)
- Note: implementing this is **future work**; this document only records the decision and the
  required Terraform/README changes.

### 3.6 Controller runtime location — **DECIDED: in-cluster Deployment in a dedicated namespace**
- Run as a `Deployment` (leader-elected) inside its **own namespace**, not `kube-system`, to isolate
  RBAC and blast radius.
- **Recommended namespace name:** `vcd-nodescale-system`
  - Clear, vCD-specific, follows the `*-system` convention used by infra components.
  - Alternatives: `nodescale-system` (your proposal — fine), `vcd-node-autoscaler`,
    `vcd-cluster-nodescale`. Avoid `kube-system`.
- vCD credentials supplied via a `Secret`; egress via the existing SNAT configured by Terraform.

### 3.7 Repository & delivery topology — **DECIDED: separate git repo for the Go code**
- The controller (Go source, CRD types, RBAC, Helm chart, tests, CI) lives in a **separate git
  repository**, independent of this Terraform module.
- **Suggested repo name:** `vcd-k8s-node-controller` (alternatives: `vcd-nodescale-controller`,
  `vcd-node-autoscaler`, `k8s-vcd-node-operator`).
- **Suggested layout:**
  ```
  vcd-k8s-node-controller/
  ├── api/v1alpha1/            # CRD Go types + deepcopy
  ├── cmd/manager/             # main entrypoint
  ├── internal/controller/     # reconcilers (worker pool, node watcher)
  ├── internal/cloud/          # platform contract: WorkerCompute, CapacityReader,
  │                            #   optional PoolMembership, Factory + Placement
  ├── internal/cloud/vcd/      # govcd factory + per-pool provider
  ├── internal/cloud/vsphere/  # vSphere stub (same contract, ErrNotImplemented)
  ├── internal/kube/           # client-go KubeProvider implementation
  ├── internal/cloudinit/      # user-data/initscript templates (go:embed)
  ├── config/                  # kustomize: crd, rbac, manager
  ├── charts/vcd-nodescale/    # published Helm chart
  ├── test/                    # unit + envtest + e2e
  ├── .github/workflows/       # CI: lint, test, build image, package chart
  ├── go.mod
  └── README.md
  ```
- **Release artifacts:** container image (semver tag) in a registry, Helm chart (semver), CRD
  manifests. Terraform pins `chart_version` + `image_tag`.
- **Consumption contract:** this Terraform module installs the controller (gated by
  `node_controller.enabled`) using a Helm provider resource that points at the published chart and
  pins versions. The module must remain usable *without* the controller (default disabled) so the
  existing workflow is unaffected.
- **CI/CD (in the new repo):** lint + `go test`, reconciler `envtest`, cloud-init schema validation,
  image build/push, chart lint/package/publish, SBOM/signing (optional).
- **Documentation:** this local `conception.md` should move to the new repo's `docs/` once it is
  created (keeping a local copy here is fine meanwhile).
- **Important:** the controller repo must **not** depend on this Terraform repo to build/test; the
  only shared surface is the CRD/Helm contract and the values that Terraform passes.

---

## 4. Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Kubernetes cluster — namespace: vcd-nodescale-system             │
│                                                                   │
│  CRD: K8sWorkerPool (singleton "default")                         │
│    spec:   replicas, node flavor (cpu/mem/disk), vCD refs,        │
│            ip plan (cidr + offsets + reserved), join settings      │
│    status: nodes[] {name, ip, providerID, cpu, mem, diskGB,       │
│                     k8sReady}, ledger {nextIndex, used, free},     │
│            conditions[]                                           │
│                                                                   │
│  WorkerPoolReconciler (controller-runtime)                       │
│        │  level-based: desired vs actual → converge               │
│        ├─ CloudProvider  (govcd)   → create/delete VM, disks,     │
│        │                              extraConfig, metadata,      │
│        │                              LB pool member add/remove    │
│        ├─ KubeProvider   (client-go)→ bootstrap token, CA hash,   │
│        │                              list nodes, wait Ready,      │
│        │                              drain, delete node           │
│        └─ CloudInitRenderer        → initscript + user-data       │
└──────────────────────────────────────────────────────────────────┘
        ▲ Helm chart / image published from the separate git repo (§3.7)
        │ installed & version-pinned by the Terraform module (node_controller.enabled)
```

### 4.1 CRD sketch (illustrative — not applied)

```yaml
apiVersion: k8s-nodescale.github.io/v1alpha1
kind: K8sWorkerPool
metadata:
  name: default
spec:
  replicas: 2
  flavor:
    cpus: 4
    memoryMB: 8192
    template: ubuntu24.04-uuid-enabled-v1
    disks:                       # mirrors Terraform disks_config diskw1/diskw2
      - {sizeGB: 20, mount: /data1}
      - {sizeGB: 60, mount: /data2}
  vcd:
    org:    myorg
    vdc:    My-VDC-Name
    vapp:   k8s-lab-vapp
    orgNetwork: routed-k8s-net
    catalog: Templates
    dvmOffset:       -3            # deployment VM slot, vCD bootstrap only
  ipPlan:                        # mirrors Terraform var.ip_plan
    cidr:            192.168.100.0/24
    mNodeOffset:     4
    wNodeOffset:     7
    kubeapiLB:       2
    ingressLB:       3
    reserved:        [192.168.100.1, 192.168.100.2, 192.168.100.3]
  join:
    endpoint: 192.168.100.2:6443
    tokenTTL: 1h
  ingressPool:
    type: nsxv            # nsxv | nsxt
    name: myorg-k8s-cluster-prod-ingress-pool
status:
  observedReplicas: 2
  ledger:
    nextIndex: 2
    used: [192.168.100.7, 192.168.100.8]
    free: []
  nodes:
    - name: myorg-k8s-cluster-prod-wrk-03
      ip: 192.168.100.9
      providerID: urn:vcloud:vm:...
      phase: Ready            # Pending|Provisioning|Joining|Ready|Draining|Deleting|Failed
      cpu: 4
      memoryMB: 8192
      diskGB: 80
      k8sReady: true
  conditions:
    - {type: Ready, status: "True", reason: Converged}
```

### 4.2 Component interfaces (for testability)
- `CloudProvider`: `ListVMs(vapp, selector)`, `CreateVM(spec)`, `DeleteVM(id)`, `AddDisk`,
  `SetExtraConfig`, `SetMetadata`, `AddPoolMember(pool, ip)`, `RemovePoolMember(pool, ip)`.
- `KubeProvider`: `CreateBootstrapToken(ttl)`, `CAHash()`, `ListNodes()`, `WaitReady(node)`,
  `Drain(node)`, `DeleteNode(node)`.
- `IPAllocator`, `NameAllocator` (ledger-backed, ip_plan-aware).
- `CloudInitRenderer` (Go `text/template` + `embed`), producing both the `initscript` and the
  cloud-init user-data.

---

## 5. Algorithms

### 5.1 Scale-up (desired > actual)
1. List worker VMs in the vApp via metadata (`managed-by=operator`, `pool=default`) → actual set.
2. For each missing index: allocate name + IP from the ledger (ip_plan-aware); verify free against
   actual vCD VM IPs.
3. `CreateBootstrapToken(ttl)`; compute CA hash.
4. Render cloud-init = OS prep (Option A disks/`/var`) + pinned K8s binaries + `kubeadm join
   <endpoint> --token .. --discovery-token-ca-cert-hash ..`.
5. govcd: create VM from template in vApp → attach Org network MANUAL IP → CPU/mem → internal disks
   → `disk.EnableUUID=TRUE` → guest customization initscript (+ `guestinfo.userdata`) → metadata →
   power on.
6. Mark `Provisioning` → watch `Node` → `Ready` → `Joining` → **add IP to ingress LB pool** →
   `Ready`.
7. Update `status` (counts, per-node resources, ledger, conditions); requeue with backoff.
8. Watchdog: not Joined within T → `Failed` → delete + recreate.

### 5.2 Scale-down (desired < actual)
1. Select victims deterministically (highest index / least utilized); ensure capacity headroom.
2. `cordon` → `drain` (PDB-aware) → **remove from ingress LB pool** → `kubeadm reset`
   (best-effort) → delete VM + internal disks → free IP/name in ledger → delete `Node` object.
3. Update `status`.

### 5.3 Continuous observation (requirement 1)
- Watch `Nodes` via client-go informer → allocatable CPU/memory, readiness, version.
- Query vCD → VM power state + disk sizes.
- Persist into `status`; optionally mirror a read-only `cluster-inventory` ConfigMap for humans.

### 5.4 Terraform parity for generated cloud-init (Option A)
Port the relevant `prepare_k8s_nodes` + `install_k8s_binaries` steps into the generated user-data:
netplan, swapoff, `modules-load.d`, `sysctl.d`, containerd config (`SystemdCgroup=true`),
`pkgs.k8s.io` repo + pinned/hold binaries, LVM on `sdb`/`sdc`, `/var` & `/var/lib` move, one reboot,
then `kubeadm join`. Validate with `cloud-init schema --annotate`.

---

## 6. Phased action plan

| Phase | Scope | Output |
|---|---|---|
| **0 — Decisions** | Resolve remaining open items (§8): disk details, IP reuse policy, NSX priority, template version pinning, repo name/registry. | Signed-off decisions. |
| **1 — Repo & skeleton (read-only)** | Create the **separate git repo**; scaffold Go module + `controller-runtime`; add CRD `K8sWorkerPool`; `CloudProvider` read-only (list VMs/disks/pool members); `KubeProvider` (list nodes, CA hash, token create). | Reconciler computes observed status only. |
| **2 — Scale-up** | `CloudInitRenderer` (Option A parity); govcd `CreateVM` full flow; wait `Ready`; **LB pool add after Ready**; metadata discovery; failure watchdog. | 2→N scaling works end-to-end. |
| **3 — Scale-down** | Cordon/drain/reset, LB remove, VM+disk delete, ledger free, Node delete; finalizers; leader election. | N→2 scaling works. |
| **4 — Packaging & integration** | Helm chart + image release from the new repo; add `node_controller` variable + README warning to this module; install controller via Helm provider gated by `node_controller.enabled`; ensure Terraform stops owning dynamic worker pool members post-cutover; e2e on dev tenant (2→4→2 via CRD). | Published release + cutover runbook. |
| **5 — Hardening** | HA, backoff/retries, PDBs, least-privilege vCD service account + K8s RBAC, chaos tests (vCD API errors, partial create). | Production-ready. |

### 6.1 Concrete Terraform-side changes (future, for §3.5 / §3.7)
- Add `node_controller` variable (object) with `enabled`, `namespace`, `chart_repo`, `chart_version`,
  `image_tag`, `values`.
- Add a `helm_release` resource (or manifest-based install) gated by
  `var.node_controller.enabled`.
- Update `README.md` with the "do not use Terraform after enabling the controller" warning.
- Decide how worker LB pool membership is handed off (remove `dynamic "member"` for workers, or
  scope it to a baseline and let the controller own the rest).

---

## 7. Security & RBAC notes
- **K8s RBAC (controller SA):** `nodes` (get/list/watch/patch/delete), `pods/eviction` (drain),
  `secrets` create in `kube-system` (bootstrap tokens), `configmaps` get in `kube-public`
  (cluster-info/CA), CRD + status patch.
- **vCD identity:** dedicated least-privilege service account limited to the target Org/VDC/vApp
  (catalog read, VM create/delete, internal disks, network attach, LB pool edit, metadata).
- **Secrets:** vCD credentials in a namespaced `Secret`; never in the CRD spec.
- RBAC/manifests are shipped in the separate repo via the Helm chart / kustomize.

---

## 8. Open questions / risks
1. **Option A details** (§3.2): one-shot boot guard, reboot detection, idempotency, recovery if the
   middleware step fails — to be discussed.
2. **IP reuse policy** on scale-in: never reuse vs reuse freed IPs.
3. **NSX-V vs NSX-T** priority for LB pool updates (confirm both required from day one).
4. **Template drift / version pinning:** kubeadm/kubelet versions must be read from the live cluster
   (or CRD), not hardcoded; confirm the template remains the source of truth.
5. **govcd method signatures** must be verified against the pinned `go-vcloud-director/v2` version
   (internal-disk add, guest customization section, NSX-T ALB pool member update). Short spike
   against a dev tenant recommended in Phase 1.
6. **Scale-down safety:** minimum worker count / explicit approval / PDB requirements.
7. **Migrating from an existing Terraform-managed cluster:** how to hand off worker ownership
   without recreating VMs (import vs greenfield).
8. **Separate repo specifics:** final repo name, image registry, chart repository, and whether the
   Terraform module installs via Helm provider or raw manifests/CI.

---

## 9. Explicit non-goals (for now)
- Managing master/control-plane nodes.
- Replacing the DVM/Ansible **initial** bootstrap (the controller handles scaling only).
- Building a full autoscaler on utilization metrics (start declarative `spec.replicas`; metrics-based
  policy can be added later).

---

*End of document.*

## 10. Reconciliation policy, ownership and placement (0.4.0 design)

These entries extend section 3. 0.3.2 shipped only the correction in 10.4;
0.5.0 implements 10.1-10.6 as written here, with the deviations noted in 10.7.

### 10.1 Ownership — DECIDED: everything with the prefix, no metadata check

The pool owns every VM in the vApp named `<namePrefix>-wrk-*` plus that worker's
Node object. Terraform's bootstrap workers carry no controller metadata and are
adopted on the first pass exactly like self-created ones: they enter
`status.nodes`, reserve an address in `status.ledger` and become candidates for
every removal path. There is deliberately no read-only class of worker, because
a worker the pool reports on but cannot remove is worse than one it owns.

Consequence for Terraform (section 3.5): once the controller is enabled,
bootstrap workers are removed **through Terraform** (`workers.vm_count`) with
`spec.replicas` lowered in the same change. `deleteWorkers` on a bootstrap worker
is unsupported: Terraform would recreate the VM at the address the pool just
freed and handed to another worker.

Implemented in 0.5.0: a `WorkerAdopted` event when a VM enters `status.nodes`
for the first time, and the rule that a worker adopted in the current pass is not
a scale-down candidate in that pass, so an outside `terraform apply` cannot turn
into "the controller deleted a different worker".

### 10.2 Missing workers — DECIDED: `spec.reconciliation.policy: Notify | AutoHeal`

Default `Notify`. The unit is the **slot**: one address in `status.ledger.used`
plus the worker name derived from its ordinal, so a missing worker keeps its
identity. A slot whose VM is gone keeps its address reserved and appears in
`status.nodes` with phase `Missing`; the pool reports `Ready=False` with reason
`WorkerMissing` and the worker name/IP in the message instead of silently
provisioning a replacement under a new name (which is what 0.3.x does, because
`observedReplicas` drops and the replica arithmetic reads it as a shortfall).

- `Notify`: never create a VM for a slot that disappeared outside the loop;
  report it. A `deleteWorkers` entry naming an absent worker is pruned and
  `spec.replicas` is aligned with the observed count - the CR is lined up with
  reality, nothing is created. A count-based scale-in writes off a missing slot
  before it considers removing a live worker.
- `AutoHeal`: recreate the slot (same name, same address) with bounded retries
  - `reconciliation.autoHealBackoff` (fixed, default `2m`) between attempts and
  `reconciliation.autoHealRetries` (default `3`) attempts, then park the slot
  as `Failed` with the reason. "VM alive, Node object gone" is repaired by one
  guest reboot under the same budget. A worker with a pending removal is never
  healed.

Rejected alternatives: healing by default (creates VMs, i.e. state and cost, on
a transient vCD hiccup, and hides a pattern of outside deletions); freeing the
address of a vanished worker (loses the identity and lets a later scale-out
reuse the name of a worker nobody asked to remove).

### 10.3 Placement — DECIDED: control plane, so the pool can reach zero

Removing the last worker, and scaling from zero, require a reconciler that does
not run on a pool worker: the final pass would otherwise die with the VM it is
deleting and could not write `status`, create the replacement, or finish the
bookkeeping. The manager requests 100m CPU / 512Mi, and masters never match the
`-wrk-` prefix, so they cannot be deleted through the CRD. Topology: 2
replicas, leader election, required `podAntiAffinity` on hostname, toleration for
`node-role.kubernetes.io/control-plane:NoSchedule`. `dnsPolicy: Default` is the
fallback if CoreDNS cannot run on a master (the controller resolves the vCD API
through cluster DNS, and at zero workers a CoreDNS pod pinned to a worker would
have nowhere to run).

With this placement `scaleDown.minReplicas: 0` is supported and `spec.replicas`
can go 0 -> N. With the controller on a pool worker the last worker is refused
(`LastWorkerProtected`, detected by comparing the request with the controller's
own Pod `spec.nodeName` from the downward API).

Implemented in 0.5.0: the chart exposes `replicaCount`, `nodeSelector`,
`tolerations`, `affinity`, `priorityClassName` and `dnsPolicy`, and the
controller reads its own Pod through the downward API (`POD_NAME` /
`POD_NAMESPACE`) for the last-worker guard.

Open: whether a dedicated non-pool worker is preferable to the control plane in
environments where the masters are small, and the Terraform module still has to
pass the placement values through to the chart (the chart values are there; the
module variables and the `terraform.tfvars` pins are 0.5.0 release work).

### 10.4 Node object deletion — DECIDED: only with a power-off, only for owned workers

`DELETE /api/v1/nodes/<name>` only removes a registry record: it does not stop
kubelet and does not touch the VM. Two consequences define the order: a Node
deleted while the guest still runs is re-registered within seconds and is left
behind as a `NotReady` corpse; and while the Node object exists, the pod garbage
collector cannot reap the DaemonSet and mirror pods that a drained worker leaves.
Therefore the removal pipeline is: cordon -> ingress pool removal -> drain
(eviction API, PDB honoured) -> VolumeAttachment wait -> **hard power-off** ->
Node delete -> VM removal -> address release. The controller deletes a Node
object only for a worker it is removing, or (`AutoHeal`) for the worker it is
repairing; masters, the DVM and VMs outside the prefix are never touched.

0.3.1 violated this in the worst way: it passed the vCD VM name
(`KION-WEB-DEV-wrk-03`) to the Kubernetes API instead of the Node object name
(`kion-web-dev-wrk-03`); `Cordon` and `DeleteNode` swallowed the 404 and `Drain`
selected no pods, so workers were removed with the guest still running, no
cordon, no drain, no PDB, and a `NotReady` Node object left in the cluster.
0.3.2 keeps the Node name in `status.nodes[].nodeName`, uses it for every
Kubernetes call and powers the guest off before deleting the Node object.

### 10.5 Emptying the pool — DECIDED: an explicit, deliberate owner action

The controller must be able to take a pool to zero (that is what makes
scale-from-zero and "park the cluster" possible), but it must never do so
because a count says so, and it must not hide the consequence behind a
"protected" refusal that looks like a bug.

Two conditions, both explicit: `scaleDown.minReplicas: 0` and
`scaleDown.allowLastWorker: true`. Without the flag a request that would empty
the pool is refused with `LastWorkerProtected` and the worker is left alone. With
it, the removal proceeds through the normal pipeline, which means the PDBs still
decide whether it can finish.

`minReplicas` is a floor on workers able to serve, not on rows in status: a
`Missing` slot has no VM and a `Failed` slot is parked, so neither counts.
Writing off a `Missing` slot takes nothing out of service and is therefore never
refused by the floor, but it is still a removal and cannot empty the pool
without the flag. The CRD defaults `minReplicas` to 1, so a manifest that omits
it is protected; `allowLastWorker` with a higher floor is rejected as
`InvalidSpec`. A refusal that arrives while a drain is already under way reverts
the cordon instead of leaving the worker `Draining`; a worker already in
`Deleting` is past the point of no return and finishes.

Being explicit about the blast radius is part of the design, and it is documented
in the README rather than buried here: the last drain evicts every pod on that
worker (ingress, any CoreDNS replica that landed there, metrics-server, CSI node
plugins, the calico DaemonSet); a `PodDisruptionBudget` whose `minAvailable`
equals its replica count blocks the drain forever, and that block is the correct
answer, so the worker stays and the node is marked `Failed`; what remains at
zero is the control plane, CoreDNS when it can run on a master, the controller
and the vCD API, so everything with a PDB, a `hostPort` or a `local`/`hostPath` PV
is down until the pool is refilled; and refilling is a cold start measured in
minutes per worker.

Two refusals survive the flag because they are not policy: the node the
controller itself runs on (the pass would die with it), and a worker with
`local`/`hostPath` PVs (`scaleDown.force` overrides only that check, never a
PDB).

### 10.6 Rolling compute-spec change — IMPLEMENTED in 0.5.0

Goal: change CPU, memory or disk of the workers with no redeploy of the cluster
and no manual vCD work.

**Source of truth.** `spec.flavor` describes what a worker should be, and every
`status.nodes[]` entry gains the flavor the VM actually runs with, read back
from vCD. A worker whose actual flavor differs from the spec is a roll-out
candidate, so the loop is idempotent, survives a lost status, and also notices a
VM someone resized by hand.

**Why a power cycle rather than a hot change.** vCD can hot-add CPU and - on
guests that support it and with the VM flag enabled - memory, but Kubernetes
capacity follows only after kubelet re-reads it, and a grown disk still needs the
guest to extend the filesystem. A cold power cycle turns all three into a known
state and makes the Node re-register with the new capacity. Shrinking is not
supported by vCD for CPU/memory flags and impossible for disks: a shrink is a
"recreate" operation, out of scope here, and the controller will report it as an
unsupported change instead of silently ignoring it.

**Per-worker sequence** (the existing removal pipeline with a resize in the middle
instead of a delete):

```
cordon (nodeName)
  -> remove from ingress pool
  -> drain (eviction API, PDB honoured)
  -> wait for VolumeAttachments to detach
  -> power off the VM
  -> ChangeCPUCount / ChangeMemorySize / ChangeDiskSize (grow only)
  -> power on
  -> wait Node Ready and status.capacity matches the new flavor
  -> update status.nodes[].flavor
  -> uncordon
  -> add back to the ingress pool
  -> next worker
```

Every step is idempotent, which is what makes a controller restart in the middle
of a roll safe: cordoning a cordoned node, draining an empty node and powering
off an off VM are all no-ops, so an interrupted worker is simply re-driven from
the top.

**Batching.** `rollout.maxUnavailable` (int or percentage, default `1`) sets how
many workers may be out of service at the same time; drains of a batch run
concurrently, each bounded by `drainTimeout`. The user-chosen number is only
the upper bound - the real limit is relocation headroom, and PDBs enforce it by
making the eviction API refuse instead of pushing the survivors over the edge. A
PDB block parks that batch (`Failed` on those nodes, VMs intact, roll paused),
not the whole pool. Scale-up is allowed during a roll (it only adds capacity);
scale-in and `deleteWorkers` are refused with `RolloutInProgress`, because a worker
cannot be resized and removed in the same pass.

**Order.** Highest ordinal first, so `wrk-01` - usually the ingress and, on a
small cluster, the controller's own host - moves last; `rollout.order: Ascending`
for the opposite.

**Failure and resumption.** `rollout.pauseOnFailure: true` (default): a worker that
does not return Ready within `rollout.readyTimeout` parks the CR at `Failed` with
that worker cordoned and its VM intact, and the roll stops there. The manual
escape is the existing `deleteWorkers` path: replace the worker, then resume.

```yaml
spec:
  flavor:
    cpus: 8
    memoryMB: 16384
    osDiskGB: 40
  rollout:
    strategy: Rolling          # Rolling | Manual
    maxUnavailable: 1          # int or "25%"
    order: Descending          # Descending | Ascending
    pauseOnFailure: true
    readyTimeout: 10m
status:
  rollout:
    phase: Progressing
    targetFlavor: {cpus: 8, memoryMB: 16384, osDiskGB: 40}
    workersTotal: 3
    workersDone: 1
    currentWorker: KION-WEB-DEV-wrk-02
    lastError: ""
```

`strategy: Manual` rolls nothing until the owner marks one worker with
`k8s-nodescale.github.io/resize=allow` - for environments where the window matters more
than the API.

**Implemented in 0.9.0.** A template change or a shrink is a recreate: drain,
power off, delete the VM, create a new one from the new spec **into the same
ledger slot**, so the name and the address are preserved. The ledger is what makes
that possible. The worker walks `Cordoned` -> `Deleting` -> `Creating` ->
`Booting`; `status.nodes[].resize.recreate` carries the mode so a restart
resumes in it, and the template is recorded on the VM at create time (read back
as `status.nodes[].flavor.template`) so a different image is detected at all. A
worker adopted without that record carries no template opinion.

### 10.7 Implementation notes and deviations (0.5.0)

What the code does, where it differs from the design above:

- **Drains inside a batch are serialized.** `rollout.maxUnavailable` decides how
  many workers may be *out of service* together (they are cordoned, drained and
  stopped one after another inside the pass loop), not how many drains run in
  parallel goroutines. The bound that matters - relocation headroom - is
  enforced by the eviction API and PDBs, not by concurrency, and serializing
  keeps one status write per pass.
- **One step per pass per worker.** A worker advances at most one step per
  reconcile (`Cordoned` -> `Stopped` -> `Booting` -> done, or `Cordoned` ->
  `Deleting` -> `Creating` -> `Booting` -> done for a recreate), which is what
  makes a controller restart mid-roll resume instead of restarting the worker.
- **Ready is verified against vCD, not the Node's capacity.** After the power-on
  the controller waits for `Ready=True` and for the flavor it read back from vCD
  to equal `spec.flavor`; the Node's allocatable is not used as the resize
  oracle, because it does not change until kubelet re-reads the new shape.
- **The system disk grows through the VM spec section** (`VcdVmSpecSection`), CPU
  and memory through `ChangeCPUCount`/`ChangeMemorySize`; the disk is applied
  first so the spec-section write cannot carry the pre-resize CPU/memory back.
- **Shrink was refused per worker in 0.5.0.** `Ready=False` with reason
  `UnsupportedChange`, no changes to the VM. Since 0.9.0 a shrink and a
  template change are a recreate into the same slot (see **Later** above) and
  `UnsupportedChange` is gone.
- **`AutoHeal` uses a fixed back-off**, not exponential (section 10.2). Bounded
  attempts are what stop a broken template; a fixed interval is easier to reason
  about and to observe in events.
- **A missing slot is written off, not reused, by a plain scale-out.** A reserved
  address is only freed by an explicit removal (scale-in or `deleteWorkers`) and
  only refilled by `AutoHeal`; otherwise a scale-out would silently resurrect a
  worker nobody asked to bring back.
- **Placement lives in the chart, not the CRD.** The pool does not decide where
  the controller runs; the chart values do.

