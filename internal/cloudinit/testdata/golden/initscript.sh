#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.6.0 hash=fa62d1fbd33d
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
log "initscript phase=$PHASE render=0.6.0 hash=fa62d1fbd33d"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="9b26d43d"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.6.0 hash=fa62d1fbd33d" >>"$STAGE_DIR/state"
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
