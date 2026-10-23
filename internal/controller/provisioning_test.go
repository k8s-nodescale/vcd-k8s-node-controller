package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// backdateProvisioning rewinds the watchdog clock of one worker, so a test can
// stand where a real pool would be ten or twenty minutes later.
func backdateProvisioning(t *testing.T, r *WorkerPoolReconciler, pool *infrav1.K8sWorkerPool, name string, age time.Duration) {
	t.Helper()
	got := getPool(t, r, pool)
	n := nodeByName(got, name)
	if n == nil || n.ProvisioningStartedAt == nil {
		t.Fatalf("worker %s has no provisioning timestamp", name)
	}
	at := metav1.NewTime(time.Now().Add(-age))
	n.ProvisioningStartedAt = &at
	if err := r.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("backdate provisioning: %v", err)
	}
}

// A VM that never registers a Node is parked once spec.provisioning.timeout
// elapses, and the pool names it instead of reporting "still converging". The
// VM itself is left running: a broken guest is evidence, not garbage.
func TestProvisioningWatchdogParksAVMThatNeverRegisters(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provisioning = infrav1.ProvisioningPolicy{Timeout: metav1.Duration{Duration: 15 * time.Minute}}
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.Phase != infrav1.NodePhaseProvisioning {
		t.Fatalf("worker = %#v, want Provisioning", n)
	}
	if n.ProvisioningStartedAt == nil {
		t.Fatal("ProvisioningStartedAt was not stamped on the first pass")
	}

	// Inside the budget the worker keeps waiting.
	backdateProvisioning(t, r, pool, "pool-wrk-01", 10*time.Minute)
	reconcileOnce(t, r, pool)
	if n := nodeByName(getPool(t, r, pool), "pool-wrk-01"); n.Phase != infrav1.NodePhaseProvisioning {
		t.Fatalf("phase = %s at 10m, want Provisioning", n.Phase)
	}

	// Past the budget it is parked, with the reason and the VM kept.
	backdateProvisioning(t, r, pool, "pool-wrk-01", 16*time.Minute)
	reconcileOnce(t, r, pool)

	got = getPool(t, r, pool)
	n = nodeByName(got, "pool-wrk-01")
	if n == nil || n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("worker = %#v, want Failed after the timeout", n)
	}
	if n.Message == "" {
		t.Fatal("message is empty, want the watchdog reason")
	}
	if n.ProviderID == "" {
		t.Fatal("providerID was cleared, want the VM kept")
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("deleted %v, the watchdog must not touch the VM", fc.deleted)
	}
	if cond := metaReady(got); cond == nil || cond.Reason != "WorkerFailed" {
		t.Fatalf("Ready condition = %#v, want WorkerFailed", cond)
	}

	// Later passes keep the verdict instead of flapping back to Provisioning.
	reconcileOnce(t, r, pool)
	if n := nodeByName(getPool(t, r, pool), "pool-wrk-01"); n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("phase = %s on a later pass, want Failed", n.Phase)
	}
}

// The watchdog reports, it does not condemn: a registration that arrives after
// the timeout still promotes the worker to Ready and clears the clock.
func TestProvisioningWatchdogRecoversWhenTheNodeArrivesLate(t *testing.T) {
	pool := testPool(1)
	pool.Spec.Provisioning = infrav1.ProvisioningPolicy{Timeout: metav1.Duration{Duration: 15 * time.Minute}}
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	backdateProvisioning(t, r, pool, "pool-wrk-01", 20*time.Minute)
	reconcileOnce(t, r, pool)
	if n := nodeByName(getPool(t, r, pool), "pool-wrk-01"); n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("phase = %s, want Failed", n.Phase)
	}

	fk.nodes = readyNodes("pool-wrk-01")
	reconcileOnce(t, r, pool)

	got := getPool(t, r, pool)
	n := nodeByName(got, "pool-wrk-01")
	if n == nil || n.Phase != infrav1.NodePhaseReady || !n.K8sReady {
		t.Fatalf("worker = %#v, want Ready after the late registration", n)
	}
	if n.ProvisioningStartedAt != nil {
		t.Fatalf("provisioningStartedAt = %v, want it cleared", n.ProvisioningStartedAt)
	}
	if cond := metaReady(got); cond == nil || cond.Reason != "Converged" {
		t.Fatalf("Ready condition = %#v, want Converged", cond)
	}
}

// An object written before spec.provisioning existed still gets a watchdog:
// the zero value falls back to the documented 15m.
func TestProvisioningWatchdogFallsBackToTheDefaultTimeout(t *testing.T) {
	pool := testPool(1)
	fc := &fakeCloud{vms: []cloud.VM{workerVM(1, "192.168.100.7")}}
	fk := &fakeKube{}
	r := newTestReconciler(t, fc, fk, pool)

	reconcileOnce(t, r, pool)
	backdateProvisioning(t, r, pool, "pool-wrk-01", 16*time.Minute)
	reconcileOnce(t, r, pool)

	if n := nodeByName(getPool(t, r, pool), "pool-wrk-01"); n.Phase != infrav1.NodePhaseFailed {
		t.Fatalf("phase = %s, want Failed from the 15m default", n.Phase)
	}
}
