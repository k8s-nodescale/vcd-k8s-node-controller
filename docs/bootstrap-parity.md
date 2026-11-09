# Bootstrap parity: Terraform + cloud-init + Ansible vs the node controller

What a worker receives before `kubeadm join`, and whether a controller-created
worker gets the same. Sources compared:

- Terraform user-data: `k8s-priv-bootstrap-tf-vsphere/templates/userdata_w.yaml`
- Ansible roles: `prepare_k8s_nodes`, `configure_lvm_and_fs`, `move_system_dir`,
  `install_k8s_binaries`, `add_custom_kernel_params`, `k8s_add_workers`
- Controller guest path: `internal/cloudinit` (`worker-userdata.yaml.tmpl`,
  `prepare-data-disks.sh.tmpl`, `ensure-layout.sh.tmpl`, `grow-*.sh.tmpl`)

Legend: **yes** - the controller does it; **partial** - done but with a
difference; **no** - missing (comes from the image today, or not delivered).

| # | Option | Where the baseline sets it | Controller | Note |
| --- | --- | --- | --- | --- |
| 1 | Account user, shell, sudo group | TF `users` | **yes** | `bootstrap.osUser`; users module |
| 2 | Account password | TF `passwd` | **yes** | `osPasswordHash`, `users` + `chpasswd` |
| 3 | Passwordless sudo | TF `write_files` sudoers | **yes** | `/etc/sudoers.d/<user>`, `NOPASSWD` |
| 4 | PAM access allow (`+:user:ALL` before `-:ALL:ALL`) | TF `runcmd` | **yes** | `bootcmd`; without it console and SSH deny every login |
| 5 | SSH password auth | TF `runcmd` | **yes** | `ssh_pwauth` + drop-in |
| 6 | SSH `AllowUsers` sources (`allowed_ssh_ips`) | TF `runcmd` | **yes** | `bootstrap.sshAllowedSources[]`, appended once each |
| 7 | SSH authorized key | TF `ssh_authorized_keys` | **yes** | `bootstrap.sshAuthorizedKey` |
| 8 | Hostname / FQDN | TF `hostname`, `fqdn`? (Ansible sets) | **yes** | `hostname`, `fqdn` |
| 9 | Static IP, prefix, gateway, DNS (netplan) | TF metadata + Ansible netplan | **yes** | `ipPlan.cidr` + `bootstrap.dns` |
| 10 | systemd-resolved DNS drop-in | Ansible `prepare_k8s_nodes` | **no** | netplan DNS only; resolved uses the same servers |
| 11 | `/etc/hosts` for peers | TF `write_files` + script | **yes** | `HostsLines` from Nodes/status |
| 12 | NTP servers (chrony) | TF `ntp.servers` | **yes** | `bootstrap.ntpServers[]` in a marked block of `chrony.conf`, then `systemctl restart chrony`; empty keeps the image's sources |
| 13 | Corporate CA certificates | TF `ca_certs` | **yes** | `bootstrap.caBundleRef` (ConfigMap/Secret PEM bundle, installation default in `nodescale-bootstrap`); one file per certificate + `update-ca-certificates` |
| 14 | Ubuntu apt mirror (Nexus) | TF `apt.sources_list` | **no** | image already points at the mirror |
| 15 | Base packages (net-tools, git, tree, sshpass, ntpdate) | TF `packages` | **no** | image carries them |
| 16 | Disable apt-daily timers / unattended upgrades | Ansible + TF | **yes** | `10periodic` + timer mask |
| 17 | Swap off + fstab | Ansible | **yes** | prereqs |
| 18 | Kernel modules overlay / br_netfilter | Ansible | **yes** | `/etc/modules-load.d/k8s.conf` |
| 19 | Kubernetes sysctls (forwarding, bridge-nf) | Ansible | **yes** | applied in prereqs |
| 20 | Custom kernel/security sysctls (`99-unixteam-sysctl.conf`) | Ansible `add_custom_kernel_params` | **yes** | `bootstrap.sysctls` -> `99-zzz-nodescale.conf` (sorts last, so it wins) + `sysctl --system`; `bootstrap.kernelModules[]` loaded first |
| 21 | Containerd package | Ansible `install_k8s_binaries` | **partial** | installed when missing; image normally has it |
| 22 | Containerd config (`SystemdCgroup=true`, sandbox image, mirrors) | image + Ansible | **yes** | base generated in the guest by `containerd config default`; site values in `/etc/containerd/conf.d/10-nodescale.toml`; `containerd.configRef` for a full file |
| 23 | `kubelet`, `kubeadm`, `kubectl` pinned | Ansible `install_k8s_binaries` | **yes** | pinned from `bootstrap.k8sVersion`, `apt-mark hold` |
| 24 | Kubernetes apt repo + key (`pkgs.k8s.io`) | Ansible | **yes** | prereqs |
| 25 | `cri-tools` + `/etc/crictl.yaml` | Ansible | **no** | image carries both |
| 26 | PV/VG/LV on data disks | Ansible `configure_lvm_and_fs` | **yes** | `prepare-data-disks.sh` |
| 27 | Copy `/var` and `/var/lib` into the new volumes | Ansible `move_system_dir` | **yes** | rsync before fstab, marker guarded |
| 28 | fstab by UUID, nested mount dirs | Ansible | **yes** | `write_fstab`, excludes preserved |
| 29 | Reboot after the move | Ansible (`Reboot machine`) | **yes** | one reboot from runcmd |
| 30 | Layout verification before join | Ansible asserts mounts | **yes** | `verify-layout.sh` pre-join gate |
| 31 | Grow `/var`, `/var/lib` after a resize | Ansible re-run | **yes** | `grow-data-disks` unit |
| 32 | Join token, CA hash, endpoint | Ansible `k8s_add_workers` | **yes** | controller mints short-lived token and reads CA |
| 33 | `kubeadm join` | Ansible | **yes** | join unit with retries and a 15 min bound |
| 34 | Worker label `node-role.kubernetes.io/worker` | Ansible | **yes** | `join.labels`, applied once Ready |
| 35 | Taints | Ansible labels/taints | **yes** | `join.taints` |
| 36 | CNI on the new node | DaemonSet in the cluster | **yes** | calico/tigera DaemonSet |
| 37 | Wait until Node Ready, then add to the LB pool | Ansible | **yes** | Ready gate; LB membership on vCD only |
| 38 | `local` service account created by TF users | TF `users: - name: local` | **no** | deliberate: not an operator account |

