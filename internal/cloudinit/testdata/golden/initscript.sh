#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.3.0 hash=03d81f2e029b
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
log "initscript phase=$PHASE render=0.3.0 hash=03d81f2e029b"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.3.0 hash=03d81f2e029b" >>"$STAGE_DIR/state"
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
