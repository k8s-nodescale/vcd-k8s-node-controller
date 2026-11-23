#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.7.1 hash=0cbfab47a31a
#
# vCD runs this twice with the phase in $1: preCustomization (before vCD sets
# hostname/network/credentials) and postCustomization (just before vCD's
# reboot). Contract, see customization-phases.md R1-R4: never reboot, never
# exit non-zero (a failing phase is terminal for the VM: "Customization.pm
# line 637" / GC_FAILED), every stage idempotent and recorded in
# /etc/vcd-stage/state, and only work that makes the next boot possible.
set -uo pipefail   # deliberately no -e: a failing stage must not abort the phase

PHASE="${1:-postCustomization}"
LOG=/var/log/vcd-cloudinit.log
# On the root filesystem, not /var/lib: the disk stage moves /var between the
# pre and post phases, and state kept there would be copied away mid-run.
STAGE_DIR=/etc/vcd-stage
mkdir -p "$STAGE_DIR"
exec >>"$LOG" 2>&1

log() { echo "[$(date -Is)] $*"; }
log "initscript phase=$PHASE render=0.7.1 hash=0cbfab47a31a"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="4df6500e"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.7.1 hash=0cbfab47a31a" >>"$STAGE_DIR/state"
}

# run_stage NAME FUNCTION
# Skips a stage whose marker exists, runs it otherwise, records the result and
# always returns 0 so the phase keeps going (R2).
run_stage() {
  local name="$1" fn="$2" rc=0
  if [[ -f "$STAGE_DIR/$name.done" ]]; then
    log "stage $name: already done"
    record "$name" skipped
    return 0
  fi
  log "stage $name: start"
  "$fn" || rc=$?
  if [[ "$rc" -eq 0 ]]; then
    touch "$STAGE_DIR/$name.done"
    log "stage $name: ok"
  else
    log "stage $name: FAILED rc=$rc; the phase continues, the next boot retries"
  fi
  record "$name" "$rc"
  return 0
}

stage_net() {
  cat > /etc/netplan/01-netplan.yaml <<EOF
network:
  version: 2
  renderer: networkd
  ethernets:
    $NIC:
      dhcp4: false
      dhcp6: false
      addresses:
        - $NODE_IP/$CIDR_PREFIX
      routes:
        - to: default
          via: $GATEWAY
      nameservers:
        addresses: $DNS_JSON
EOF
  chmod 0600 /etc/netplan/01-netplan.yaml
  rm -f /etc/netplan/50-cloud-init.yaml || true
  netplan apply || log "netplan apply returned non-zero"

  # vCD applies its own network settings right after this phase and stalls when
  # the guest has no working network (observed: GC_PENDING for 20+ minutes).
  # Wait for the address and the default route, but never reboot here: a reboot
  # in this phase is what produced "Customization.pm line 637".
  local i
  for i in $(seq 1 20); do
    if ip -4 addr show dev "$NIC" 2>/dev/null | grep -q "inet $NODE_IP/" \
       && ip route 2>/dev/null | grep -q '^default'; then
      log "network up: $NODE_IP/$CIDR_PREFIX via $GATEWAY"
      return 0
    fi
    sleep 1
  done
  log "network did not come up within 20 s on $NIC"
  return 1
}

stage_seed() {
  mkdir -p /var/lib/cloud/seed/nocloud
  cat > /var/lib/cloud/seed/nocloud/meta-data <<EOF
instance-id: $NODE_NAME-$SPEC_HASH
local-hostname: $NODE_NAME
EOF
  base64 -d <<'CLOUDINIT' | gzip -d > /var/lib/cloud/seed/nocloud/user-data
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
CLOUDINIT
  sync
  log "NoCloud seed written: instance-id $NODE_NAME-$SPEC_HASH"
}

