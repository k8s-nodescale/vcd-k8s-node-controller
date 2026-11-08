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
| 12 | NTP servers (chrony) | TF `ntp.servers` | **no** | image ships chrony with the site servers |
| 13 | Corporate CA certificates | TF `ca_certs` | **no** | image carries them; not injected per worker |
| 14 | Ubuntu apt mirror (Nexus) | TF `apt.sources_list` | **no** | image already points at the mirror |
| 15 | Base packages (net-tools, git, tree, sshpass, ntpdate) | TF `packages` | **no** | image carries them |
| 16 | Disable apt-daily timers / unattended upgrades | Ansible + TF | **yes** | `10periodic` + timer mask |
| 17 | Swap off + fstab | Ansible | **yes** | prereqs |
| 18 | Kernel modules overlay / br_netfilter | Ansible | **yes** | `/etc/modules-load.d/k8s.conf` |
| 19 | Kubernetes sysctls (forwarding, bridge-nf) | Ansible | **yes** | applied in prereqs |
| 20 | Custom kernel/security sysctls (`99-unixteam-sysctl.conf`) | Ansible `add_custom_kernel_params` | **no** | controller writes only its own k8s list |
| 21 | Containerd package | Ansible `install_k8s_binaries` | **partial** | installed when missing; image normally has it |
| 22 | Containerd config (`SystemdCgroup=true`, sandbox image) | image + Ansible | **partial** | controller does not write `config.toml`; relies on the image |
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

## Known gaps to decide on

1. **NTP and corporate CAs** are the two items that a controller-made worker
   currently inherits only from the image. They are environment policy, and the
   Terraform baseline sets them per cluster.
2. **Containerd config** is inherited from the image. If the image ever ships
   the Ubuntu default (`SystemdCgroup=false`), a worker joins with the wrong
   cgroup driver; writing the file would make the controller independent of the
   image.
3. **Custom security sysctls** (`99-unixteam-sysctl.conf`) are not applied.
4. **`cri-tools`/`crictl.yaml`**, base packages and the apt mirror are inherited
   from the image; an image without them would need the controller to install
   them.

## Verified end to end (lab, 2026-11-08)

`K8sWorkerPool` replicas 3 -> 4 on vSphere: VM cloned on the cluster's host,
40/10/30 GB disks, static `.112`, data volumes mounted (`/var` 10 G,
`/var/lib` 30 G, `dpkg` present), packages installed and held, CSR approved,
`join finished`, Node `Ready` in 2m48s, pool `Converged 4/4`.
