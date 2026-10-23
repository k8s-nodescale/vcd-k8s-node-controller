# Customization phases — breakdown, and how to spread the work for reproducibility

Date: 2026-10-21. Local working note, same status as `project_state.md` / `project_issues.md`
(not tracked, not pushed).

Sources read: `internal/cloudinit/templates/*` (uncommitted 0.1.6 working tree),
`internal/cloudinit/renderer.go`, `internal/cloud/vcd/provider.go`, `internal/cloud/diskplan.go`,
`internal/controller/k8sworkerpool_controller.go`, `../tf-vcloud-k8s-module/templates/customization_node.sh`,
plus the live evidence in `project_state.md` and `project_issues.md`.

---

## 1. The pipeline as it exists today

### 1.1 Who runs what, in what order

`S` = stage. `⇒` marks a boundary the controller cannot retry.

| S | Step | Actor | Code | Can it fail & be retried? |
|---|---|---|---|---|
| S0 | token + CA hash + render user-data/initscript | controller | `scaleUp` 472-502 | yes (next pass), but leaks the token |
| S1 | `AddNewVM` (deploy, **powered off**), metadata, compute, `disk.EnableUUID` | controller/vCD | `CreateVM` 194-238 | yes on the vCD side; failure ⇒ VM removed |
| S2 | internal disks bus 1/0 + 1/1 | controller | `attachDisks` 236, 490-511 | yes; **VM is still off** |
| S3 | guest customization section (enabled, ComputerName, initscript) | controller | `setGuestCustomization` 240, 551 | yes |
| S4 | power on | controller/vCD | `CreateVM` 247 | yes |
| S5 | **precustomization** script | vCD ⇒ guest | initscript 59-70 | no — see §2 |
| S6 | vCD sets hostname/NIC/DNS/password, checks `cloud-init status` | vCD | — | no; **stalls** without network |
| S7 | **postcustomization** script | vCD ⇒ guest | initscript 72-108 | no |
| S8 | vCD reboots the guest (`REBOOT_PENDING`) | vCD | — | no; may be skipped |
| S9 | boot: cloud-init reads the NoCloud seed | guest | userdata | only if instance state was reset (F3) |
| S10 | `bootcmd`: disk retry, enable bootstrap unit | guest | userdata 20-31 | yes |
| S11 | `k8s-node-bootstrap.service`: swapoff, modules, sysctl, containerd, apt, join | guest/systemd | userdata 75-167 | yes (60 s retry, 45 min timeout) |
| S12 | Node registers → `Joining` → `Ready` | k8s/controller | `refreshReadiness` 185 | observation only |
| S13 | labels + taints after Ready | controller | `applyNodeAttributes` 258 | yes |
| S14 | token/JWS cleanup, GC-status polling, provisioning watchdog | **nobody** | — | **missing** |

### 1.2 The three handoffs that are not ours

Everything expensive in this pipeline sits behind three transitions we do not control and cannot
replay:

1. **S4→S5** vCD starts the customizer. From here the guest runs a script whose exit code is
   interpreted by a Perl customizer we cannot retry. A non-zero exit or a guest reboot leaves
   `GC_FAILED`, and recovery is manual (`power-cycle` + `customizeAtNextPowerOn`, see F2).
2. **S6→S7** vCD applies its own network configuration between the two phases. Its own step fails
   when the guest has no working network yet (F2), i.e. **our** network stage is a precondition of
   **their** stage.
3. **S8→S9** the single guest reboot. Both the disk move and cloud-init's second run depend on it.
   If vCD skips it, the VM is healthy, customized, and permanently not a node.

The immutable artefact is the customization script + user-data, baked into the VM at creation. There
is no "apply a new render" operation: a wrong render means delete the VM and create another (§5.1).

### 1.3 Task inventory — where each task lives today

| Task | Today | Needs reboot? | Idempotent? | Fails loudly? |
|---|---|---|---|---|
| netplan + `netplan apply` | initscript pre (59-70) | no | yes | logs only |
| NoCloud seed write | initscript post (72-81) | no | yes | `set -u` only |
| `cloud-init clean` | initscript post (89-91) | no | yes | logs only |
| LVM + mkfs + mount + rsync + fstab | initscript post (94-106) **and** `bootcmd` (20-26) | **yes** | yes (marker) | logs, continues |
| swapoff / modules / sysctl | bootstrap unit | no | yes | retried by systemd |
| containerd + kubeadm/kubelet apt install | bootstrap unit | no | `command -v` guard | retried |
| `kubeadm join` | bootstrap unit | no | `kubelet.conf` guard | retried |
| labels / taints | controller | no | yes | reconcile error |
| version stamping of the render | **nowhere** | — | — | — |
| layout verification before join | **nowhere** | — | — | — |

