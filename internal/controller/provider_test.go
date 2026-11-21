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
	r.Platform = cloud.ProviderVSphere

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

// A pool that names the other platform is refused by name: the factory for it
// is not even registered, and "no cloud provider named ..." would be a worse
// explanation than "this installation runs the other platform".
func TestReconcileRejectsAPoolOfTheOtherPlatform(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.Provider = "vsphere"
	fc := &fakeCloud{}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)
	// The installation is vcd, which is also the default when Platform is
	// empty; the pool asks for vsphere and carries a vsphere block.
	pool.Spec.VSphere = &infrav1.VSphereRef{
		Datacenter: "MSK_Dev_DC",
		Cluster:    "mskdev",
		Folder:     "/lab",
		Datastore:  "ds",
		Network:    "pg",
	}

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs for a pool of the other platform", len(fc.created))
	}
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v, want InvalidSpec", cond)
	}
	if !strings.Contains(cond.Message, "does not match the platform of this installation (vcd)") {
		t.Fatalf("message does not explain the mismatch: %q", cond.Message)
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
// empty provider means the installation's platform, the matching block is
// carried over, and the guest network of a vsphere pool is derived from the
// same spec the renderer uses.
func TestPlacementForMapsThePool(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provider = ""
	pl := placementFor(pool, cloud.ProviderVCD)
	if pl.Provider != cloud.ProviderVCD {
		t.Fatalf("provider = %q, want vcd", pl.Provider)
	}
	if pl.VCD == nil || pl.VCD.VApp != "vapp" || pl.VCD.OrgNetwork != "net" {
		t.Fatalf("placement = %#v, want the pool vCD block", pl.VCD)
	}

	pool.Spec.Provider = cloud.ProviderVSphere
	pool.Spec.VCD = nil
	pool.Spec.VSphere = &infrav1.VSphereRef{
		Datacenter: "MSK_Dev_DC",
		Cluster:    "mskdev",
		Folder:     "/HUB Infrastructure/dev/k8s-test-lab",
		Datastore:  "99DOR13_DEV_NFS_L11",
		Network:    "Avantage_VL_DEV_MGMT_CNO-330",
	}
	pl = placementFor(pool, cloud.ProviderVSphere)
	if pl.Provider != cloud.ProviderVSphere || pl.VCD != nil || pl.VSphere == nil {
		t.Fatalf("placement = %#v, want vsphere with no vCD block", pl)
	}
	v := pl.VSphere
	if v.Datacenter != "MSK_Dev_DC" || v.Cluster != "mskdev" || v.Datastore != "99DOR13_DEV_NFS_L11" || v.Network != "Avantage_VL_DEV_MGMT_CNO-330" {
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
		Datacenter: "MSK_Dev_DC",
		Cluster:    "mskdev",
		Folder:     "/OTHER/dev/lab",
		Datastore:  "ds",
		Network:    "pg",
	}
	r := &WorkerPoolReconciler{
		Platform:          cloud.ProviderVSphere,
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

// The pool's scope holds more than its workers: masters, load balancers, the
// deployment VM. Their addresses must never be handed to a worker, and the
// ledger must not claim them.
func TestForeignAddressesExcludeThePoolsOwnVMs(t *testing.T) {
	owned := []cloud.VM{
		{Name: "lab-wrk-01", IP: "10.99.96.109"},
		{Name: "lab-wrk-02", IP: "10.99.96.110"},
	}
	all := []cloud.VM{
		{Name: "lab-wrk-01", IP: "10.99.96.109"},
		{Name: "lab-wrk-02", IP: "10.99.96.110"},
		{Name: "lab-mst-01", IP: "10.99.96.106"},
		{Name: "lab-k8s-lb-01", IP: "10.99.96.104"},
		{Name: "lab-lb-02", IP: ""}, // powered off: no address to block
		{Name: "dvm", IP: "10.99.96.101"},
	}
	got := foreignAddresses(owned, all)
	want := map[string]bool{"10.99.96.106": true, "10.99.96.104": true, "10.99.96.101": true}
	if len(got) != len(want) {
		t.Fatalf("foreign addresses = %v, want %d entries", got, len(want))
	}
	for _, ip := range got {
		if !want[ip] {
			t.Fatalf("foreign addresses = %v, want %v", got, want)
		}
	}
}
