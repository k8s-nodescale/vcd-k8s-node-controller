#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.3.1 hash=4658f4e53764
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
log "initscript phase=$PHASE render=0.3.1 hash=4658f4e53764"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.3.1 hash=4658f4e53764" >>"$STAGE_DIR/state"
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
LOG=/var/log/vcd-data-disks.log
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

  if mountpoint -q "$src"; then
    log "$src is already a mountpoint; skipping data copy"
    return 0
  fi
  if [[ ! -d "$src" ]]; then
    log "source $src does not exist; skipping copy"
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
