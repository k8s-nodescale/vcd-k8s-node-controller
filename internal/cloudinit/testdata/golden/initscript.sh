#!/usr/bin/env bash
# vCD guest-customization initscript: staged dispatcher.
# render=0.7.0 hash=eeae8634f9f6
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
log "initscript phase=$PHASE render=0.7.0 hash=eeae8634f9f6"

NIC="ens192"
NODE_NAME="pool-wrk-01"
NODE_IP="192.0.2.10"
CIDR_PREFIX="24"
GATEWAY="192.0.2.1"
DNS_JSON='["192.0.2.53"]'
SPEC_HASH="4df6500e"

record() { # stage rc
  printf '%s stage=%s rc=%s phase=%s %s\n' \
    "$(date -Is)" "$1" "$2" "$PHASE" "render=0.7.0 hash=eeae8634f9f6" >>"$STAGE_DIR/state"
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
