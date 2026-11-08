#!/usr/bin/env bash
# Renders the chart for every platform and checks the isolation the `platform`
# value exists for:
#
#   - the value is mandatory: rendering without it fails and says so;
#   - an unknown value fails instead of silently picking a side;
#   - a vcd release carries VCD_* and no VSPHERE_* at all;
#   - a vsphere release carries VSPHERE_* and no VCD_* at all;
#   - the namespace is the platform-neutral nodescale-system;
#   - the chart's CRD is identical to the generated one.
#
# Run from the repository root. helm 3.9 defaults to Kubernetes 1.24, which the
# chart's kubeVersion rejects, so the target version is always explicit.
set -euo pipefail

chart="charts/vcd-nodescale"
kube_version="${KUBE_VERSION:-1.28.15}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
	echo "chart-matrix: $*" >&2
	exit 1
}

render() {
	helm template release "$chart" --kube-version "$kube_version" "$@"
}

# 1. platform is mandatory.
if render >"$tmp/out" 2>&1; then
	fail "the chart rendered without platform"
fi
grep -q "platform is required" "$tmp/out" ||
	fail "the missing-platform error does not name platform"

# 2. an unknown platform is refused by name.
if render --set platform=openstack >"$tmp/out" 2>&1; then
	fail "the chart rendered platform=openstack"
fi
grep -q 'platform "openstack" is not supported' "$tmp/out" ||
	fail "an unknown platform was not rejected by name"

# 3. vcd: its credentials and nothing from the other platform.
render --set platform=vcd >"$tmp/vcd.yaml"
grep -q 'value: "vcd"' "$tmp/vcd.yaml" ||
	fail "the vcd release does not set NODESCALE_PLATFORM=vcd"
grep -q 'name: VCD_URL' "$tmp/vcd.yaml" ||
	fail "the vcd release has no VCD_URL"
grep -q 'name: vcd-credentials' "$tmp/vcd.yaml" ||
	fail "the vcd release does not reference vcd-credentials"
if grep -q 'name: VSPHERE_' "$tmp/vcd.yaml"; then
	fail "the vcd release carries VSPHERE_* variables"
fi
if grep -q 'name: vsphere-credentials' "$tmp/vcd.yaml"; then
	fail "the vcd release references the vsphere Secret"
fi

# 4. vsphere: its credentials and nothing from the other platform.
render --set platform=vsphere \
	--set vsphere.folderRoot="/HUB Infrastructure/dev/k8s-test-lab" \
	>"$tmp/vsphere.yaml"
grep -q 'value: "vsphere"' "$tmp/vsphere.yaml" ||
	fail "the vsphere release does not set NODESCALE_PLATFORM=vsphere"
grep -q 'name: VSPHERE_URL' "$tmp/vsphere.yaml" ||
	fail "the vsphere release has no VSPHERE_URL"
grep -q 'name: vsphere-credentials' "$tmp/vsphere.yaml" ||
	fail "the vsphere release does not reference vsphere-credentials"
grep -q 'name: VSPHERE_FOLDER_ROOT' "$tmp/vsphere.yaml" ||
	fail "the vsphere release does not render the folder root"
if grep -q 'name: VCD_' "$tmp/vsphere.yaml"; then
	fail "the vsphere release carries VCD_* variables"
fi
if grep -q 'name: vcd-credentials' "$tmp/vsphere.yaml"; then
	fail "the vsphere release references the vcd Secret"
fi

# 5. the namespace is the product's, not the platform's.
for f in "$tmp/vcd.yaml" "$tmp/vsphere.yaml"; do
	grep -q 'namespace: nodescale-system' "$f" ||
		fail "$(basename "$f") does not use nodescale-system"
	if grep -q 'vcd-nodescale-system' "$f"; then
		fail "$(basename "$f") still uses the old vcd-prefixed namespace"
	fi
done

# 6. the packaged CRD is the generated one: a chart that ships a stale CRD
# prunes fields the controller writes.
if ! diff -q "config/crd/bases/k8s-nodescale.github.io_k8sworkerpools.yaml" \
	"$chart/crds/k8s-nodescale.github.io_k8sworkerpools.yaml" >/dev/null; then
	fail "the chart CRD differs from config/crd/bases; run make manifests and copy it"
fi

echo "chart-matrix: vcd and vsphere both render only their own credentials"
