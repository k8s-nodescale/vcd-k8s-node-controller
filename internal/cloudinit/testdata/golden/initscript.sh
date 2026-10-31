#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.3.2 hash=732099503917
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
log "initscript phase=$PHASE render=0.3.2 hash=732099503917"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.3.2 hash=732099503917" >>"$STAGE_DIR/state"
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
