package controller

import (
	"context"
	"strings"
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// A vsphere pool has to name its placement. Flipping the provider without the
// block is rejected before anything is created, the way a vcd pool without
// spec.vcd is.
func TestReconcileRejectsAVSpherePoolWithoutTheBlock(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.Provider = "vsphere"
	pool.Spec.VCD = nil
	fc := &fakeCloud{}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs for a pool without a placement", len(fc.created))
	}
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v, want InvalidSpec", cond)
	}
	if !strings.Contains(cond.Message, "spec.vsphere is required") {
		t.Fatalf("message does not name the missing block: %q", cond.Message)
	}
}

func TestReconcileRejectsAnUnknownProvider(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.Provider = "openstack"
	r := newTestReconciler(t, &fakeCloud{}, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v, want InvalidSpec", cond)
	}
}

// placementFor is the whole mapping from the CR to the platform contract: an
// empty provider means vcd, the matching block is carried over, and the guest
// network of a vsphere pool is derived from the same spec the renderer uses.
func TestPlacementForMapsThePool(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provider = ""
	pl := placementFor(pool)
	if pl.Provider != cloud.ProviderVCD {
		t.Fatalf("provider = %q, want vcd", pl.Provider)
	}
	if pl.VCD == nil || pl.VCD.VApp != "vapp" || pl.VCD.OrgNetwork != "net" {
		t.Fatalf("placement = %#v, want the pool vCD block", pl.VCD)
	}

	pool.Spec.Provider = cloud.ProviderVSphere
	pool.Spec.VCD = nil
	pool.Spec.VSphere = &infrav1.VSphereRef{
		Cluster:   "mskdev",
		Folder:    "/HUB Infrastructure/dev/k8s-test-lab",
		Datastore: "99DOR13_DEV_NFS_L11",
		Network:   "Avantage_VL_DEV_MGMT_CNO-330",
	}
	pl = placementFor(pool)
	if pl.Provider != cloud.ProviderVSphere || pl.VCD != nil || pl.VSphere == nil {
		t.Fatalf("placement = %#v, want vsphere with no vCD block", pl)
	}
	v := pl.VSphere
	if v.Cluster != "mskdev" || v.Datastore != "99DOR13_DEV_NFS_L11" || v.Network != "Avantage_VL_DEV_MGMT_CNO-330" {
		t.Fatalf("placement = %#v, want the pool block", v)
	}
	// testPool uses 192.168.100.0/24 with ens192 and one resolver.
	if v.Gateway != "192.168.100.1" || v.PrefixLen != 24 {
		t.Fatalf("guest network = %s/%d, want 192.168.100.1/24", v.Gateway, v.PrefixLen)
	}
	if v.NICName != "ens192" || len(v.DNSServers) != 1 || v.DNSServers[0] != "192.0.2.53" {
		t.Fatalf("guest network = %#v, want the bootstrap interface and resolver", v)
	}
}

// The folder root is the boundary vCenter grants the account. The provider
// checks the resolved inventory path; validateSpec reports it before the first
// call, and accepts the path written with or without the datacenter prefix.
func TestValidateSpecChecksTheVSphereFolderRoot(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provider = cloud.ProviderVSphere
	pool.Spec.VCD = nil
	pool.Spec.VSphere = &infrav1.VSphereRef{
		Cluster:   "mskdev",
		Folder:    "/OTHER/dev/lab",
		Datastore: "ds",
		Network:   "pg",
	}
	r := &WorkerPoolReconciler{
		VSphereDatacenter: "MSK_Dev_DC",
		VSphereFolderRoot: "/HUB Infrastructure/dev/k8s-test-lab",
	}
	if msg := r.validateSpec(pool); !strings.Contains(msg, "outside the allowed root") {
		t.Fatalf("msg = %q, want the root check", msg)
	}
	for _, folder := range []string{
		"/HUB Infrastructure/dev/k8s-test-lab",
		"/HUB Infrastructure/dev/k8s-test-lab/pool-a",
		"vm/HUB Infrastructure/dev/k8s-test-lab/pool-a",
		"/MSK_Dev_DC/vm/HUB Infrastructure/dev/k8s-test-lab/pool-a",
	} {
		pool.Spec.VSphere.Folder = folder
		if msg := r.validateSpec(pool); msg != "" {
			t.Fatalf("folderspec %q was rejected: %s", folder, msg)
		}
	}
}
