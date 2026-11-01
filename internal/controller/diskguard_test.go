package controller

import (
	"context"
	"strings"
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// A disk grown by hand in vCD must not be "repaired" by replacing the worker:
// the pool reports the difference and waits for a human. The vCPU difference in
// this fixture would normally take the worker into a rollout, which is exactly
// what must not happen while a disk is larger than the spec.
func TestDiskLargerThanSpecIsGuardedNotRolled(t *testing.T) {
	ctx := context.Background()
	pool, fc, fk := rolloutPool(t)
	pool.Spec.Flavor.Disks = []infrav1.DiskSpec{{BusNumber: 1, UnitNumber: 1, Mount: "/var", SizeGB: 10}}
	fc.vms[0].DataDisks = []cloud.DiskState{{BusNumber: 1, UnitNumber: 1, SizeGB: 40}}

	r := newTestReconciler(t, fc, fk, pool)
	// First pass adopts the VM and records its shape.
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(fk.cordoned) != 0 {
		t.Fatalf("cordoned %v: the worker was taken out of service for a disk that is larger than the spec", fk.cordoned)
	}
	if len(fc.resizes) != 0 {
		t.Fatalf("resized %v: nothing may be resized while a disk is larger than the spec", fc.resizes)
	}
	got := getPool(t, r, pool)
	cond := metaReady(got)
	if cond == nil || cond.Reason != "DiskLargerThanSpec" {
		t.Fatalf("condition = %#v, want Ready=False/DiskLargerThanSpec", cond)
	}
	if !strings.Contains(cond.Message, "/var") || !strings.Contains(cond.Message, "40 GB") {
		t.Fatalf("message = %q, want it to name the mount and the size", cond.Message)
	}
	if !strings.Contains(cond.Message, "not recreated automatically") {
		t.Fatalf("message = %q, want the hint about the spec", cond.Message)
	}
}