Two owners for the disk task and zero owners for the last two rows is the shape of the problem.

---

## 2. What the failures actually taught us

| Failure | Phase boundary it crossed | General rule it establishes |
|---|---|---|
| F1 reboot in `precustomization` → `Customization.pm line 637`, `GC_FAILED` | our reboot inside vCD's phase | **No guest stage may reboot.** vCD owns the only reboot. |
| F2 no network in `precustomization` → vCD's own step `ERRORED`, 20+ min `GC_PENDING` | our precondition vs. their step | **vCD's phases are not a place to do work that needs the network to be up first.** Bring the NIC up before their step, without side effects. |
| F3 seed ignored, `status: done` from the first boot | cloud-init instance state vs. wall-clock | **"It ran once" is not a state you can clean your way out of.** Make the instance-id depend on content, not on a name plus a cleanup call. |
| 0.1.3-0.1.5: JWS entry, CA pin, empty-value labels | pure functions executed in the guest/controller | **Anything expressible as a pure function belongs in Go with a fixture test**, not in a shell script baked into an immutable VM. |
| 10 tokens in 3 min, 1 h TTL, 20 min stall | token lifetime vs. phase budget | **A TTL must cover the whole S0→S11 budget**, and be deleted on both success and failure. |
| `strings manager2 \| grep SetLabels` as the only way to identify what a VM was built with | missing version stamp | **A guest must carry the identity of the render that produced it.** |

The Terraform path never hit these because it *deferred* the fragile half: Terraform created the
disks and the seed, then **Ansible** (run from the DVM, minutes later, with a human and unlimited
retries) did `/var` relocation, packages and join. Ansible roles are re-runnable because they are
declarative task lists with markers and handlers.

**That is the model to copy**: our job is to reproduce Ansible's role structure inside the guest,
not to flatten it into one linear script that vCD runs twice.

---

## 3. Recommended stage contract

Six stages, one owner each, each with an explicit budget, retry policy and failure visibility.
The arrow between S2 and S3 is the only place where vCD can strand us.

```
controller                vCD / guest (no retry)                      guest (retriable)
S0 build & stamp  ──►  S1 pre-boot (API)  ──►  S2 pre  ──► S3 post ──► reboot ──► S4 cloud-init ──► S5 prereqs ──► S6 join ──► S7 controller
(pure, testable)        (idempotent)           (≤30s)     (≤5min)      (vCD)      (declarative)   (systemd)       (systemd)
```

| Stage | Where | Budget | Exit policy | Marker | Failure visible as |
|---|---|---|---|---|---|
| **S0 build & stamp** | controller, pure | — | error before VM exists | render hash | reconcile error, no VM |
| **S1 pre-boot** | vCD API, powered off | 10 min | retry next pass | VM metadata | vCD task error |
| **S2 pre** | vCD phase 1 | 30 s | **always 0** | none needed | log + GC status |
| **S3 post** | vCD phase 2 | 5 min | **always 0**, writes `*.failed` | `/etc/vcd-data-disks.done` | GC status + watchdog |
| **S4 first boot** | cloud-init | 2 min | failures logged | `/var/lib/vcd-stage/` | Node absent |
| **S5 prereqs** | systemd oneshot | 20 min | retried 60 s | `prereqs.done` | unit state, Node absent |
| **S6 join** | systemd oneshot | 45 min | retried 60 s | `/etc/kubernetes/kubeadm-joined` | Node absent, deadline |
| **S7 postconditions** | controller | 15 s pass | requeue | CR status | `status.nodes[].subPhase` |

### Rules (these are the invariants worth encoding as tests)

- **R1** Exactly one reboot per VM, and it is vCD's. No guest script contains `reboot`.
- **R2** A script that runs inside a vCD phase always exits 0. Failures are recorded in a marker and
  retried later by something that *can* retry. `GC_FAILED` is reserved for "abort on purpose", which
  we do not use. (`set -uo pipefail` without `-e` is correct; keep it and assert it in a test.)
