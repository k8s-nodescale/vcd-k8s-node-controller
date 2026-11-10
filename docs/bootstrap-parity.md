# Bootstrap parity: Terraform + cloud-init + Ansible vs the node controller

What a worker receives before `kubeadm join`, and whether a controller-created
worker gets the same. Sources compared, item by item:

- Terraform user-data: `k8s-priv-bootstrap-tf-vsphere/templates/userdata_w.yaml`
  (`users`, `ntp`, `ca_certs`, `apt`, `packages`, `write_files`, `runcmd`)
- Ansible roles: `prepare_k8s_nodes` (incl. `prepare_hosts.yaml`),
  `install_k8s_binaries`, `add_custom_kernel_params`, `configure_lvm_and_fs`,
  `move_system_dir`, `k8s_add_workers`
- Controller guest path: `internal/cloudinit` (`worker-userdata.yaml.tmpl`,
  `prepare-data-disks.sh.tmpl`, `ensure-layout.sh.tmpl`, `grow-*.sh.tmpl`) plus
  the guest policy resolved in `internal/controller/bootstrap.go`

Legend: **yes** - the controller does it, with the pool or installation policy
naming the site values; **partial** - done, with a stated difference;
**no** - not done by the controller; **n/a** - the baseline step has no meaning
for a worker on this path.

## Account and access

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 1 | Operator account, shell, `sudo` group | TF `users` | **yes** | `bootstrap.osUser`, `users` module |
| 2 | Account password, `lock_passwd: false` | TF `passwd`, `lock_passwd` | **yes** | `bootstrap.osPasswordHash`; `users` + `chpasswd` (an image that already has the account keeps its own hash otherwise) |
| 3 | Passwordless sudo | TF `/etc/sudoers.d/<user>` | **yes** | `/etc/sudoers.d/<user>`, `NOPASSWD: ALL`, mode 0440 |
| 4 | `Defaults:<user> !requiretty` | TF `/etc/sudoers.d/<user>` | **yes** | same file |
| 5 | L2 support group sudoers (`%0000-dl-unixauth-l2paas`) | TF `/etc/sudoers.d/0000-dl-unixauth-l2paas` | **no** | group comes from AD/the image; a cluster-wide group policy is not per-pool |
| 6 | `local` service account | TF `users: - name: local` | **no** | deliberate: a service account with a password is not an operator account |
| 7 | PAM `access.conf` `+:<user>:ALL` ahead of `-:ALL:ALL` | TF `runcmd` | **yes** | `bootcmd`; without it console and SSH deny every login |
| 8 | PAM `access.conf` for the L2 group | TF `runcmd` | **no** | inherited from the image |
| 9 | SSH password authentication | TF `runcmd` + `ssh_pwauth` | **yes** | `ssh_pwauth: true` + drop-in |
| 10 | sshd drop-in (pubkey, password, kbd-interactive, no root) | TF `runcmd` (sed on `sshd_config.d`) | **yes** | `/etc/ssh/sshd_config.d/01-k8s-auth.conf`, mode 0600 |
| 11 | sshd `AllowUsers` sources (`allowed_ssh_ips`) | TF `runcmd` | **yes** | `bootstrap.sshAllowedSources[]`, each appended once |
| 12 | SSH authorized key | TF `ssh_authorized_keys` | **yes** | `bootstrap.sshAuthorizedKey` |
| 13 | `systemctl restart sshd` | TF `runcmd` | **partial** | no restart in early boot: the drop-in is read by the next connection, and sshd is started by the image long after cloud-init wrote it |
| 14 | `sssd` restart | TF `runcmd` | **n/a** | no sssd in this path |

