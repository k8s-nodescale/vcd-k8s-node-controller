#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.6.1 hash=28cfcc56b1fa
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
log "initscript phase=$PHASE render=0.6.1 hash=28cfcc56b1fa"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="9b26d43d"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.6.1 hash=28cfcc56b1fa" >>"$STAGE_DIR/state"
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