- **R3** Every stage re-runs safely: marker + precondition check + no destructive default. The disk
  script already refuses to overwrite a foreign signature (57-68) — that is the pattern.
- **R4** A vCD phase may only do what is needed to make the next boot possible: network, seed,
  instance epoch, and the one operation that the reboot pays for (moving `/var`). Nothing slow,
  nothing network-dependent, nothing with a package manager.
- **R5** Declarative before imperative: what can be a file in user-data is a file; what needs
  ordering, retry and a timeout is a systemd unit; what needs a second attempt after a reboot is a
  marker-guarded script used by both.
- **R6** One owner per field, marker and status value. (Same lesson as `1a5401d`, applied to the
  guest: `/var` is owned by the disk script, not by the disk script *and* the initscript.)
- **R7** **The join is gated on the layout.** Before `kubeadm join`, assert `findmnt /var`,
  `findmnt /var/lib` and sizes match `spec.flavor.disks`. A half-migrated guest must fail with a
  readable message instead of joining with `/var` on the root disk (the `wrk-03` class).
- **R8** Everything that enters a guest is versioned and stamped (§5.1).

---

## 4. Concrete re-layout of the guest scripts

### 4.1 `node-initscript.sh` — dispatcher, not worker

```bash
phase="$1"; [ -n "$phase" ] || phase=postcustomization
export VCD_STAGE_DIR=/var/lib/vcd-stage   # state: stage, rc, ts, render-hash
run_stage() {  # id, fn  — marker + precondition + soft failure
  ...
}
case "$phase" in
  precustomization)
      run_stage net  stage_net     # netplan 01-, remove 50-, apply, wait for link+route <=20 s
      run_stage seed stage_seed    # meta-data + user-data, fsync        <-- moved from post
      ;;                           # exit 0, no other side effects
  postcustomization)
      run_stage seed stage_seed    # cheap re-check: if pre was skipped, converge here
      run_stage disks stage_disks  # LVM + rsync + fstab  (the reboot pays for this)
      run_stage epoch stage_epoch  # instance-id epoch + cloud-init clean
      ;;
esac
exit 0
```

Changes vs. today:

- **seed moved to `pre`** (and idempotently re-checked in `post`). It costs nothing, needs no
  network, and means a VM that stalls in vCD's step at S6 still carries a valid seed for the boot
  that eventually happens. Today a stall before `post` leaves a guest with no seed at all.
- **`cloud-init clean` becomes a consequence, not a mechanism.** The instance-id carries a content
  hash (§5.2), so the seed re-runs because it *is* a different instance. `clean` stays as a belt.
  This removes the open risk from `project_issues.md` §4/F3: if `cloud-init status` reads `not run`
  and vCD skips the reboot, we still do not depend on `clean` having worked.
- **per-stage state file** `/var/lib/vcd-stage/state` (`stage rc ts render-hash`) — three lines per
  stage, readable over ssh, and enough to tell "vCD stalled" from "disk move failed" from "join
  failed" without a tap listener.

### 4.2 `worker-userdata.yaml` — declarative only

Keep: users, ssh keys, netplan file, module/sysctl/containerd config, join-command file, unit files.
Move out of `write_files` inline blobs: the prereqs and join logic become two units so that the
boundary is observable and retryable independently:

| Unit | `After=` / condition | Work | Why separate |
|---|---|---|---|
| `k8s-node-prereqs.service` | `network-online`, `Before=containerd kubelet` | swapoff, modules, sysctl, containerd, kubeadm/kubelet apt + hold | slow and network-dependent; may legitimately retry for 20 min without touching the cluster |
| `k8s-node-join.service` | `After=prereqs`, `Requires=prereqs`, `ConditionPathExists=!/etc/kubernetes/kubeadm-joined` | wait for `:6443/healthz`, layout gate (R7), `kubeadm join`, marker | the only step that mutates cluster state; a retry loop here should never re-install packages |

`bootcmd` keeps exactly two lines: disk retry (unguarded, as now) and `enable --now prereqs`.

### 4.3 What must *not* go into a vCD phase

- package installation or any apt/dpkg call (a 20 min apt run inside a phase with a 45 min systemd
  timeout is exactly how the token outlives its TTL);
- `kubeadm join` (network + API + token; must be retryable);
- `cloud-init init/modules` (the Terraform script does this because it has no second boot; we do, and
  running it before the disk move is actively wrong — containerd would advertise `/var/lib` paths
  that only exist after the reboot);
