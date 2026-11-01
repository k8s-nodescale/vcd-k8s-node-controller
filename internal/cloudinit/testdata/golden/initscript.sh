#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.4.1 hash=e46f7a26d589
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
log "initscript phase=$PHASE render=0.4.1 hash=e46f7a26d589"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.4.1 hash=e46f7a26d589" >>"$STAGE_DIR/state"
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