## Remaining gaps

1. **`cri-tools`/`crictl.yaml`**, base packages and the apt mirror are inherited
   from the image; an image without them would need the controller to install
   them.
2. **systemd-resolved drop-in** (row 10): netplan DNS only; resolved uses the
   same servers through the netplan file.
3. **`local` service account** (row 38) is deliberately not created.

NTP, the CA bundle, the custom sysctls and the containerd configuration are no
longer gaps: they are installation policy in the `nodescale-bootstrap`
ConfigMap, overridable per pool, and written into the guest by cloud-init (see
"Guest policy" in the README).

## Verified end to end (lab, 2026-11-08)

`K8sWorkerPool` replicas 3 -> 4 on vSphere: VM cloned on the cluster's host,
40/10/30 GB disks, static `.112`, data volumes mounted (`/var` 10 G,
`/var/lib` 30 G, `dpkg` present), packages installed and held, CSR approved,
`join finished`, Node `Ready` in 2m48s, pool `Converged 4/4`.

## Verified directly in the guest (lab, 2026-11-09)

The vSphere account was rejected on this date, so no VM could be cloned; the
rendered guest half was exercised on the existing nodes instead, which is where
the parity gaps live:

| Check | `wrk-01` (containerd 1.7.12) | `wrk-04` (containerd 2.2.1) |
| --- | --- | --- |
| `containerd config.toml` absent -> generated from `containerd config default` | version 2 | version 3 |
| `imports` patched once, drop-in loaded | yes | yes |
| layout of the drop-in | `io.containerd.grpc.v1.cri` | `io.containerd.cri.v1.runtime` / `cri.v1.images` |
| `containerd config dump`: `SystemdCgroup = true` | yes | yes |
| `dump`: pause image from `bootstrap.containerd.sandboxImage` | yes | yes |
| `dump`: mirror endpoint (1.x `registry.mirrors`, 2.x `certs.d` hosts.toml for TLS) | yes | hosts.toml written |
| legacy version-2 base on a 2.2.1 binary (image-supplied config) still applies the 1.x drop-in | - | yes |
| `sysctl --system` from `99-zzz-nodescale.conf` | `nf_conntrack_max = 2621440`, `somaxconn = 4096` | - |
| chrony marked block and `chronyc sources` | both servers listed | - |
| CA bundle: one file per certificate + `update-ca-certificates` | `openssl verify` against the system bundle: OK | - |