- any `reboot` (R1);
- unconditional `rm`/`mkfs` (R3).

---

## 5. Reproducibility mechanics

### 5.1 Stamp the render into the guest (highest value per line of code)

Right now the only way to answer "which build baked this VM?" is `strings manager2 | grep`. Add:

1. `renderVersion` constant + `sha256` over the three embedded templates, computed at startup.
2. The first line of the initscript and of user-data:
   `# vcd-nodescale render=<version> hash=<12 hex> spec=<12 hex>`.
3. VM metadata at creation (`scaleUp` 517-521): `render-hash`, `template-href`, `token-id`,
   `attempt`.
4. Node label added by the controller after Ready: `k8s-nodescale.github.io/render-hash=<hash>`.
5. `--render-only` flag (or `hack/render`) that prints user-data + initscript for a pool spec, with
   the generated output committed as golden files in `internal/cloudinit/testdata/`. Then a template
   change shows up as a reviewable diff, and `renderer_test.go` becomes a change-detector instead of
   a substring grep.

Consequence, and the real payoff: an adopted VM whose `render-hash` differs from the running build,
and which is **not** joined, is provably broken rather than "slow" — the watchdog (§6) can replace
it automatically instead of a human doing `hack/rmvm`.

### 5.2 Instance-id from content

```
meta-data:
  instance-id: {{ .NodeName }}-{{ .SpecHash8 }}
  local-hostname: {{ .NodeName }}
```

`SpecHash` = hash over user-data *without* secrets (exclude the join command token). Properties:

- identical spec ⇒ identical instance-id ⇒ cloud-init correctly no-ops (idempotent boot);
- changed spec ⇒ new instance ⇒ cloud-init runs again (converges, no `clean` needed);
- the token is excluded, so a re-created VM with a new token does not accidentally become the "same"
  instance — the node name plus ordinal already differ in that case.

### 5.3 Pin the things that mutate silently

| Thing | Risk | Fix |
|---|---|---|
| catalog template `Ubuntu-24.04-ENG-Server` | a template update changes the guest (tools `13322` was load-bearing for the customizer) | record catalog + item + version + href in metadata; optionally fail if the href changed since the last create |
| image `:0.1.6` / `:latest` | `IfNotPresent` + mutable tag = unknown binary | pin by digest in the chart, publish the digest in the release notes |
| chart version | drifts from the image tag (happened: 0.1.1 in tfvars vs 0.1.5 running) | assert chart version == image tag in a startup log line |
| GitHub Actions `docker-publish` | broken since 0.1.2 (missing secrets) → local builds | fix secrets; then the digest is reproducible from a tag |

### 5.4 Token budget, and the leak

Measured/observed: VM create 8-14 min (worker 8-10), data disk ~3.5 min each, GC stall p95 unknown
(20+ min worst case), boot 2 min, prereqs 10 min, join 1-5 min. p95 total from token creation to
join ≈ 40 min; worst case ≈ 70-90 min.

- default `join.tokenTTL` should be **3h**, not 1h, with the arithmetic in the README;
- delete the token Secret + `jws-kubeconfig-<id>` on Ready **and** on any failed create.
  Today `scaleUp`'s `release()` (467-471) frees the IP but not the token, and the error path via
  `reserveUnlessGone` does not either — that is the "10 tokens in 3 min" leak, still present.
- record `token-id` in VM metadata so a janitor can find orphans.

### 5.5 Convergence proof without a cluster

- `bash -n` on every rendered script (already);
- run each stage twice in a container (`hack/stage-sim`): loop devices for `prepare-data-disks.sh`,
  assert the second run is a no-op and fstab has one entry per mount;
- assert R2 mechanically: grep the rendered initscript for `exit [1-9]` outside `run_stage`;
- assert R1 mechanically (already in `TestInitscriptIsPhaseAware`);
- assert the layout gate exists in the bootstrap script (R7 test).

---

## 6. Failure matrix — detection and automatic action

`vm.GetGuestCustomizationStatus()` (`govcd/vm.go` 405) returns `GC_PENDING`, `REBOOT_PENDING`,
`GC_FAILED`, `POST_GC_PENDING`, `GC_COMPLETE`. We never call it, so today every guest-side problem
looks identical: "no Node, phase Provisioning". Add it to `cloud.VM` and drive `status.nodes[].phase`:

