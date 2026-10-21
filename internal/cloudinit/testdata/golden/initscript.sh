#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.2.0 hash=c9a32a99f590
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
log "initscript phase=$PHASE render=0.2.0 hash=c9a32a99f590"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.2.0 hash=c9a32a99f590" >>"$STAGE_DIR/state"
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
