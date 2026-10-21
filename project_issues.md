# project_issues.md — worker bootstrap on vCD (live dev cluster)

> **Update 2026-10-21 (image 0.2.0).** The staged sequence was implemented and validated on two scratch
> VMs, then released: `internal/cloudinit` renders `0.2.0`, the guest runs `pre` = netplan + seed and
> `post` = disks + instance epoch, and all boot work moved into three marker-guarded units
> (`vcd-ensure-layout`, `k8s-node-prereqs`, `k8s-node-join`). A fresh VM built from the shipped render
> reached `GC_COMPLETE`, rebooted, mounted `/var` on `data1_lv` and `/var/lib` on `data2_lv`, and joined
> **by itself** - `prereqs done` 13:43:49, `join finished` 13:43:50. The run found four more defects,
> all fixed before the release and recorded in `customization-phases.md` §10:
> an fstab entry that did not mount (empty `/var/lib`), `lock_passwd` locking sudo, `enable` without
> `--now` so the first boot never joined, and a bootstrap token whose missing JWS entry makes
> `kubeadm join` hang silently forever.
>
> Still open: Ready-path token/JWS cleanup (9 tokens were live on the cluster), GC-status polling,
> `subPhase`, the failed-provisioning watchdog, `DeleteVM`/scale-down. The dev cluster used for the
> validation was torn down afterwards with Terraform; the entries below describe it as it was.


**Datetime tag: 2026-10-20 20:10 UTC**
**Scope:** `vcd-k8s-node-controller` (this repo), live cluster `KION-WEB-DEV`, vApp `K8s_cluster`.
**State at end of day:** root cause fixed in code (uncommitted, tests green), **not yet shipped**; two
follow-on defects of the customization path identified today (both mitigated, both awaiting an end-to-end
retest); the live cluster is still on the broken image `0.1.5` and the worker pool is stuck in
`ScaleUpFailed`.

---

## 1. Day result in three lines

1. The original failure (`Customization.pm line 637`, `GC_FAILED`) is caused by **rebooting the guest during
   the `precustomization` phase**; the fix (phase-aware customizer, no reboot, disks prepared in
   `postcustomization`) is written and unit-tested.
2. A **second, independent defect** surfaced while validating the fix on a scratch VM: vCD's own
   customization step ends `ERRORED` / stalls when the guest has no working network at that moment. The
   proven Terraform module avoids it by configuring **netplan inside `precustomization`** — that block was
   restored today.
3. A **third defect** explains why even a `GC_COMPLETE` VM never joined: the NoCloud seed written during
   customization is **ignored by cloud-init** because an instance with the same id was already recorded on
   the first boot (OVF datasource). Fixed by `cloud-init clean` before vCD's reboot — the step the working
   Terraform script performs and that the rewrite dropped.

---

## 2. Original symptom

Workers created by the controller never join:

- VM created, powered on, `guestCustomizationStatus = GC_FAILED` (later stuck at `GC_PENDING`).
- `wrk-03` / `wrk-04` had no `/var/lib/cloud/seed/nocloud`, no `kuberadm` user, no kubelet.
- Guest customization log (read from a working node, same template):
  `RunCustomScript has completed` → `Execution of precustomization failed ... Customization.pm line 637`.

Mechanism (confirmed on `wrk-01`, a Terraform-built worker, log
`/var/log/vmware-imc/toolsDeployPkg.log`):

```
14:27:05 Handling pre-customization
14:27:05 RunCustomScript invoked ... for 'precustomization'
14:27:21 RunCustomScript has completed          <- 16 s, script exits 0
14:27:21 Customizing Network settings / NICS / Hosts / DNS / Date&Time / password
14:27:22 Handling post-customization
14:27:22 RunCustomScript invoked ... for 'postcustomization'
14:27:22 RunCustomScript has completed
14:27:23 Trigger reboot / Rebooting             <- vCD reboots the guest itself
```

The old controller script ran the disk migration (which ends with `reboot`) in **both** phases. The reboot
in `precustomization` kills the Perl customizer → `GC_FAILED`, and everything after it (seed, cloud-init,
join) never happens. `wrk-03`/`wrk-04` were unrecoverable (half-migrated `/var`), so they were deleted
rather than repaired.