| Observed | Meaning | Action |
|---|---|---|
| `GC_PENDING` < 3 min | customizer running | requeue |
| `GC_PENDING` > 10 min | S6 stall (F2) | surface `subPhase=CustomizationStalled`, alert; no auto-fix available (needs power-cycle; do it only with an attempt limit) |
| `GC_FAILED` | S5/S7 aborted | replace VM immediately (immutable render, nothing to repair) |
| `REBOOT_PENDING` | waiting for vCD's reboot | requeue with short interval |
| `GC_COMPLETE`, no Node < 15 min | boot/cloud-init/prep | `subPhase=Bootstrapping` |
| `GC_COMPLETE`, no Node > 20 min | join path failing, or vCD skipped the reboot | `subPhase=JoinFailed` + read the guest deadline; replace after `joinDeadline` |
| Node exists, not Ready > 15 min | kubelet/prereqs | `Joining`, no action |
| Node Ready | done | labels, taints, token cleanup |

The two automatic actions (`replace on GC_FAILED`, `replace on join deadline`) both need
`DeleteVM` (still `ErrNotImplemented`, provider.go 266) plus a per-node `attempts` counter with a
backoff, so a persistent bug does not become a create/delete loop. That is the P2 watchdog, and it is
the difference between "operator wakes up to a stuck pool" and "the pool heals".

---

## 7. Strategic option: remove the reboot dependency

The move of `/var` and `/var/lib` is the only reason a second boot is needed. It is also the source
of the half-migrated class of failures (rsync + fstab + reboot in an unretryable phase).

