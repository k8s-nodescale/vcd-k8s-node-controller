#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.4.1 hash=807bf42a96e8
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
log "initscript phase=$PHASE render=0.4.1 hash=807bf42a96e8"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="3498da35"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.4.1 hash=807bf42a96e8" >>"$STAGE_DIR/state"
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