---

## 3. Code changes (uncommitted, on top of the stage-11 work)

| file | change |
|---|---|
| `internal/cloudinit/templates/node-initscript.sh.tmpl` | phase-aware entry point: `phase="$1"`; `precustomization` = configure netplan + `netplan apply` + `exit 0` (no reboot); `postcustomization` = write NoCloud seed, `cloud-init clean`, prepare data disks, `exit 0`; no `exit 42` convention, no manual `cloud-init init/modules` |
| `internal/cloudinit/templates/prepare-data-disks.sh.tmpl` | removed the trailing `reboot` and the `exit 42` contract; ends with `touch $MARKER; sync; exit 0` |
| `internal/cloudinit/templates/worker-userdata.yaml.tmpl` | `bootcmd` disk call is an unguarded retry: `[ -x ... ] && ./prepare-data-disks.sh || echo ...`; no special handling of `42` |
| `internal/cloudinit/renderer_test.go` | `TestInitscriptIsPhaseAware` asserts: phase argument exists, both phase branches exist, `cloud-init clean` is present, and neither the initscript nor the disk script contains a `reboot` |

Verification: `go vet ./...` clean, `go test ./...` all packages `ok`
(`cloudinit`, `cloud`, `cloud/vcd`, `controller`, `ipam`, `kube`).

`hack/diagvm` and `hack/rmvm` are **throwaway diagnostics, untracked** — delete before committing.

---

## 4. Findings

### F1 — reboot in `precustomization` aborts the customizer (confirmed, fixed)

Evidence: the `wrk-01` success path above vs. the `wrk-03` failure (`Customization.pm line 637`) plus the
`GC_FAILED` status. Fix: phase-aware script, no reboot at all; vCD reboots after `postcustomization`
(visible in the log: `Trigger reboot` / `Rebooting`).

### F2 — vCD's customization step fails when the guest has no network yet (mitigated, not yet re-tested)