## Network, DNS, hosts, time

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 15 | Static address, prefix, gateway (netplan) | TF metadata + Ansible netplan | **yes** | `ipPlan.cidr` + `bootstrap.nicName` |
| 16 | netplan DNS servers | Ansible `search netplan file ... change dns server` | **yes** | `bootstrap.dns` |
| 17 | systemd-resolved drop-in (`DNS=`, `Domains=~.`) | Ansible `configure systemd-resolved DNS drop-in` | **yes** | `/etc/systemd/resolved.conf.d/nodescale.conf` + `systemctl restart systemd-resolved` |
| 18 | `netplan apply` after the DNS change | Ansible | **yes** | cloud-init applies the only netplan file it wrote; nothing to re-apply |
| 19 | `/etc/hosts` peer records | TF `write_files` (`append: true`) | **yes** | `HostsLines` from the pool's workers and the cluster's Nodes; append, not replace |
| 20 | `/add_hosts_records.sh` computed peers | TF `runcmd` | **yes** | same data, resolved in the controller instead of a shell loop |
| 21 | NTP servers (chrony) | TF `ntp.servers` | **yes** | `bootstrap.ntpServers[]` in a marked block of `chrony.conf` |
| 22 | `systemctl restart chrony` | cloud-init `ntp` module | **yes** | after the block is written; empty list keeps the image's sources |

## Package sources, apt policy, packages

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 23 | Mirror as the only apt source (deb822) | TF `apt.sources_list` | **yes** | `bootstrap.apt.mirror` -> `/etc/apt/sources.list.d/nodescale.sources`, written before the first apt call |
| 24 | Suites `<code> <code>-updates <code>-backports <code>-security` | TF `apt.sources_list` | **yes** | derived from the guest's `VERSION_CODENAME`; `bootstrap.apt.suites` overrides |
| 25 | Components `main universe restricted multiverse` | TF `apt.sources_list` | **yes** | `bootstrap.apt.components` overrides |
| 26 | The image's own sources taken out of the way | implied by `sources_list` | **yes** | `*.sources`/`*.list` get `.disabled-by-nodescale`, `deb` lines in `sources.list` commented; `kubernetes.list` is kept |
| 27 | `Acquire { Retries "60" }` | TF `apt.conf` | **yes** | cloud-config `apt.conf`, so it is in place for every apt call including cloud-init's own |
| 28 | `DPkg { Lock { Timeout "60" } }` | TF `apt.conf` | **yes** | same block |
| 29 | `package_update: true` | TF | **yes** | the first `apt-get update` after the mirror, and the `apt-get update` before the Kubernetes repo |
| 30 | `packages_upgrade: true` | TF | **no** | deliberate: upgrading the image's 300 packages on a worker makes boot time and the guest's package state unpredictable; the image is the version baseline |
| 31 | Base packages (`net-tools`, `ntpdate`, `git`, `tree`, `openssh-server`, `sshpass`) | TF `packages` | **no** | the image carries them; a worker installs only what `kubeadm` needs |
| 32 | `cri-tools` + `/etc/crictl.yaml` | Ansible `install_k8s_binaries` | **no** | the image carries both; `kubelet` does not need crictl to join |
| 33 | Disable / mask apt-daily timers | Ansible | **partial** | the timers are not masked; `10periodic` sets every periodic action to `0`, so they fire and do nothing |
| 34 | `/etc/apt/apt.conf.d/10periodic` disables auto updates | Ansible | **yes** | written by the prereqs script, because on this image it is a conffile of the Pro packages purged just before |
| 35 | Purge `unattended-upgrades` / Pro clients, drop the ESM hook | TF + Ansible | **yes** | plus `Prompt=never` and the release-upgrader marker |
| 36 | `apt-mark hold` on `kubelet`/`kubeadm`/`kubectl` | Ansible | **yes** | after the pinned install |

