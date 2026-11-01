#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.4.0 hash=0ea72d37ece9
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
log "initscript phase=$PHASE render=0.4.0 hash=0ea72d37ece9"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.4.0 hash=0ea72d37ece9" >>"$STAGE_DIR/state"
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
