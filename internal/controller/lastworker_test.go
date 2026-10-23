package controller

import (
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// A slot whose VM is gone is written off without a drain, but the write-off is
// still a removal: it may not empty the pool behind the allowLastWorker policy.
func TestReconcileMissingLastWorkerNeedsTheOptIn(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 1}
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	fc.vms = nil // the VM disappears outside the controller
	reconcileOnce(t, r, pool)

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-01"}
	})
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if cond := metaReady(got); cond == nil || cond.Reason != "LastWorkerProtected" {
		t.Fatalf("Ready condition = %#v, want LastWorkerProtected", cond)
	}
	if nodeByName(got, "pool-wrk-01") == nil {
		t.Fatal("the missing slot was written off despite the refusal")
	}
	if len(got.Status.Ledger.Used) != 1 {
		t.Fatalf("ledger used = %v, want the address to stay reserved", got.Status.Ledger.Used)
	}
	if len(got.Spec.ScaleDown.DeleteWorkers) != 1 {
		t.Fatalf("deleteWorkers = %v, want the request to stay pending", got.Spec.ScaleDown.DeleteWorkers)
	}

	// The explicit opt-in still allows the cleanup.
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.MinReplicas = 0
		p.Spec.ScaleDown.AllowLastWorker = true
	})
	reconcileOnce(t, r, pool)

	got = getPool(t, r, pool)
	if len(got.Status.Nodes) != 0 || got.Spec.Replicas != 0 {
		t.Fatalf("nodes=%d replicas=%d, want an empty pool", len(got.Status.Nodes), got.Spec.Replicas)
	}
	if len(got.Status.Ledger.Used) != 0 {
		t.Fatalf("ledger used = %v, want the address released", got.Status.Ledger.Used)
	}
}

// The floor counts workers able to serve, not status rows: one Ready and one
// Missing slot is not a two-worker pool, so removing the Ready one is refused.
func TestReconcileMinReplicasCountsServingWorkers(t *testing.T) {
	pool := testPool(2)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 1}
	fc := &fakeCloud{vms: []cloud.VM{
		workerVM(1, "192.168.100.7"),
		workerVM(2, "192.168.100.8"),
	}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01", "pool-wrk-02")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	fc.vms = fc.vms[:1] // pool-wrk-02 is gone
	reconcileOnce(t, r, pool)

	// Now ask for the only serving worker to be removed.
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-01"}
	})
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if cond := metaReady(got); cond == nil || cond.Reason != "MinReplicasProtected" {
		t.Fatalf("Ready condition = %#v, want MinReplicasProtected", cond)
	}
	if nodeByName(got, "pool-wrk-01") == nil || len(fc.deleted) != 0 {
		t.Fatalf("the only serving worker was removed (deleted=%v)", fc.deleted)
	}

	// Emptying the pool is explicitly allowed, so the same request completes.
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.MinReplicas = 0
		p.Spec.ScaleDown.AllowLastWorker = true
	})
	reconcilePasses(t, r, pool, 6)
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-01" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-01]", fc.deleted)
	}
}

// A removal that had already cordoned the worker must not stay stuck when the
// policy changes under it: the cordon is reverted and the worker serves again.
func TestReconcileRefusedMidDrainRevertsTheCordon(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 0, AllowLastWorker: true}
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool) // adopted, ready
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.DeleteWorkers = []string{"pool-wrk-01"}
	})
	reconcileOnce(t, r, pool) // cordoned, draining
	if len(fk.cordoned) != 1 {
		t.Fatalf("cordoned = %v, want the removal to have started", fk.cordoned)
	}

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) {
		p.Spec.ScaleDown.AllowLastWorker = false
	})
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	node := nodeByName(got, "pool-wrk-01")
	if node == nil {
		t.Fatal("the worker disappeared")
	}
	if node.Phase != infrav1.NodePhaseReady || node.DeleteStartedAt != nil {
		t.Fatalf("phase=%q deleteStartedAt=%v, want Ready and no removal state", node.Phase, node.DeleteStartedAt)
	}
	if len(fk.uncordoned) != 1 || fk.uncordoned[0] != "pool-wrk-01" {
		t.Fatalf("uncordoned = %v, want [pool-wrk-01]", fk.uncordoned)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing", fc.deleted)
	}
	if cond := metaReady(got); cond == nil || cond.Reason != "LastWorkerProtected" {
		t.Fatalf("Ready condition = %#v, want LastWorkerProtected", cond)
	}

	// The refusal is stable: the next pass does not cordon again.
	reconcileOnce(t, r, pool)
	if len(fk.cordoned) != 1 {
		t.Fatalf("cordoned = %v, want no second cordon", fk.cordoned)
	}
}

// allowLastWorker with a floor is contradictory: the flag could never be
// honoured, so the spec is rejected instead of silently refusing removals.
func TestValidateSpecRejectsAllowLastWorkerWithFloor(t *testing.T) {
	pool := testPool(1)
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 1, AllowLastWorker: true}
	fc := &fakeCloud{}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	if cond := metaReady(got); cond == nil || cond.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v, want InvalidSpec", cond)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created = %v, want nothing", fc.created)
	}
}