stage_disks() {
  cat > /usr/local/sbin/prepare-data-disks.sh <<'DISKSETUP'
#!/usr/bin/env bash
# Option A data-disk preparation, mirroring the Ansible prepare_k8s_nodes role:
#   LVM on the data disks -> temporary mounts -> move /var and /var/lib ->
#   rewrite fstab by UUID -> the guest is rebooted by vCD afterwards.
#
# The script never reboots: it runs during the vCD post-customization phase and
# vCD power-cycles the guest once the phase returns. A marker on the root
# filesystem keeps it idempotent, and LVM/filesystem creation is skipped when
# it already exists.
set -euo pipefail

MARKER=/etc/vcd-data-disks.done
LOG=/root/vcd-data-disks.log
exec >>"$LOG" 2>&1

log() {
  echo "[$(date -Is)] $*"
}

if [[ -f "$MARKER" ]]; then
  log "data disks already prepared; nothing to do"
  exit 0
fi

for tool in pvcreate vgcreate lvcreate pvs vgs lvs mkfs blkid rsync blockdev mountpoint; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    log "missing required tool: $tool"
    exit 1
  fi
done

wait_for_device() {
  local dev="$1" size_gb="$2" i bytes expected
  expected=$((size_gb * 1024 * 1024 * 1024))
  for i in $(seq 1 60); do
    if [[ -b "$dev" ]]; then
      bytes=$(blockdev --getsize64 "$dev")
      # Allow 1 GiB tolerance for controller/geometry rounding.
      if (( bytes >= expected - 1073741824 && bytes <= expected + 1073741824 )); then
        return 0
      fi
      log "device $dev has unexpected size $bytes (expected about $expected)"
      return 1
    fi
    sleep 2
  done
  log "timed out waiting for $dev"
  return 1
}

prepare_disk() {
  local dev="$1" vg="$2" lv="$3" temp="$4" fs="$5" size_gb="$6"
  local lvdev existing_fs

  wait_for_device "$dev" "$size_gb"

  if pvs --noheadings -o pv_name "$dev" 2>/dev/null | grep -qw "$dev"; then
    log "$dev is already an LVM physical volume"
  else
    # Never overwrite a disk that already carries a filesystem or partition
    # table. A blank data disk has no signature.
    if blkid -p "$dev" >/dev/null 2>&1; then
      log "$dev already has a filesystem/partition signature; refusing to overwrite"
      return 1
    fi
    log "creating physical volume on $dev"
    pvcreate -ff -y "$dev"
  fi

  if ! vgs "$vg" >/dev/null 2>&1; then
    log "creating volume group $vg"
    vgcreate "$vg" "$dev"
  fi

  if ! lvs "$vg/$lv" >/dev/null 2>&1; then
    log "creating logical volume $vg/$lv"
    lvcreate -l 100%FREE -n "$lv" "$vg"
  fi

  lvdev=$(readlink -f "/dev/${vg}/${lv}")
  existing_fs=$(blkid -p -s TYPE -o value "$lvdev" 2>/dev/null || true)
  if [[ -n "$existing_fs" && "$existing_fs" != "$fs" ]]; then
    log "$lvdev already contains filesystem $existing_fs, expected $fs; refusing to format"
    return 1
  fi
  if [[ -z "$existing_fs" ]]; then
    log "formatting $lvdev as $fs"
    mkfs -t "$fs" "$lvdev"
  fi

  mkdir -p "$temp"
  if ! mountpoint -q "$temp"; then
    log "mounting $lvdev at $temp"
    mount "$lvdev" "$temp"
  fi
}

sync_data() {
  local src="$1" dst="$2"
  shift 2
  local excludes=() e

  if [[ ! -d "$src" ]]; then
    log "source $src does not exist; skipping copy"
    return 0
  fi
  # Skip only when the target volume already carries the data. The source being
  # a mount point is not enough: on a controller-made worker /var or /var/lib
  # can be mounted when this runs, and skipping there left the new volume empty
  # (no /var/lib/dpkg, apt then failed with "Unable to acquire the dpkg frontend
  # lock").
  if [[ -n "$(ls -A "$dst" 2>/dev/null | grep -v '^lost+found$')" ]]; then
    log "$dst already contains data; skipping copy of $src"
    return 0
  fi
  if [[ -z "$(ls -A "$src" 2>/dev/null)" ]]; then
    log "source $src is empty; skipping copy"
    return 0
  fi

  for e in "$@"; do
    excludes+=("--exclude=$e")
  done
  log "copying $src -> $dst (excludes: ${*:-none})"
  rsync -aHAX --one-file-system "${excludes[@]}" "$src"/ "$dst"/

  # Keep excluded child mount points present under the parent so the later
  # nested mount (e.g. /var/lib on data2) has a directory to mount over.
  for e in "$@"; do
    mkdir -p "$dst$e"
  done
}

write_fstab() {
  local final="$1" temp="$2" vg="$3" lv="$4" fs="$5"
  local lvdev uuid

  lvdev=$(readlink -f "/dev/${vg}/${lv}")
  uuid=$(blkid -s UUID -o value "$lvdev")
  if [[ -z "$uuid" ]]; then
    log "no UUID found on $lvdev"
    return 1
  fi

  # Remove stale entries for the temporary and final mount points, but keep
  # comments. The trailing whitespace in the pattern prevents /var from
  # matching /var/lib.
  sed -i "\#^[^#].*[[:space:]]${temp}[[:space:]]#d" /etc/fstab
  sed -i "\#^[^#].*[[:space:]]${final}[[:space:]]#d" /etc/fstab
  echo "UUID=$uuid $final $fs defaults 0 2" >>/etc/fstab
  log "fstab entry: UUID=$uuid $final $fs"
}
prepare_disk "/dev/sdb" "data1_vg" "data1_lv" "/data1" "ext4" 10
sync_data "/var" "/data1" "/lib"
write_fstab "/var" "/data1" "data1_vg" "data1_lv" "ext4"
prepare_disk "/dev/sdc" "data2_vg" "data2_lv" "/data2" "ext4" 30
sync_data "/var/lib" "/data2"
write_fstab "/var/lib" "/data2" "data2_vg" "data2_lv" "ext4"

touch "$MARKER"
sync
log "data disks prepared; vCD reboots the guest after post-customization so the new mounts take effect"
exit 0

DISKSETUP
  chmod 0700 /usr/local/sbin/prepare-data-disks.sh
  log "preparing data disks"
  /usr/local/sbin/prepare-data-disks.sh
}

stage_epoch() {
  # Belt and braces: the instance-id already differs from whatever the OVF
  # datasource recorded at first boot (it carries the spec hash), so cloud-init
  # runs the seed on the next boot even without this. Keeping the wipe means a
  # VM re-created from the same spec also converges.
  if cloud-init clean; then
    log "cloud-init state reset"
  else
    log "cloud-init clean failed (the instance-id still differs)"
  fi
}

log "stage plan for phase $PHASE"
case "$PHASE" in
  preCustomization|precustomization)
    # Only the network and the seed. Both need no egress and cannot block.
    run_stage net  stage_net
    run_stage seed stage_seed
    ;;
  postCustomization|postcustomization)
    # The seed is re-checked here so a VM whose pre phase was skipped still
    # converges; epoch runs last, immediately before vCD's reboot.
    run_stage seed  stage_seed
    run_stage disks stage_disks
    run_stage epoch stage_epoch
    ;;
  *)
    log "unknown phase '$PHASE': running the post-customization stages"
    run_stage seed  stage_seed
    run_stage disks stage_disks
    run_stage epoch stage_epoch
    ;;
esac

log "initscript finished phase=$PHASE; vCD owns the reboot"
exit 0
