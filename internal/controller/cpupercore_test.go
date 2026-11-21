package controller

import (
	"context"
	"strings"
	"testing"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
)

// cpuZone is a cluster whose free CPU is short of a 1:1 charge for one worker
// and long enough at 2:1. testPool asks for 2 vCPU, so 1:1 is 5200 MHz.
func cpuZone(physical bool, freeMHz int64) cloud.PlatformCapacity {
	return cloud.PlatformCapacity{
		Physical:      physical,
		CPUMHzPerCore: 2600,
		CPU:           cloud.ResourceCapacity{Limit: freeMHz, Used: 0},
		MemoryMB:      cloud.ResourceCapacity{Limit: 1 << 20, Used: 0},
	}
}

// The pool's assumption is the owner's choice, and the pre-flight honours it:
// the same cluster refuses a worker at 1:1 and admits it at 2:1.
func TestScaleUpHonoursCPUPerCore(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	fc := &fakeCloud{capacity: cpuZone(true, 4000)}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs at 1 vCPU : 1 core, want 0", len(fc.created))
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].Phase != infrav1.NodePhaseQueued {
		t.Fatalf("want the slot queued at 1:1, got %#v", got.Status.Nodes)
	}

	perCore := int32(2)
	stored := getPool(t, r, pool)
	stored.Spec.Capacity.CPUPerCore = &perCore
	if err := r.Update(ctx, stored); err != nil {
		t.Fatalf("write the assumption: %v", err)
	}
	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile at 2:1: %v", err)
	}
	if len(fc.created) != 1 {
		t.Fatalf("created %d VMs at 2 vCPU : 1 core, want 1", len(fc.created))
	}
}

// A quota is what the platform will actually grant. Dividing it would admit a
// request the platform then refuses, so the assumption must not apply there.
func TestCPUPerCoreDoesNotApplyToAQuota(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	perCore := int32(4)
	pool.Spec.Capacity.CPUPerCore = &perCore
	fc := &fakeCloud{capacity: cpuZone(false, 4000)}
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created %d VMs against a quota, want 0", len(fc.created))
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 1 || got.Status.Nodes[0].Phase != infrav1.NodePhaseQueued {
		t.Fatalf("want the slot queued, got %#v", got.Status.Nodes)
	}
}

// The shortfall has to name the assumption: "1300 MHz" alone reads like a core
// speed that does not exist.
func TestQueuedMessageNamesTheAssumption(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	perCore := int32(2)
	pool.Spec.Capacity.CPUPerCore = &perCore
	fc := &fakeCloud{capacity: cpuZone(true, 100)} // nothing fits
	r := newTestReconciler(t, fc, &fakeKube{}, pool)

	if _, err := r.Reconcile(ctx, requestFor(pool)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getPool(t, r, pool)
	if len(got.Status.Nodes) != 1 {
		t.Fatalf("want one slot, got %#v", got.Status.Nodes)
	}
	if msg := got.Status.Nodes[0].Message; !strings.Contains(msg, "2 vCPU : 1 core") {
		t.Fatalf("message = %q, want it to name the assumption", msg)
	}
}
