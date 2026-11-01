package controller

import (
	"context"
	"testing"

	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// A removal the guard refuses must not stop the rest of the pass. This is the
// live case from 2026-11-01: a worker was replaced (so the pool was one short),
// the guard then refused the next replacement because of a node-local volume,
// and the refused request starved the scale-up - the pool sat at two workers
// with replicas: three until the annotation was removed by hand.
func TestRefusedRemovalDoesNotStarveScaleUp(t *testing.T) {
	ctx := context.Background()
	pool := testPool(3)
	// The floor is the whole pool, so the removal stays refused even after the
	// scale-up brings the third worker back: the point is that the refusal does
	// not block that scale-up in the first place.
	pool.Spec.ScaleDown.MinReplicas = 3
	pool.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-02"}

	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096, OSDiskGB: 20, DiskGB: 30, PoweredOn: true},
		{ID: "vm-2", Name: "pool-wrk-02", IP: "192.168.100.8", CPU: 2, MemoryMB: 4096, OSDiskGB: 20, DiskGB: 30, PoweredOn: true},
	}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	// One pass adopts the two VMs, the next refuses the deletion and must still
	// create the third worker.
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	got := getPool(t, r, pool)
	if got.Status.ObservedReplicas < 3 {
		t.Fatalf("observed %d workers, want 3: a refused deletion starved the scale-up", got.Status.ObservedReplicas)
	}
	cond := metaReady(got)
	if cond == nil || cond.Reason != "MinReplicasProtected" {
		t.Fatalf("condition = %#v, want the refusal to stay visible", cond)
	}
	if len(fk.cordoned) != 0 {
		t.Fatalf("cordoned %v: nothing may be taken out of service", fk.cordoned)
	}
}

// The guard's message survives the readiness evaluation in the same pass.
func TestRefusalKeepsTheConditionOnAConvergedPool(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	pool.Spec.ScaleDown.MinReplicas = 1
	pool.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-01"}

	fc := &fakeCloud{vms: []cloud.VM{
		{ID: "vm-1", Name: "pool-wrk-01", IP: "192.168.100.7", CPU: 2, MemoryMB: 4096, OSDiskGB: 20, DiskGB: 30, PoweredOn: true},
	}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	got := getPool(t, r, pool)
	cond := metaReady(got)
	if cond == nil || cond.Status != "False" || cond.Reason != "MinReplicasProtected" {
		t.Fatalf("condition = %#v, want Ready=False/MinReplicasProtected", cond)
	}
	if got.Status.ObservedReplicas != 1 {
		t.Fatalf("observed %d, want the worker untouched", got.Status.ObservedReplicas)
	}
}
