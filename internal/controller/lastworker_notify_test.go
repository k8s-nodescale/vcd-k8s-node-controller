package controller

import (
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// notifyingPool is a one-worker pool that may be emptied on request.
func notifyingPool() *infrav1.K8sWorkerPool {
	pool := testPool(1)
	pool.Spec.Reconciliation = infrav1.ReconciliationPolicy{Policy: infrav1.ReconciliationNotify}
	pool.Spec.ScaleDown = infrav1.ScaleDownPolicy{MinReplicas: 0, AllowLastWorker: true}
	return pool
}

// Emptying a one-worker pool with AutoHeal disabled must stay empty. Notify
// creates nothing for a slot that is gone, so the freed address and the
// removed name must not come back as a "healed" worker.
func TestNotifyScaleToZeroKeepsThePoolEmpty(t *testing.T) {
	pool := notifyingPool()
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool) // adopt the existing worker
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 0 })
	reconcilePasses(t, r, pool, 6)

	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 0 || got.Status.ObservedReplicas != 0 || got.Spec.Replicas != 0 {
		t.Fatalf("nodes=%d observed=%d replicas=%d, want an empty pool",
			len(got.Status.Nodes), got.Status.ObservedReplicas, got.Spec.Replicas)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "vm-pool-wrk-01" {
		t.Fatalf("deleted VMs = %v, want [vm-pool-wrk-01]", fc.deleted)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created = %v, want nothing created", fc.created)
	}
	if len(got.Status.Ledger.Used) != 0 {
		t.Fatalf("ledger used = %v, want the address released", got.Status.Ledger.Used)
	}
	if len(got.Status.Ledger.Free) != 1 || got.Status.Ledger.Free[0] != "192.168.100.7" {
		t.Fatalf("ledger free = %v, want [192.168.100.7]", got.Status.Ledger.Free)
	}
	if cond := metaReady(got); cond == nil || string(cond.Status) != "True" {
		t.Fatalf("Ready condition = %#v, want True for a converged empty pool", cond)
	}

	// Later passes must not resurrect the worker either.
	reconcilePasses(t, r, pool, 3)
	got = getPool(t, r, pool)
	if len(got.Status.Nodes) != 0 || len(fc.created) != 0 {
		t.Fatalf("nodes=%d created=%v, want the pool to stay empty", len(got.Status.Nodes), fc.created)
	}
}

// The other half of the same scenario: an empty pool has to come back up. The
// freed address is reused first, so the worker returns under the name its
// address gives it (wrk-01) instead of a fresh ordinal.
func TestNotifyScaleUpAfterEmptyingThePool(t *testing.T) {
	pool := notifyingPool()
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{nodes: readyNodes("pool-wrk-01")}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 0 })
	reconcilePasses(t, r, pool, 6)
	if got := getPool(t, r, pool); len(got.Status.Nodes) != 0 {
		t.Fatalf("nodes = %d, want an empty pool before scaling back up", len(got.Status.Nodes))
	}

	updateSpec(t, r, pool, func(p *infrav1.K8sWorkerPool) { p.Spec.Replicas = 2 })
	reconcilePasses(t, r, pool, 8)

	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(got.Status.Nodes))
	}
	if len(fc.created) != 2 {
		t.Fatalf("created = %d VMs, want 2", len(fc.created))
	}
	if fc.created[0].Name != "pool-wrk-01" || fc.created[0].IP != "192.168.100.7" {
		t.Fatalf("first created = %s/%s, want pool-wrk-01/192.168.100.7", fc.created[0].Name, fc.created[0].IP)
	}
	if fc.created[1].Name != "pool-wrk-02" || fc.created[1].IP != "192.168.100.8" {
		t.Fatalf("second created = %s/%s, want pool-wrk-02/192.168.100.8", fc.created[1].Name, fc.created[1].IP)
	}

	// Once kubelet registers both Nodes the pool converges to Ready.
	fk.nodes = readyNodes("pool-wrk-01", "pool-wrk-02")
	reconcilePasses(t, r, pool, 3)

	got = getPool(t, r, pool)
	if got.Status.ObservedReplicas != 2 {
		t.Fatalf("observed = %d, want 2", got.Status.ObservedReplicas)
	}
	if cond := metaReady(got); cond == nil || string(cond.Status) != "True" {
		t.Fatalf("Ready condition = %#v, want True", cond)
	}
	if len(got.Status.Ledger.Used) != 2 {
		t.Fatalf("ledger used = %v, want two addresses", got.Status.Ledger.Used)
	}
}