## Kernel, runtime, node directories

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 37 | `swapoff -a` and commented swap in fstab | Ansible | **yes** | prereqs, before `kubelet` starts |
| 38 | `overlay`, `br_netfilter` in `modules-load.d` | Ansible `load.d_k8s.conf` | **yes** | `/etc/modules-load.d/k8s.conf` |
| 39 | Extra kernel modules | Ansible `add_custom_kernel_params` | **yes** | `bootstrap.kernelModules[]` -> `modules-load.d/nodescale.conf`, loaded before the sysctls |
| 40 | Kubernetes sysctls (forwarding, `bridge-nf-call-*`) | Ansible `sysctl.d_k8s.conf` | **yes** | `/etc/sysctl.d/k8s.conf` |
| 41 | Custom kernel/security sysctls (`99-unixteam-sysctl.conf`) | Ansible `add_custom_kernel_params` | **yes** | `bootstrap.sysctls` -> `99-zzz-nodescale.conf`, which sorts last so it wins |
| 42 | `sysctl --system` | Ansible | **yes** | after the module loads |
| 43 | Disable Ubuntu's apparmor profile for `runc` | Ansible `disable ubuntu's default apparmor runc profile` | **yes** | symlink into `/etc/apparmor.d/disable` + `apparmor_parser -R`; guarded, an image without apparmor is not an error |
| 44 | containerd package | Ansible `install_k8s_binaries` | **partial** | installed only when the image has no containerd (with `conntrack`, `socat`, `cloud-guest-utils`) |
| 45 | containerd config, version-matched | Ansible `containerd_config_*.toml` | **yes** | base generated in the guest by `containerd config default`, so 1.x and 2.x get their own key layout |
| 46 | Site values in a drop-in (`SystemdCgroup`, sandbox image, mirrors) | Ansible template | **yes** | `/etc/containerd/conf.d/10-nodescale.toml`, `imports` patched once, layout detected 1.x vs 2.x |
| 47 | Registry TLS (`certs.d/<host>/hosts.toml`) | Ansible `Prepare containerd specific registry configs` | **yes** | for 2.x; 1.x uses `registry.mirrors` in the drop-in |
| 48 | `containerd.configRef` for a full file | Ansible template | **yes** | written verbatim to `/etc/containerd/config.toml`, no drop-in |
| 49 | `systemctl enable/restart containerd` | Ansible | **yes** | plus a `config dump` check that logs when `SystemdCgroup` is not true |
| 50 | `/etc/kubernetes/policies/audit-policy.yaml` | Ansible `Copy Audit Policy` | **no** | read by `kube-apiserver`; a kubelet does not have an audit policy |
| 51 | `/etc/kubernetes/pki/kc-ca.pem` | Ansible `copy oidc CA file` | **no** | used by the API server / OIDC kubeconfigs, not by a joining worker |
| 52 | Kubernetes apt key + `pkgs.k8s.io` repo | Ansible | **yes** | prereqs, before the pinned install |
| 53 | `kubelet`/`kubeadm`/`kubectl` pinned to `bootstrap.k8sVersion` | Ansible | **yes** | `apt-get install kubelet=<version> ...`; unset version takes the repo's current |
| 54 | `systemctl enable kubelet` | Ansible | **yes** | before the join unit |

## Storage

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 55 | PV/VG/LV on each data disk | Ansible `configure_lvm_and_fs` | **yes** | `prepare-data-disks.sh`, `spec.flavor.disks` |
| 56 | Filesystem creation | Ansible | **yes** | `mkfs` by `spec.flavor.disks[].fstype` |
| 57 | Copy `/var` and `/var/lib` into the new volumes | Ansible `move_system_dir` | **yes** | rsync, with the parent's exclude list for nested mounts |
| 58 | fstab by UUID, nested mount directories | Ansible | **yes** | `write_fstab`, `/lib` excluded from the `/var` copy |
| 59 | Reboot after the move | Ansible `Reboot machine` | **yes** | one reboot from `runcmd`, marker guarded |
| 60 | Layout verification before join | Ansible asserts mounts | **yes** | `verify-layout.sh` pre-join gate |
| 61 | Grow the filesystems after a resize | Ansible re-run | **yes** | `vcd-grow-disks.service`, runs on every later boot |

## Join and after

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 62 | Join token, CA hash, endpoint | Ansible `k8s_add_workers` | **yes** | short-lived token minted per worker, CA hash read from the cluster |
| 63 | `kubeadm join` | Ansible `join new workers nodes` | **yes** | join unit, retried, bounded at 15 min |
| 64 | Ready wait | Ansible `get new workers-nodes ready status` | **yes** | Ready gate with `rollout.readyTimeout` |
| 65 | Worker label `node-role.kubernetes.io/worker` | Ansible | **yes** | `join.labels`, applied once Ready |
| 66 | Taints | Ansible labels/taints | **yes** | `join.taints` |
| 67 | CNI on the new node | DaemonSet in the cluster | **yes** | calico/tigera DaemonSet |
| 68 | Add the node to the LB pool | Ansible (vCD) | **n/a** | no LB member management on the vSphere path |

## What is deliberately not copied

1. **`packages_upgrade`** (row 30) and the **base package list** (row 31):
   a worker's package state is the image's, and upgrading it on every boot
   makes boot time and the guest's state a moving target. The image is the
   version baseline; `containerd` and the Kubernetes binaries are the only
   packages the controller installs.
2. **`cri-tools`** (row 32) and the L2 group's sudoers/`access.conf` entries
   (rows 5, 8): inherited from the image, not per-cluster policy.
3. **`local` service account** (row 6): a password-bearing service account is
   not an operator account.
4. **Audit policy and the OIDC CA** (rows 50, 51): control-plane inputs.
5. **apt-daily timer masking** (row 33): the timers are left running but
   neutralised by `10periodic`; masking them as well would be a second place to
   look when a future image no longer ships them.

## Where the site values come from

Everything the baseline reads from `terraform.tfvars` is either a pool field or
installation policy:

| Baseline variable | Controller |
| --- | --- |
| `vm_user_name`, `vm_user_password`, `vm_user_ssh_key` | `spec.bootstrap.osUser`, `osPasswordHash`, `sshAuthorizedKey` |
| `ssh_ips` | `spec.bootstrap.sshAllowedSources` |
| `ntp.servers` | `spec.bootstrap.ntpServers`, default in `bootstrap.yaml` |
| `ca_certs.trusted` | `spec.bootstrap.caBundleRef` / chart `bootstrap.caBundle` |
| `apt.sources_list` | `spec.bootstrap.apt` (`mirror`, `suites`, `components`), default in `bootstrap.yaml` |
| `apt.conf` | written unconditionally, not configurable |
| `packages`, `packages_upgrade` | not copied (see above) |
| `99-unixteam-sysctl.conf` | `spec.bootstrap.sysctls` |
| `load.d_k8s.conf` extra modules | `spec.bootstrap.kernelModules` |
| containerd template | `spec.bootstrap.containerd` (`sandboxImage`, `mirrors`, `insecureSkipVerify`, `configRef`) |
| `master0_ip`, `workers_count` for `/etc/hosts` | derived from `spec.join.endpoint` and `status.nodes` |
| `k8s_version`, `k8s_minor_repo` | `spec.bootstrap.k8sVersion`, `k8sMinorRepo` |

## Verified end to end

**2026-11-08.** `K8sWorkerPool` replicas 3 -> 4 on vSphere: VM cloned on the
cluster's host, 40/10/30 GB disks, static `.112`, data volumes mounted (`/var`
10 G, `/var/lib` 30 G, `dpkg` present), packages installed and held, CSR
approved, `join finished`, Node `Ready` in 2m48s, pool `Converged 4/4`.

**2026-11-09 (guest half, on existing nodes).** The vSphere account was rejected
on this date, so no VM could be cloned; the rendered guest was exercised on the
existing workers:

| Check | `wrk-01` (containerd 1.7.12) | `wrk-04` (containerd 2.2.1) |
| --- | --- | --- |
| `config.toml` generated by `containerd config default` | version 2 | version 3 |
| `imports` patched once, drop-in loaded | yes | yes |
| drop-in layout | `io.containerd.grpc.v1.cri` | `io.containerd.cri.v1.runtime` / `cri.v1.images` |
| `config dump`: `SystemdCgroup = true` | yes | yes |
| `dump`: pause image from `bootstrap.containerd.sandboxImage` | yes | yes |
| `dump`: mirror endpoint (1.x `registry.mirrors`, 2.x `hosts.toml`) | yes | hosts.toml written |
| legacy version-2 base on a 2.2.1 binary still applies the 1.x drop-in | - | yes |
| `sysctl --system` from `99-zzz-nodescale.conf` | `nf_conntrack_max = 2621440`, `somaxconn = 4096` | - |
| chrony marked block and `chronyc sources` | both servers listed | - |
| CA bundle: one file per certificate + `update-ca-certificates` | `openssl verify` against the system bundle: OK | - |
| apt sources: image's `ubuntu.sources` at the Nexus mirror | yes | yes |
| apparmor `runc` profile / resolved drop-in | absent on the 1.1.1 build | absent on the 1.1.1 build |

The last row is what 1.2.0 closes: those two steps are written by the current
template and are verified on a freshly cloned worker (see the next section).

**2026-11-10 (fresh worker, 1.2.0).** Filled in by the scale matrix of this
release - see the release notes.
