package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// Roadmap 10: the platform is chosen per pool, and a platform that is only a
// stub says so before anything is created.
func TestReconcileRejectsTheVSphereStub(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.Provider = "vsphere"
	fc := &fakeCloud{}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs for a stub provider", len(fc.created))
	}
	cond := metaReady(getPool(t, r, pool))
	if cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v, want InvalidSpec", cond)
	}
	if !strings.Contains(cond.Message, "not implemented") {
		t.Fatalf("message does not say the provider is a stub: %q", cond.Message)
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
// empty provider means vcd, and the vCD block is carried over as-is.
func TestPlacementForMapsThePool(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provider = ""
	pl := placementFor(pool)
	if pl.Provider != cloud.ProviderVCD {
		t.Fatalf("provider = %q, want vcd", pl.Provider)
	}
	if pl.VCD == nil || pl.VCD.VApp != "vapp" || pl.VCD.OrgNetwork != "net" {
		t.Fatalf("placement = %#v, want the pool's vCD block", pl.VCD)
	}

	pool.Spec.Provider = cloud.ProviderVSphere
	pool.Spec.VCD = nil
	pl = placementFor(pool)
	if pl.Provider != cloud.ProviderVSphere || pl.VCD != nil {
		t.Fatalf("placement = %#v, want vsphere with no vCD block", pl)
	}
}
