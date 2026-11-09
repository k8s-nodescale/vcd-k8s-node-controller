#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.5.0 hash=e33bfdbabffc
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
log "initscript phase=$PHASE render=0.5.0 hash=e33bfdbabffc"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="9b26d43d"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.5.0 hash=e33bfdbabffc" >>"$STAGE_DIR/state"
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