| | **A. move `/var`, `/var/lib`** (today) | **B. mount data disks, point the services at them** |
|---|---|---|
| Guest reboots | 2 (template first boot + vCD) | 1 (vCD's, and even that becomes optional) |
| Guest work in vCD phases | LVM + rsync `/var` (3 s measured, unbounded worst case) + fstab | LVM + fstab on an empty directory (no rsync) |
| Failure class | half-migrated `/var`, invisible until join | none: fstab mounts at boot; a missing mount fails the layout gate |
| Depends on | vCD performing the reboot | nothing in vCD beyond network + seed |
| Deviation | matches Ansible/Terraform layout exactly | containerd `root=` and kubelet `--root-dir` point at `/data/...`; `/var` sizes differ from the Terraform nodes |
| Migration | — | new workers only; existing Terraform workers keep layout A |

Mechanically, B is: attach data1 (10 GB) and data2 (30 GB), mount them at `/srv/var-log` and
`/srv/var-lib` (or one LV at `/data`), set `containerd root=/data/containerd`,
`kubelet --root-dir=/data/kubelet`, `journald Storage`/`SystemMaxUse` for logs, and let the first
boot mount them via fstab — no rsync, no reboot, cloud-init can run in the same boot exactly like
the proven Terraform script.

Recommendation: **ship 0.1.6 with A** (it is one boot away from green and A keeps parity with the
Terraform-built nodes), then evaluate B as v0.2 behind `spec.flavor.disks[].strategy: move|direct`.
B is the design that makes the whole pipeline single-boot and therefore reproducible by construction.

---

## 8. Work order

| # | Change | Size | Test |
|---|---|---|---|
| 1 | R2: soft-fail both vCD phases, per-stage state file | S | rendered-script grep + double-run |
| 2 | R7: layout gate before `kubeadm join` | S | bootstrap script unit test |
| 3 | §5.2 content-hash instance-id | S | renderer golden |
| 4 | §5.1 render hash in scripts, metadata, Node label, `--render` + golden files | M | golden diff |
| 5 | Split `k8s-node-prereqs` / `k8s-node-join` units | M | user-data YAML assertions |
| 6 | GC status in `cloud.VM` + `subPhase` in status | M | fake cloud test |
| 7 | Token cleanup on Ready **and** on create failure (the leak) | S | fake kube test |
| 8 | Watchdog + `DeleteVM` + `attempts` backoff | L | reconcile test with fake cloud |
| 9 | Option B decision (`strategy: direct`) | L | — |

Items 1-3 are the ones that would have prevented this week's three defects; 4-7 are what stop the
next incident from needing a tap listener and `hack/diagvm`.

---

## 9. Checklist for the next live run (0.1.6)

1. Fresh scratch VM, current templates, valid token — expect `GC_COMPLETE` **and a reboot** on the
   first attempt, and `/var/lib/vcd-stage/state` shows `net/seed/disks/epoch` all `rc=0`.
2. `journalctl -u k8s-node-prereqs -u k8s-node-join` — the boundary between "packages" and "join"
   must be visible; two units, not one.
3. Layout gate: `findmnt /var`, `findmnt /var/lib`, `df -h` **before** the node appears as Ready.
4. `kubectl get node <name> -o jsonpath='{.metadata.labels}'` contains the render hash.
5. Pool `status.nodes[].subPhase` walks `VMCreating → GuestCustomizing → Bootstrapping → Joining →
   Ready` instead of sitting in `Provisioning`.
6. `kubectl get secrets -n kube-system | grep bootstrap-token` returns to baseline after Ready.
7. Kill and re-create the same node once: same IP, same name, new token, clean join (proves the
   re-create path is not special-cased).

---

## 10. What the live run on 2026-10-21 changed

Ran the checklist of §9 against two scratch VMs (`diag-wrk-03`, same name/IP, created twice) with the
rendered `0.2.0` script, then shipped that render. What held and what the run added:

| §9 check | Result |
|---|---|
| 1. `GC_COMPLETE` + reboot, all stages `rc=0` | yes - `net/seed` in `pre`, `seed=skipped/disks/epoch` in `post`; state file moved to `/etc/vcd-stage/state` because `/var` is what the disk stage relocates |
| 2. two units, visible boundary | yes - `prereqs done` at 13:43:49, `join finished` at 13:43:50 |
| 3. layout gate before the Node is Ready | yes - caught one boot where `/var/lib` did not mount; after the repair, `/var` 9.8G on `data1_lv`, `/var/lib` 30G on `data2_lv` |
| 4. render hash in the Node labels | partial - the stamp is in both scripts and the VM metadata; labels are patched by the controller, which the scratch path does not run |
| 5. `status.nodes[].subPhase` | not yet - GC polling is still item 6 of the plan |
| 6. token count back to baseline after Ready | no - 9 live tokens on the cluster; revoke-on-failed-create is in the code, the Ready-path janitor is not |
| 7. re-create the same node | yes - same name and IP, new token, clean join |

### 10.1 Defects the run found (all fixed before the release)

1. **A healthy-looking boot with an empty `/var/lib`.** `/var` mounted from `data1_lv` while
   `var-lib.mount` stayed dead and `data2_lv` sat on `/data2`: no dpkg database, no cloud-init seed,
   prereqs failing with "are you root?". The fstab entry was correct; the mount simply did not happen on
   that boot. Fix: `vcd-ensure-layout.service` mounts each configured device where the spec says,
   ordered `Before=` both stage units, and `k8s-node-prereqs` gained the same `ExecStartPre` gate the join
   unit had - apt must not run on a guest whose layout is wrong, and the error must be readable.
2. **`lock_passwd` defaults to true.** Setting `passwd:` to a hash *locks* the account, so the operator
   cannot `sudo` on a guest that failed a stage. Now `lock_passwd: false`.
3. **`enable` without `--now`.** `k8s-node-join` was enabled during `runcmd`, i.e. after
   `multi-user.target` had already been reached, so the first boot never joined. Now
   `enable --now --no-block`.
4. **A token without its JWS entry hangs `kubeadm join` forever.** Reproduced: 4.5 minutes of
   `[preflight] Running pre-flight checks`, then `[discovery] Waiting for the cluster-info ConfigMap to
   receive a JWS signature` - no error, no exit, the unit stuck in `activating`. Two fixes: the
   publication is read back and retried (a lost `cluster-info` update used to be silent), and the join is
   bounded by `timeout 900` so the unit fails, retries and says so.
5. **State on the filesystem that the stage moves.** `/var/lib/vcd-stage` was written in `pre`, excluded
   from the `/var` copy (rsync `/lib` excludes `/var/lib`), and therefore lost. Now `/etc/vcd-stage`.

### 10.2 Measurement that decides the token TTL

Token creation to Node Ready on the scratch path: 2 min (VM create + GC + reboot) + ~2 min (packages) +
~1 min (join) = **~5 min** in the happy case. The failure cases are what set the budget: a join that
hangs costs the 15 min timeout, a GC that stalls costs whatever the poll allows. `join.tokenTTL` stays at
3h in the docs, but the real protection is deleting the token, which is still pending.