Scratch VM `diag-wrk-02` (created through the controller's own `CreateVM` code path, static IP `.12`), tap
trace streamed from inside the guest:

```
19:20:13  === invoked phase=precustomization ===
          connect: Network is unreachable        <- guest had no network at that moment
19:20:13  precustomization: nothing to do
          ... postcustomization never ran ...    <- GC_PENDING for 20 minutes
          ... vCD log later shows: EXIT STATE 'ERRORED' for that attempt
19:40:27  (after power-cycle + customizeAtNextPowerOn) precustomization
19:40:48  phase=postcustomization -> seed written -> disk prep rc=0 in 3 s
19:40:31  vCD: "Cloud-init status is 'done'" -> "Trigger reboot" -> Rebooting
19:41     GC_COMPLETE                            <- first controller-created VM ever to get here
```

So the 20-minute stall was **not** the disk work (3 s) and **not** our script: vCD's own customization
ended `ERRORED` right after `precustomization`, and only a power-cycle with `customizeAtNextPowerOn` re-ran
the workflow successfully.

The proven Terraform customization script (`tf-vcloud-k8s-module/templates/customization_node.sh`) writes
`/etc/netplan/01-netplan.yaml` and runs `netplan apply` **inside `precustomization`**; every Terraform-built
node on this cluster (`wrk-01`, `wrk-02`, masters) customized on the first try. The rewrite had dropped that
block. It is restored (phase-guarded, still no reboot). Status: **hypothesis strong, mitigation applied,
end-to-end retest pending**.

### F3 — cloud-init ignores the seed because the instance was already recorded (mitigated, not yet re-tested)

`diag-wrk-02` reached `GC_COMPLETE`, the guest is healthy (ping, `SSH-2.0-OpenSSH_9.6p1`), but **no
`diag-wrk-02` node appeared** 15+ minutes later — i.e. cloud-init never applied the seed.

Evidence from the same tap stream (vCD's check just before its reboot):

```
[info] Customization command output:
'status: done
'.
[info] Cloud-init status is 'done'.
[info] Cloud-init execution is not on-going.
```

`status: done` at 19:40 was the **first boot** (no seed existed then — the customizer had not completed
yet). That boot already recorded an instance for this VM (OVF datasource, `instance-id` derived from the
VM). When our seed appeared with `instance-id: diag-wrk-02`, cloud-init treated it as the same, already
configured instance and skipped user-data entirely: no `kuberadm`, no `bootcmd`, no join.

Checked on the working `wrk-01`:

- `datasource_list: [ NoCloud, OVF, None ]` — NoCloud wins when a seed exists;
- `/var/lib/cloud/instances/KION-WEB-DEV-wrk-01` (id from our `meta-data`) — the seed is what ran;
- `cloud-init clean` does **not** delete `/var/lib/cloud/seed` unless `--seed` is passed;
- the Terraform script calls `cloud-init clean` (then `cloud-init init`) — that is why it works there.

Fix: `cloud-init clean` in `postcustomization`, right after the seed is written and before the disk move.
Status: **mitigation applied, end-to-end retest pending**.

> Open risk of this fix: vCD inspects `cloud-init status` immediately after `postcustomization` to decide
> whether it may reboot. With the state wiped the status may read `not run` instead of `done`. It should
> still be "not on-going" (vCD only avoids a reboot when cloud-init is *executing*), but this must be
> watched on the next run. Safer fallback if vCD then skips the reboot: keep the state and only make the
> seed id unique (`instance-id: <node>-vcd`), which is enough to make cloud-init re-run.

### F4 — the broken image keeps creating VMs, and IPAM collides with the scratch VM (operational)

While the fix was being validated, the pool (`spec.replicas=4`) reconciled with the old image and created
`KION-WEB-DEV-wrk-05` (`.11`, old script, `GC_PENDING`) and then `KION-WEB-DEV-wrk-06`, which failed:

```
create worker KION-WEB-DEV-wrk-06: wait for VM "KION-WEB-DEV-wrk-06" power-on:
[400:BAD_REQUEST] The following IP/MAC addresses have already been used by running virtual machines
```

because `diag-wrk-02` holds `.12` and the controller's IPAM does not know about it. `wrk-06` is left
`POWERED_OFF`/`PARTIALLY_POWERED_OFF` with `GC_PENDING`. The pool must be scaled down (or the scratch VMs
deleted) before the next creation attempt.

---

## 5. Live environment snapshot (2026-10-20 20:06 UTC)

| VM | IP | state | GC | note |
|---|---|---|---|---|
| `KION-WEB-DEV-mst-01/02/03` | .4/.5/.6 | POWERED_ON | — | Ready, control-plane |
| `KION-WEB-DEV-wrk-01` | .7 | POWERED_ON | GC_COMPLETE | Ready (Terraform-built) |
| `KION-WEB-DEV-wrk-02` | .8 | POWERED_ON | GC_COMPLETE | Ready (Terraform-built) |
| `KION-WEB-DEV-wrk-05` | .11 | POWERED_ON | GC_PENDING | created by the old controller, old script, broken |
| `KION-WEB-DEV-wrk-06` | .12 | POWERED_OFF | GC_PENDING | creation failed (IP/MAC conflict), broken |
| `diag-wrk-02` | .12 | POWERED_ON | GC_COMPLETE | scratch validation VM, **not joined** |

Deleted today: `KION-WEB-DEV-wrk-03`, `KION-WEB-DEV-wrk-04` (unrecoverable), `diag-wrk-01` (scratch).

- Pool `default/kion-web-dev`: `spec.replicas=4`, `Ready=False`, reason `ScaleUpFailed`, 2/4 workers.
- Nodes Ready: 3 masters + `wrk-01` + `wrk-02`.
- Controller: `vcd-nodescale-system/vcd-nodescale`, image `...:0.1.5` (broken templates), `IfNotPresent`.
- Cluster facts used in the fix: vApp `K8s_cluster`, net `Kub_cluster_net02` `192.168.100.0/24`,
  VIP `.2`, workers `.7+`, join `192.168.100.2:6443`, template `Ubuntu-24.04-ENG-Server`,
  flavor 2 vCPU / 8192 MB, root 10 GB, data 10 GB -> `/var`, data 30 GB -> `/var/lib`.

---

## 6. Ruled out today

- **Slow storage / slow disk migration.** `wrk-01`: `dd oflag=direct` 436 MB/s; the full disk preparation
  (LVM + rsync `/var` and `/var/lib` + fstab) took **3 s** on `diag-wrk-02` (`disk prep rc=0`).
- **Stuck vCD task blocking the queue.** `GET /api/query?type=task&filter=status==running` -> 0 running;
  no failed task other than the IP conflict above.
- **Customization not configured / silently disabled.** `GuestCustomizationSection` for broken, working and
  scratch VMs is equivalent in the relevant fields (`Enabled=true`, `AdminPasswordEnabled=false`, script
  present, `ComputerName` set); the scratch VM customized fine.
- **Script delivery broken.** The tap listener on the DVM was reachable (`192.168.100.253:9999` connected
  from `wrk-01` and from `diag-wrk-02`); the script ran and logged in both phases.
- **Template / VMware Tools change.** `VmToolsVersion = 13322` on all VMs (working and broken).
- **`UseCloudInitWorkflow`.** Returns `4` -> "Executing traditional GOSC workflow" on this vCD; the
  customizer script is the supported path (same on the working nodes).

---

## 7. Next steps (in order)

1. **Validate F2 + F3 in one shot** on a fresh scratch VM (`diag-wrk-03`, `.13`) using the initscript
   rendered from the *current templates* (not the hand-made tap script) and a user-data carrying a valid
   join token. Expected: `GC_COMPLETE` **on first boot** (no power-cycle) and the node `Ready` in ~10 min.
   If the risk above materialises (`cloud-init status: not run` -> vCD skips the reboot), switch to the
   unique-instance-id variant.
2. **Stop the bleeding:** `kubectl -n default patch k8sworkerpool kion-web-dev --type merge -p '{"spec":{"replicas":2}}'`,
   delete `wrk-05`, `wrk-06`, `diag-wrk-02` (and `diag-wrk-03` after validation).
3. **Ship the fix:** build image `0.1.6` (scratch base + locally built static `manager` + host
   `ca-certificates`, since the CI `docker-publish` job is broken), import into containerd on every node
   (`ctr -n k8s.io images import`), `kubectl -n vcd-nodescale-system set image deployment/vcd-nodescale ...`,
   confirm rollout.
4. **Verify the production path:** scale the pool back to `4`; both new workers must reach `GC_COMPLETE`
   without a power-cycle, join as nodes and become `Ready`.
5. **Record:** update `project_state.md` (datetime-tagged), remove `hack/diagvm` + `hack/rmvm`, commit the
   fix + this file. No push without explicit approval.

---

## 8. References

- Repro/validation harness (scratch, not part of the repo): `hack/diagvm` (replays `cloud.CreateVMSpec` +
  `vcdprov.New`; env `SCRIPT_PATH`, `VM_NAME`, `VM_IP`), `hack/rmvm` (`ls`/`rm` VMs in the vApp);
  binaries and generated scripts under `/tmp/opencode/probe/`.
- Guest-side instrumentation used today: `phase_tap.sh` (streams `toolsDeployPkg.log`, `lsblk` and a
  `bash -x` run of the disk script over TCP to the DVM listener `/tmp/tap.py`), log
  `/tmp/probe-tap.log` on the DVM.
- Working reference implementation: `tf-vcloud-k8s-module/templates/customization_node.sh` (netplan +
  seed + `cloud-init clean` + `cloud-init init/modules`, **no reboot**).
- Related code: `internal/cloud/vcd/provider.go` (`CreateVM` l.153, `setGuestCustomization` l.551,
  `adapterType` l.532).
- Credentials used for diagnostics live only in `/tmp/opencode/probe/{pw.env,vcd.env}` and
  `/tmp/._tmp_secret` (dedicated vCD diagnostic account; `kub_clusteradm` is locked out and was not used).
